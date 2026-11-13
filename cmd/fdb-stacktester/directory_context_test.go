//go:build bazelrunfiles

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// Withhold value replies to prove a real directory read without blocking scans or commits.
// Framing follows the real-client hold dialer in fdb/ownership_async_test.go.
type directoryReplyGate struct {
	mu      sync.Mutex
	armed   bool
	held    []directoryHeldReply
	reached chan struct{}
}

type directoryHeldReply struct {
	conn  *directoryGateConn
	token transport.UID
	body  []byte
}

type directoryGateConn struct {
	net.Conn
	gate      *directoryReplyGate
	reader    *io.PipeReader
	writer    *io.PipeWriter
	forwardMu sync.Mutex
}

func (g *directoryReplyGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	r, w := io.Pipe()
	c := &directoryGateConn{Conn: raw, gate: g, reader: r, writer: w}
	go c.readLoop()
	return c, nil
}

func (g *directoryReplyGate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.reached = make(chan struct{}, 1)
}

func (g *directoryReplyGate) release() {
	g.mu.Lock()
	held := g.held
	g.held = nil
	g.armed = false
	g.mu.Unlock()
	for _, reply := range held {
		_ = reply.conn.forward(reply.token, reply.body)
	}
}

func (c *directoryGateConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *directoryGateConn) Close() error               { _ = c.reader.Close(); return c.Conn.Close() }

func (c *directoryGateConn) forward(token transport.UID, body []byte) error {
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()
	return transport.WriteFrame(c.writer, token, body, false)
}

func (c *directoryGateConn) readLoop() {
	var hello [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(c.Conn, hello[:]); err != nil {
		_ = c.writer.CloseWithError(err)
		return
	}
	c.forwardMu.Lock()
	_, err := c.writer.Write(hello[:])
	c.forwardMu.Unlock()
	if err != nil {
		return
	}
	for {
		token, body, err := transport.ReadFrame(c.Conn, false)
		if err != nil {
			_ = c.writer.CloseWithError(err)
			return
		}
		if len(body) >= 8 {
			id := binary.LittleEndian.Uint32(body[4:8])
			if id == types.GetValueReplyFileID || id == 2<<24|types.GetValueReplyFileID {
				c.gate.mu.Lock()
				if c.gate.armed {
					c.gate.held = append(c.gate.held, directoryHeldReply{c, token, append([]byte{}, body...)})
					select {
					case c.gate.reached <- struct{}{}:
					default:
					}
					c.gate.mu.Unlock()
					continue
				}
				c.gate.mu.Unlock()
			}
		}
		if c.forward(token, body) != nil {
			return
		}
	}
}

func TestDirectoryChildrenJoinAfterContextCancellation(t *testing.T) {
	t.Parallel()
	gate := &directoryReplyGate{}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
	t.Cleanup(gate.release)
	var ops []string
	for _, base := range []string{"CREATE_OR_OPEN", "CREATE", "MOVE", "MOVE_TO", "REMOVE", "REMOVE_IF_EXISTS"} {
		ops = append(ops, "DIRECTORY_"+base, "DIRECTORY_"+base+"_DATABASE")
	}
	for _, base := range []string{"OPEN", "LIST", "EXISTS", "LOG_DIRECTORY"} {
		ops = append(ops, "DIRECTORY_"+base, "DIRECTORY_"+base+"_DATABASE", "DIRECTORY_"+base+"_SNAPSHOT")
	}
	instruction := func(op string, args ...any) []tuple.Tuple {
		var program []tuple.Tuple
		for i := len(args) - 1; i >= 0; i-- {
			program = append(program, tuple.Tuple{"PUSH", args[i]})
		}
		return append(program, tuple.Tuple{op})
	}
	for _, op := range ops {
		for _, joinTimeout := range []bool{false, true} {
			name := op + "/caller"
			if joinTimeout {
				name = op + "/join-timeout"
			}
			t.Run(name, func(t *testing.T) {
				root, child := name+"/root", name+"/child"
				barrier := []byte(name + "/parent-barrier")
				ready := []byte(name + "/child-setup-barrier")
				startOp := []byte(name + "/child-operation-barrier")
				sm := NewStackMachine(db, []byte(root))
				if _, err := sm.dirList[0].dir.CreateOrOpen(sm.fdbDB(ctx), []string{name, "scope", "source"}, nil); err != nil {
					t.Fatal(err)
				}
				base := strings.TrimSuffix(strings.TrimSuffix(op, "_DATABASE"), "_SNAPSHOT")
				var args []any
				switch base {
				case "DIRECTORY_CREATE_OR_OPEN":
					args = []any{int64(1), "new", []byte{}}
				case "DIRECTORY_CREATE":
					args = []any{int64(1), "new", []byte{}, nil}
				case "DIRECTORY_OPEN":
					args = []any{int64(1), "source", []byte{}}
				case "DIRECTORY_MOVE":
					args = []any{int64(1), "source", int64(1), "destination"}
				case "DIRECTORY_MOVE_TO":
					args = []any{int64(2), name, "moved-scope"}
				case "DIRECTORY_REMOVE", "DIRECTORY_REMOVE_IF_EXISTS", "DIRECTORY_LIST", "DIRECTORY_EXISTS":
					args = []any{int64(1), int64(1), "source"}
				case "DIRECTORY_LOG_DIRECTORY":
					args = []any{[]byte(name + "/directory-log/")}
				default:
					t.Fatalf("missing directory cancellation arguments for %s", op)
				}
				program := instruction("DIRECTORY_OPEN_DATABASE", int64(2), name, "scope", []byte{})
				program = append(program, tuple.Tuple{"PUSH", int64(1)}, tuple.Tuple{"DIRECTORY_CHANGE"},
					tuple.Tuple{"PUSH", ready}, tuple.Tuple{"CLEAR"}, tuple.Tuple{"COMMIT"}, tuple.Tuple{"NEW_TRANSACTION"},
					tuple.Tuple{"PUSH", startOp}, tuple.Tuple{"WAIT_EMPTY"})
				program = append(program, instruction(op, args...)...)
				if _, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
					tx.Set(ready, []byte("held"))
					tx.Set(startOp, []byte("held"))
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
					root:  {{"PUSH", []byte(child)}, {"START_THREAD"}, {"PUSH", barrier}, {"WAIT_EMPTY"}},
					child: program,
				}, barrier)
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				// The parent cannot start its join timer until the test releases
				// the barrier after observing the child's held directory read.
				sm.childJoinTimeout = 30 * time.Second
				if joinTimeout {
					sm.childJoinTimeout = 2 * time.Second
				}
				errs := make(chan error, 1)
				go func() { errs <- sm.Run(runCtx) }()
				readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
				defer readyCancel()
				if err := sm.waitEmpty(readyCtx, ready); err != nil {
					t.Fatalf("child directory setup did not finish: %v", err)
				}
				gate.arm()
				defer gate.release()
				if _, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
					tx.Clear(startOp)
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-gate.reached:
				case err := <-errs:
					t.Fatalf("Run ended before directory read blocked: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("directory read did not reach real FDB")
				}
				if joinTimeout {
					_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
						tx.Clear(barrier)
						return nil, nil
					})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				select {
				case err := <-errs:
					if joinTimeout {
						if err == nil || !strings.Contains(err.Error(), "timed out waiting for child threads") {
							t.Fatalf("join error=%v", err)
						}
					} else if !errors.Is(err, context.Canceled) {
						t.Fatalf("caller cancellation=%v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Run hung joining child blocked in directory facade")
				}
				assertThreadWorkersJoined(t, sm)
				if ctx.Err() != nil {
					t.Fatal("directory cancellation escaped to caller's parent context")
				}
			})
		}
	}
}
