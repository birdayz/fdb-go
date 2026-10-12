package client

import (
	"context"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// Hold only commit replies: forwarding PINGs keeps the real connection alive.
type commitReplyGate struct {
	mu         sync.Mutex
	armed      bool
	severFirst bool
	seen       int
	held       []heldCommitReply
	reached    chan struct{}
}

type heldCommitReply struct {
	conn  *commitGateConn
	token transport.UID
	body  []byte
}

type commitGateConn struct {
	net.Conn
	gate *commitReplyGate
	pr   *io.PipeReader
	pw   *io.PipeWriter
	mu   sync.Mutex
}

func (g *commitReplyGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	c := &commitGateConn{Conn: raw, gate: g, pr: pr, pw: pw}
	go c.readLoop()
	return c, nil
}

func (c *commitGateConn) Read(b []byte) (int, error) { return c.pr.Read(b) }

func (c *commitGateConn) Close() error {
	_ = c.pr.Close()
	_ = c.pw.Close()
	return c.Conn.Close()
}

func (c *commitGateConn) forward(token transport.UID, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return transport.WriteFrame(c.pw, token, body, false)
}

func (c *commitGateConn) readLoop() {
	defer c.pw.Close()
	var hello [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(c.Conn, hello[:]); err != nil {
		c.pw.CloseWithError(err)
		return
	}
	if _, err := c.pw.Write(hello[:]); err != nil {
		return
	}
	for {
		token, body, err := transport.ReadFrame(c.Conn, false)
		if err != nil {
			c.pw.CloseWithError(err)
			return
		}
		if isCommitReplyBody(body) {
			g := c.gate
			g.mu.Lock()
			if g.armed {
				g.seen++
				if g.severFirst && g.seen == 1 {
					g.mu.Unlock()
					_ = c.Close()
					return
				}
				g.held = append(g.held, heldCommitReply{conn: c, token: token, body: append([]byte(nil), body...)})
				if len(g.held) == 1 {
					close(g.reached)
				}
				g.mu.Unlock()
				continue
			}
			g.mu.Unlock()
		}
		if c.forward(token, body) != nil {
			return
		}
	}
}

func (g *commitReplyGate) release() error {
	g.mu.Lock()
	g.armed = false
	held := g.held
	g.held = nil
	g.mu.Unlock()
	for _, h := range held {
		if err := h.conn.forward(h.token, h.body); err != nil {
			return err
		}
	}
	return nil
}

func newGatedCommit(t *testing.T, ctx context.Context, severFirst bool) (*commitReplyGate, *Database, *Transaction, []byte) {
	t.Helper()
	g := &commitReplyGate{severFirst: severFirst, reached: make(chan struct{})}
	db := newTestDatabase(t, ctx, sharedClusterFile, g.dial)
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() { _ = g.release() })
	tx := db.CreateTransaction()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	key := []byte("commit-wait/" + t.Name())
	tx.Set(key, []byte("committed"))
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
	return g, db, tx, key
}

func startGatedCommit(tx *Transaction, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- tx.Commit(ctx) }()
	return done
}

func awaitGatedCommit(t *testing.T, ctx context.Context, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatalf("commit did not finish: %v", ctx.Err())
		return nil
	}
}

func releaseCommitGate(t *testing.T, g *commitReplyGate) {
	t.Helper()
	if err := g.release(); err != nil {
		t.Fatalf("release real commit reply: %v", err)
	}
}

func TestCommitWait_NoSyntheticTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, db, tx, key := newGatedCommit(t, ctx, false)
	done := startGatedCommit(tx, ctx)
	waitCommitLifetimeGate(t, ctx, g.reached, "real commit reply withheld")
	select {
	case err := <-done:
		t.Fatalf("commit returned before its held reply: %v", err)
	case <-time.After(DefaultRPCTimeout + time.Second):
	}
	releaseCommitGate(t, g)
	if err := awaitGatedCommit(t, ctx, done); err != nil {
		t.Fatalf("healthy delayed commit became unknown: %v", err)
	}
	if got := readCommitLifetimeValue(t, ctx, db, key); string(got) != "committed" {
		t.Fatalf("committed value = %q", got)
	}
}

func TestCommitWait_TransactionInterrupts(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "timeout", "reset"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			g, db, tx, key := newGatedCommit(t, ctx, false)
			done := startGatedCommit(tx, ctx)
			waitCommitLifetimeGate(t, ctx, g.reached, "real commit reply withheld")
			native := currentNativeCommit(t, tx)
			want := ErrTransactionCancelled
			switch mode {
			case "cancel":
				tx.Cancel()
			case "timeout":
				// Arm the timeout only after the actual commit reply is withheld.
				tx.SetTimeout(1)
				want = ErrTransactionTimedOut
			case "reset":
				tx.Reset()
			}
			waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			err := awaitGatedCommit(t, waitCtx, done)
			requireCommitLifetimeCode(t, err, want, "interrupted public commit wait")
			if mode == "reset" {
				waitCommitLifetimeGate(t, ctx, native.finished, "Reset drops native commit before reply")
			} else {
				select {
				case <-native.finished:
					t.Fatal("public interruption dropped native commit before reply")
				default:
				}
			}
			releaseCommitGate(t, g)
			waitCommitLifetimeGate(t, ctx, native.finished, "native commit after public interruption")
			version, err := tx.GetCommittedVersion()
			if err != nil {
				t.Fatal(err)
			}
			wantStamp := want
			completed := db.db.metrics.transactionsCommitCompleted.Load()
			if mode == "reset" {
				wantStamp = 2015
				if version != -1 || completed != 0 {
					t.Fatalf("Reset published retired native success: version %d, completed %d", version, completed)
				}
			} else if version <= 0 || completed != 1 {
				t.Fatalf("retained native success missing: version %d, completed %d", version, completed)
			}
			if _, err := tx.GetVersionstamp(); err == nil {
				t.Fatal("late native reply revived retired public versionstamp")
			} else {
				requireCommitLifetimeCode(t, err, wantStamp, "public versionstamp after native completion")
			}
			if got := readCommitLifetimeValue(t, ctx, db, key); string(got) != "committed" {
				t.Fatalf("a successful reply was withheld but its write vanished: %q", got)
			}
			runtime.KeepAlive(tx)
		})
	}
}

func TestCommitWait_TransactionInterruptionLeavesNativeFenceAlive(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), commitLifetimeTimeout)
			defer cancel()
			g, db, tx, key := newGatedCommit(t, ctx, true)
			done := startGatedCommit(tx, ctx)
			// The original reply was lost with its connection; this reply belongs to the fence.
			waitCommitLifetimeGate(t, ctx, g.reached, "native uncertainty fence reply withheld")
			native := currentNativeCommit(t, tx)
			want := ErrTransactionCancelled
			if mode == "cancel" {
				tx.Cancel()
			} else {
				tx.SetTimeout(1)
				want = ErrTransactionTimedOut
			}
			waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			requireCommitLifetimeCode(t, awaitGatedCommit(t, waitCtx, done), want, "interrupted public wait during fence")
			select {
			case <-native.finished:
				t.Fatal("public interruption dropped native uncertainty fence")
			default:
			}
			releaseCommitGate(t, g)
			waitCommitLifetimeGate(t, ctx, native.finished, "retained native fence after public interruption")
			if completed := db.db.metrics.transactionsCommitCompleted.Load(); completed != 1 {
				t.Fatalf("successful native fence counted %d completions, want 1", completed)
			}
			if version, err := tx.GetCommittedVersion(); err != nil || version != -1 {
				t.Fatalf("uncertain original published a committed version: %d, %v", version, err)
			}
			if _, err := tx.GetVersionstamp(); err == nil {
				t.Fatal("native fence completion revived public versionstamp")
			} else {
				requireCommitLifetimeCode(t, err, want, "public versionstamp after retained native fence")
			}
			if got := readCommitLifetimeValue(t, ctx, db, key); string(got) != "committed" {
				t.Fatalf("original applied mutation = %q", got)
			}
			runtime.KeepAlive(tx)
		})
	}
}

func TestCommitWait_CallerContextStaysDetached(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, _, tx, _ := newGatedCommit(t, ctx, false)
	caller, cancelCaller := context.WithCancel(ctx)
	defer cancelCaller()
	done := startGatedCommit(tx, caller)
	waitCommitLifetimeGate(t, ctx, g.reached, "real commit reply withheld")
	cancelCaller()
	select {
	case err := <-done:
		t.Fatalf("caller context changed dispatched commit outcome: %v", err)
	case <-time.After(time.Second):
	}
	releaseCommitGate(t, g)
	if err := awaitGatedCommit(t, ctx, done); err != nil {
		t.Fatalf("commit with canceled caller: %v", err)
	}
}
