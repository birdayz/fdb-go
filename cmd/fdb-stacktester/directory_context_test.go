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

// Withhold only value replies: instruction range scans and commits still run,
// so a held reply proves that the child reached an actual directory read.
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
	for _, op := range []string{"DIRECTORY_CREATE_OR_OPEN_DATABASE", "DIRECTORY_EXISTS_DATABASE", "DIRECTORY_EXISTS", "DIRECTORY_EXISTS_SNAPSHOT"} {
		for _, joinTimeout := range []bool{false, true} {
			name := op + "/caller"
			if joinTimeout {
				name = op + "/join-timeout"
			}
			t.Run(name, func(t *testing.T) {
				root, child := name+"/root", name+"/child"
				barrier := []byte(name + "/parent-barrier")
				program := []tuple.Tuple{{"PUSH", "uncreated-directory"}, {"PUSH", int64(1)}, {op}}
				if op == "DIRECTORY_CREATE_OR_OPEN_DATABASE" {
					program = append([]tuple.Tuple{{"PUSH", []byte{}}}, program...)
				} else {
					// EXISTS pops the optional-path flag before the path's tuple count.
					program = append(program[:len(program)-1], tuple.Tuple{"PUSH", int64(1)}, tuple.Tuple{op})
				}
				writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
					root:  {{"PUSH", []byte(child)}, {"START_THREAD"}, {"PUSH", barrier}, {"WAIT_EMPTY"}},
					child: program,
				}, barrier)
				gate.arm()
				defer gate.release()
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				sm := NewStackMachine(db, []byte(root))
				// The parent cannot start its join timer until the test releases
				// the barrier after observing the child's held directory read.
				sm.childJoinTimeout = 30 * time.Second
				if joinTimeout {
					sm.childJoinTimeout = 2 * time.Second
				}
				errs := make(chan error, 1)
				go func() { errs <- sm.Run(runCtx) }()
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
