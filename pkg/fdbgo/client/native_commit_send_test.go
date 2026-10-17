package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Unlike an address-only gate, this parks only a decoded commit request, not
// an earlier PING or GRV. The reply gate is the sole reader of the raw socket.
type nativeCommitWriteGate struct {
	mu           sync.Mutex
	fence        bool
	armed        bool
	parked       chan struct{}
	release      chan struct{}
	once         sync.Once
	replies      *commitReplyGate
	addr         string
	request      types.CommitTransactionRequest
	err          error
	forward      bool // forward to FDB before parking the socket write's return
	writeErr     error
	partial      bool
	failedBytes  int
	failedLength int
	targetStart  int
	targetEnd    int
	commits      int
	fences       int
}

func (g *nativeCommitWriteGate) releaseIt() { g.once.Do(func() { close(g.release) }) }

func (g *nativeCommitWriteGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var c net.Conn
	var err error
	if g.replies != nil {
		c, err = g.replies.dial(ctx, network, addr)
	} else {
		c, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	if err != nil {
		return nil, err
	}
	return &nativeCommitWriteConn{Conn: c, gate: g, addr: addr, hello: transport.ConnectPacketSize}, nil
}

type nativeCommitWriteConn struct {
	net.Conn
	gate    *nativeCommitWriteGate
	addr    string
	hello   int
	pending []byte
	blocked atomic.Bool
}

func (c *nativeCommitWriteConn) Write(p []byte) (int, error) {
	forwarded := false
	var written int
	var writeErr error
	incoming := p
	if c.hello > 0 {
		n := min(c.hello, len(incoming))
		c.hello -= n
		incoming = incoming[n:]
	}
	frameStart := len(p) - len(incoming) - len(c.pending)
	c.pending = append(c.pending, incoming...)
	for len(c.pending) >= 4 {
		length := int(binary.LittleEndian.Uint32(c.pending)) + 12
		if length < 28 || length > 100<<20+12 {
			return 0, fmt.Errorf("invalid outbound frame length %d", length)
		}
		if len(c.pending) < length {
			break
		}
		_, body, err := transport.ReadFrame(bytes.NewReader(c.pending[:length]), false)
		if err != nil {
			return 0, err
		}
		start, end := frameStart, frameStart+length
		frameStart = end
		c.pending = c.pending[length:]
		// CommitProxyInterface.h: CommitTransactionRequest::file_identifier.
		if len(body) < 8 || binary.LittleEndian.Uint32(body[4:8]) != 93948 {
			continue
		}
		var request types.CommitTransactionRequest
		err = request.UnmarshalFDB(body)
		g := c.gate
		g.mu.Lock()
		isFence := len(request.Transaction.Mutations) == 0 && len(request.Transaction.WriteConflictRanges) > 0
		if isFence {
			g.fences++
		} else {
			g.commits++
		}
		hold := g.armed && (err != nil || isFence == g.fence)
		if hold {
			g.armed, g.addr, g.request, g.err = false, c.addr, request, err
			g.targetStart, g.targetEnd = start, end
		}
		g.mu.Unlock()
		if hold {
			c.blocked.Store(true)
			before := bytes.Clone(p)
			if g.forward {
				written, writeErr = c.Conn.Write(p)
				forwarded = true
			}
			close(g.parked)
			<-g.release
			if !bytes.Equal(p, before) {
				g.mu.Lock()
				g.err = fmt.Errorf("parked wire frame changed after native owner retirement")
				g.mu.Unlock()
				return 0, io.ErrUnexpectedEOF
			}
			if g.forward {
				if writeErr == nil {
					writeErr = g.writeErr
				}
				if g.writeErr != nil {
					g.mu.Lock()
					g.failedBytes, g.failedLength = written, len(p)
					g.mu.Unlock()
				}
				continue
			}
			if g.writeErr != nil {
				if g.partial {
					cut := max(start, 0)
					cut += (end - cut) / 2
					written, writeErr = c.Conn.Write(p[:cut])
				}
				g.mu.Lock()
				g.failedBytes, g.failedLength = written, len(p)
				g.mu.Unlock()
				if writeErr != nil {
					return written, writeErr
				}
				return written, g.writeErr
			}
		}
	}
	if forwarded {
		return written, writeErr
	}
	return c.Conn.Write(p)
}

func (c *nativeCommitWriteConn) Close() error {
	if c.blocked.Load() {
		c.gate.releaseIt()
	}
	return c.Conn.Close()
}

func newNativeCommitWrite(t *testing.T, ctx context.Context, fence bool) (*nativeCommitWriteGate, *Database, *Transaction, []byte, []byte, <-chan error) {
	t.Helper()
	g := &nativeCommitWriteGate{fence: fence, parked: make(chan struct{}), release: make(chan struct{})}
	return startNativeCommitWrite(t, ctx, g)
}

func startNativeCommitWrite(t *testing.T, ctx context.Context, g *nativeCommitWriteGate) (*nativeCommitWriteGate, *Database, *Transaction, []byte, []byte, <-chan error) {
	t.Helper()
	if g.fence {
		g.replies = &commitReplyGate{severFirst: true, reached: make(chan struct{})}
	}
	db := newTestDatabase(t, ctx, sharedClusterFile, g.dial)
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		if g.replies != nil {
			_ = g.replies.release()
		}
		g.releaseIt()
	})
	tx := db.CreateTransaction()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	key := []byte(t.Name() + "/old")
	value := bytes.Repeat([]byte("native-commit-body"), 128)
	tx.Set(key, value)
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
	if g.replies != nil {
		g.replies.mu.Lock()
		g.replies.armed = true
		g.replies.mu.Unlock()
	}
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, g.parked, "actual commit frame at socket write")
	g.mu.Lock()
	request, gateErr := g.request, g.err
	g.mu.Unlock()
	if gateErr != nil {
		t.Fatal(gateErr)
	}
	if g.fence {
		if len(request.Transaction.Mutations) != 0 || len(request.Transaction.ReadConflictRanges) == 0 || len(request.Transaction.WriteConflictRanges) == 0 {
			t.Fatalf("parked request is not the dummy fence: %+v", request.Transaction)
		}
	} else {
		if len(request.Transaction.Mutations) != 1 {
			t.Fatalf("parked request has %d mutations", len(request.Transaction.Mutations))
		}
		mutation := request.Transaction.Mutations[0]
		if mutation.MutType != uint8(MutSetValue) || !bytes.Equal(mutation.Param1, key) || !bytes.Equal(mutation.Param2, value) {
			t.Fatalf("parked frame is not the original mutation: %+v", mutation)
		}
	}
	return g, db, tx, key, value, done
}

func abandonNativeCommitWrite(t *testing.T, ctx context.Context, fence bool) (*nativeCommitWriteGate, *Database, *nativeCommit, weak.Pointer[Transaction], []byte, []byte) {
	t.Helper()
	g, db, tx, key, value, done := newNativeCommitWrite(t, ctx, fence)
	native := currentNativeCommit(t, tx)
	tx.Cancel()
	waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	awaitCommitLifetimeCode(t, waitCtx, done, ErrTransactionCancelled, "Cancel at actual commit write")
	select {
	case <-native.finished:
		t.Fatal("Cancel destroyed the retained native write")
	default:
	}
	return g, db, native, weak.Make(tx), key, value
}

func TestNativeCommit_ReplyBeforeWriteAcknowledgment(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"commit", "fence"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/write-error-%t", phase, fail), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
				defer cancel()
				g := &nativeCommitWriteGate{
					fence: phase == "fence", forward: true,
					parked: make(chan struct{}), release: make(chan struct{}),
				}
				if fail {
					g.writeErr = io.ErrUnexpectedEOF
				}
				g, db, tx, key, value, done := startNativeCommitWrite(t, ctx, g)
				if g.replies != nil {
					releaseCommitGate(t, g.replies)
				}
				waitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
				defer stop()
				if g.fence {
					awaitCommitLifetimeCode(t, waitCtx, done, 1021, "uncertainty fence reply before write acknowledgment")
				} else {
					awaitCommitLifetimeResult(t, waitCtx, done, "commit reply before write acknowledgment")
				}
				select {
				case <-g.release:
					t.Fatal("socket write returned before the real reply completed the native actor")
				default:
				}
				g.mu.Lock()
				commits, fences := g.commits, g.fences
				g.mu.Unlock()
				wantFences := 0
				if g.fence {
					wantFences = 1
				}
				if commits != 1 || fences != wantFences {
					t.Fatalf("sent %d original commits and %d fences, want 1 and %d", commits, fences, wantFences)
				}
				verify := openTestDB(t, ctx)
				if got := readCommitLifetimeValue(t, ctx, verify, key); !bytes.Equal(got, value) {
					t.Fatalf("real FDB committed value = %q, want %q", got, value)
				}
				g.mu.Lock()
				addr := g.addr
				g.mu.Unlock()
				conn, err := db.db.getOrDial(ctx, addr)
				if err != nil {
					t.Fatal(err)
				}
				g.releaseIt()
				if fail {
					waitCommitLifetimeGate(t, ctx, conn.Done(), "late socket failure after a completed native result")
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				g.mu.Lock()
				gateErr, written, length := g.err, g.failedBytes, g.failedLength
				commits, fences = g.commits, g.fences
				g.mu.Unlock()
				if gateErr != nil {
					t.Fatal(gateErr)
				}
				if commits != 1 || fences != wantFences {
					t.Fatalf("late failure resent work: %d original commits and %d fences", commits, fences)
				}
				if fail && (length == 0 || written != length) {
					t.Fatalf("late write failure did not follow a full real write: %d/%d", written, length)
				}
				runtime.KeepAlive(tx)
			})
		}
	}
}

func TestNativeCommit_QueuedWriteFailureFences(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"commit", "fence"} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial-%t", phase, partial), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
				defer cancel()
				g := &nativeCommitWriteGate{
					fence: phase == "fence", partial: partial, writeErr: io.ErrUnexpectedEOF,
					parked: make(chan struct{}), release: make(chan struct{}),
				}
				g, db, tx, key, value, done := startNativeCommitWrite(t, ctx, g)
				if g.replies != nil {
					releaseCommitGate(t, g.replies)
				}
				g.releaseIt()
				awaitCommitLifetimeCode(t, ctx, done, 1021, "queued socket failure after uncertainty fence")
				g.mu.Lock()
				commits, fences, written, length := g.commits, g.fences, g.failedBytes, g.failedLength
				start, end := g.targetStart, g.targetEnd
				g.mu.Unlock()
				wantFences := 1
				if g.fence {
					wantFences = 2 // the failed fence must itself be retried
				}
				if commits != 1 || fences != wantFences {
					t.Fatalf("sent %d original commits and %d fences, want 1 and %d", commits, fences, wantFences)
				}
				if length == 0 || end <= start || written >= end || (!partial && written != 0) || (partial && written <= start) {
					t.Fatalf("fault wrote %d/%d bytes with target frame [%d,%d), partial=%t", written, length, start, end, partial)
				}
				verify := openTestDB(t, ctx)
				got := readCommitLifetimeValue(t, ctx, verify, key)
				if g.fence {
					if !bytes.Equal(got, value) {
						t.Fatalf("lost original reply must preserve its committed value: %q", got)
					}
				} else if got != nil {
					t.Fatalf("incomplete original frame unexpectedly committed: %q", got)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				runtime.KeepAlive(tx)
			})
		}
	}
}

func TestNativeCommit_QueuedProxyChangeFences(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
	defer cancel()
	g := &nativeCommitWriteGate{
		parked: make(chan struct{}), release: make(chan struct{}),
		replies: &commitReplyGate{reached: make(chan struct{})},
	}
	g, db, tx, key, value, done := startNativeCommitWrite(t, ctx, g)
	pause := installOneShotCommitPause(t, db)
	defer pause.unblock()
	before := db.Metrics()
	db.db.proxiesChangedMu.Lock()
	close(db.db.proxiesChanged)
	db.db.proxiesChanged = make(chan struct{})
	db.db.proxiesChangedMu.Unlock()
	g.releaseIt()
	waitCommitLifetimeGate(t, ctx, pause.reached, "uncertainty fence selected while the original reply is withheld")
	pause.unblock()
	releaseCommitGate(t, g.replies)
	awaitCommitLifetimeCode(t, ctx, done, 1021, "proxy change while original bytes were queued")
	g.mu.Lock()
	commits, fences := g.commits, g.fences
	g.mu.Unlock()
	// The queued original can conflict with the first fence. C++ retries that
	// fence via onError, never the original (NativeAPI.actor.cpp:6334–6343).
	after := db.Metrics()
	conflicts := after.TransactionsNotCommitted - before.TransactionsNotCommitted
	retries := after.TransactionRetries - before.TransactionRetries
	if commits != 1 || fences < 1 || int64(fences) != 1+conflicts || retries != conflicts {
		t.Fatalf("sent %d original commits and %d fences, with %d conflict retries and %d total retries", commits, fences, conflicts, retries)
	}
	if got := after.TransactionsCommitCompleted - before.TransactionsCommitCompleted; got != 1 {
		t.Fatalf("successful fence completions = %d, want 1", got)
	}
	verify := openTestDB(t, ctx)
	if got := readCommitLifetimeValue(t, ctx, verify, key); got != nil && !bytes.Equal(got, value) {
		t.Fatalf("uncertain commit corrupted its operand: %q", got)
	}
	runtime.KeepAlive(tx)
}

func TestNativeCommit_WriteBackpressureOwnerRetirement(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"commit", "fence"} {
		for _, mode := range []string{"reset", "cancel-owner-gc"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
				defer cancel()
				var g *nativeCommitWriteGate
				var db *Database
				var native *nativeCommit
				var tx *Transaction
				var key, value []byte
				if mode == "reset" {
					var done <-chan error
					g, db, tx, key, value, done = newNativeCommitWrite(t, ctx, phase == "fence")
					native = currentNativeCommit(t, tx)
					tx.Reset()
					waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
					defer stop()
					awaitCommitLifetimeCode(t, waitCtx, done, ErrTransactionCancelled, "Reset at actual commit write")
				} else {
					var owner weak.Pointer[Transaction]
					g, db, native, owner, key, value = abandonNativeCommitWrite(t, ctx, phase == "fence")
					gcCtx, stop := context.WithTimeout(ctx, 3*time.Second)
					defer stop()
					for {
						runtime.GC()
						if owner.Value() == nil {
							break
						}
						select {
						case <-gcCtx.Done():
							t.Fatal("native socket write retains abandoned owner")
						case <-time.After(time.Millisecond):
						}
					}
				}
				waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				defer stop()
				waitCommitLifetimeGate(t, waitCtx, native.finished, "native actor exits before socket writer release")
				select {
				case <-g.release:
					t.Fatal("owner retirement closed the gated shared connection")
				default:
				}
				g.mu.Lock()
				addr := g.addr
				g.mu.Unlock()
				conn, err := db.db.getOrDial(ctx, addr)
				if err != nil || conn.IsClosed() {
					t.Fatalf("gated connection unhealthy after owner retirement: %v", err)
				}
				failures := db.db.metrics.Snapshot().ClientConnectionFailures

				// Exercise the same marshal pool and fresh operand slabs while the
				// original writer still owns its bytes; queued writes may still apply.
				verify := openTestDB(t, ctx)
				for i := 0; i < 8; i++ {
					reuse := verify.CreateTransaction()
					reuse.Set([]byte(fmt.Sprintf("%s/reuse-%d", t.Name(), i)), bytes.Repeat([]byte{byte(i + 1)}, len(value)))
					if err := reuse.Commit(ctx); err != nil {
						t.Fatalf("marshal pool reuse: %v", err)
					}
				}
				if g.replies != nil {
					releaseCommitGate(t, g.replies)
				}
				g.releaseIt()
				if err := conn.FlushContext(ctx); err != nil {
					t.Fatalf("flush parked whole frame: %v", err)
				}
				g.mu.Lock()
				gateErr := g.err
				g.mu.Unlock()
				if gateErr != nil {
					t.Fatal(gateErr)
				}
				for {
					got := readCommitLifetimeValue(t, ctx, verify, key)
					if bytes.Equal(got, value) {
						break
					}
					if got != nil {
						t.Fatalf("queued commit data corrupted: %q", got)
					}
					select {
					case <-ctx.Done():
						t.Fatal("released whole commit frame did not apply its original data")
					case <-time.After(time.Millisecond):
					}
				}
				if conn.IsClosed() || db.db.failMon.isFailed(addr) || db.db.metrics.Snapshot().ClientConnectionFailures != failures {
					t.Fatal("native owner cancellation poisoned the healthy shared connection")
				}
				if tx == nil {
					tx = db.CreateTransaction()
				}
				tx.Set([]byte(t.Name()+"/new"), []byte("successor"))
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("successor on healthy connection: %v", err)
				}
				if got := readCommitLifetimeValue(t, ctx, verify, []byte(t.Name()+"/new")); string(got) != "successor" {
					t.Fatalf("successor mutation = %q", got)
				}
				runtime.KeepAlive(tx)
			})
		}
	}
}
