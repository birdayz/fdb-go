package client

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// RFC-093 — the commit-path GRV (ensureReadVersion, transaction.go:1106) must
// honor the caller ctx, while the commit RPC + commit_unknown_result barrier
// (transaction.go:1126 → commitpath.go) stay detached (RFC-090). These tests pin
// that split end-to-end against a real FDB.
//
// Fault infra: a frame-level GRV-reply-blocking dialer. It mirrors
// wrongShardConn's proxy exactly (read complete frames, pass PINGs through), but
// instead of REPLACING the next non-PING reply body it HOLDS that frame on a
// release channel — leaving the in-flight GRV pending and the caller blocked in
// getReadVersion's select (grv.go:213). heldCh closes when the frame is captured,
// so the test can cancel deterministically (no sleeps).

// grvBlockConn wraps a net.Conn with a pipe-based proxy goroutine. When armed, the
// proxy holds the next non-PING response frame on releaseCh (blocking forwarding)
// and closes the dialer's heldCh, then forwards it once released.
type grvBlockConn struct {
	net.Conn
	d           *grvBlockDialer
	pr          *io.PipeReader
	injectCh    chan struct{} // buffered cap-1; arm() sends
	releaseCh   chan struct{} // closed to release a held frame
	releaseOnce sync.Once
}

func newGRVBlockConn(c net.Conn, d *grvBlockDialer) *grvBlockConn {
	pr, pw := io.Pipe()
	w := &grvBlockConn{
		Conn:      c,
		d:         d,
		pr:        pr,
		injectCh:  make(chan struct{}, 1),
		releaseCh: make(chan struct{}),
	}
	go w.proxyLoop(pw)
	return w
}

func (w *grvBlockConn) Read(b []byte) (int, error) { return w.pr.Read(b) }

func (w *grvBlockConn) proxyLoop(pw *io.PipeWriter) {
	defer pw.Close()

	// Forward the ConnectPacket before entering frame mode (matches wrongShardConn).
	var cpBuf [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(w.Conn, cpBuf[:]); err != nil {
		pw.CloseWithError(err)
		return
	}
	if _, err := pw.Write(cpBuf[:]); err != nil {
		return
	}

	pingToken := transport.WellKnownToken(transport.WLTokenPingPacket)
	for {
		token, body, err := transport.ReadFrame(w.Conn, false)
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		select {
		case <-w.injectCh:
			if token == pingToken {
				// PING — pass through, re-queue the inject signal (next non-PING
				// frame is the one we hold).
				w.injectCh <- struct{}{}
			} else {
				// Capture: the GRV reply. Signal held, then block until released.
				// injectCh is now drained, so only this one frame is held.
				w.d.markHeld()
				<-w.releaseCh
			}
		default:
		}

		if err := transport.WriteFrame(pw, token, body, false); err != nil {
			return
		}
	}
}

func (w *grvBlockConn) arm() {
	select {
	case w.injectCh <- struct{}{}:
	default:
	}
}

func (w *grvBlockConn) release() {
	w.releaseOnce.Do(func() { close(w.releaseCh) })
}

// grvBlockDialer creates grvBlockConns. armAll() holds the next non-PING reply on
// every connection; heldCh closes when any connection captures one.
type grvBlockDialer struct {
	mu       sync.Mutex
	conns    []*grvBlockConn
	heldCh   chan struct{}
	heldOnce sync.Once
}

func newGRVBlockDialer() *grvBlockDialer {
	return &grvBlockDialer{heldCh: make(chan struct{})}
}

func (d *grvBlockDialer) markHeld() {
	d.heldOnce.Do(func() { close(d.heldCh) })
}

func (d *grvBlockDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	w := newGRVBlockConn(c, d)
	d.mu.Lock()
	d.conns = append(d.conns, w)
	d.mu.Unlock()
	return w, nil
}

func (d *grvBlockDialer) armAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.arm()
	}
}

// releaseAll unwedges every held proxyLoop. MUST run before the conns/pipes close
// or a still-held proxyLoop leaks its goroutine + conn (Torvalds). Idempotent.
func (d *grvBlockDialer) releaseAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.release()
	}
}

func newGRVBlockTestDB(t *testing.T, ctx context.Context) (*Database, *grvBlockDialer) {
	t.Helper()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	// Terminate on a FRESH context (t.Cleanup runs after the caller's cancel).
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		container.Terminate(termCtx)
	})

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, internalReader, _ := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, _ := ParseClusterString(strings.TrimSpace(internalStr))

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	gd := newGRVBlockDialer()
	db := newTestDatabase(t, ctx, connectCF, gd.dial)
	// LIFO: releaseAll is registered LAST so it runs FIRST — proxyLoops unblock
	// before db.Close/Terminate tear down the pipes.
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { gd.releaseAll() })
	return db, gd
}

// coldGRVCache forces the next commit-path GRV onto the slow (blockable) RPC path:
// tryCache short-circuits on version==0 (grv.go:42), and a cold cache yields no
// cache HIT, so the background GRV refresher (which only starts inside the hit
// branch) never runs — the commit's GRV is the only RPC in flight.
func coldGRVCache(db *Database) {
	db.db.grvCache.version.Store(0)
}

// TestFDB_CommitPathGRV_HonorsCtxCancel pins RFC-093: a ctx cancel arriving while
// the commit-path GRV is in flight aborts the commit promptly, and the write does
// not commit. Pre-fix (WithoutCancel over the whole Commit) the GRV ignored the
// cancel and the commit eventually succeeded — this test FAILS on the reverted code.
func TestFDB_CommitPathGRV_HonorsCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, gd := newGRVBlockTestDB(t, ctx)
	key := []byte(t.Name() + "_key")

	coldGRVCache(db)
	gd.armAll() // hold the next non-PING reply (the commit-path GRV reply)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := db.Transact(wctx, func(tx *Transaction) (any, error) {
			tx.Set(key, []byte("v")) // a write → Commit skips the read-only fast path → GRV
			return nil, nil
		})
		errCh <- err
	}()

	// Wait until the GRV reply is captured & held — the txn's getReadVersion is now
	// blocked in its select (grv.go:213). Deterministic, no sleep.
	select {
	case <-gd.heldCh:
	case <-time.After(60 * time.Second):
		t.Fatal("commit-path GRV reply was never held — setup wrong (cache not cold, or no GRV issued)")
	}

	wcancel() // cancel while the GRV is in flight

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("commit-path GRV did not honor ctx cancel: Transact returned %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Transact did not return within 15s after cancel — commit-path GRV ignored the cancel")
	}

	// The write must not have committed. Release the held GRV first so the verifying
	// read can make progress.
	gd.releaseAll()
	got, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify read: %v", err)
	}
	if b, _ := got.([]byte); len(b) != 0 {
		t.Fatalf("key was committed despite cancel: got %q, want absent", b)
	}
}

// TestFDB_CommitReadOnlyNoForcedGRV guards against the reverted-P2 regression: a
// no-op/read-only commit hits the fast-path return (transaction.go:1100) BEFORE any
// GRV, so with the GRV blocker armed it must NOT capture a frame and must return
// promptly. If a GRV is forced here, heldCh fires and the test fails.
func TestFDB_CommitReadOnlyNoForcedGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, gd := newGRVBlockTestDB(t, ctx)

	coldGRVCache(db)
	gd.armAll()

	errCh := make(chan error, 1)
	go func() {
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, nil // no mutations, no reads → no commit, no GRV
		})
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("no-op Transact: %v", err)
		}
	case <-gd.heldCh:
		t.Fatal("no-op commit issued a GRV (held by the blocker) — forced-GRV regression on the read-only fast path")
	case <-time.After(20 * time.Second):
		t.Fatal("no-op Transact did not return — unexpected blocking")
	}
}
