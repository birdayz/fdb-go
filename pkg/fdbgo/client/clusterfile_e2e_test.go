package client

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// forwardInjectConn proxies frames (like wrongShardConn) but, when injecting,
// replaces the FIRST non-PING server reply with a crafted ClientDBInfo carrying a
// `forward` — simulating an old coordinator forwarding the client to a rotated set
// during `coordinators auto`/`change`. PINGs pass through so the connection isn't
// torn down as dead; subsequent replies pass through (inject-once).
type forwardInjectConn struct {
	net.Conn
	pr          *io.PipeReader
	forwardBody []byte
	armed       atomic.Bool
}

func newForwardInjectConn(c net.Conn, forwardBody []byte, inject bool) *forwardInjectConn {
	pr, pw := io.Pipe()
	fc := &forwardInjectConn{Conn: c, pr: pr, forwardBody: forwardBody}
	fc.armed.Store(inject)
	go fc.proxyLoop(pw)
	return fc
}

func (f *forwardInjectConn) Read(b []byte) (int, error) { return f.pr.Read(b) }

func (f *forwardInjectConn) proxyLoop(pw *io.PipeWriter) {
	defer pw.Close()
	var cpBuf [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(f.Conn, cpBuf[:]); err != nil {
		pw.CloseWithError(err)
		return
	}
	if _, err := pw.Write(cpBuf[:]); err != nil {
		return
	}
	pingToken := transport.WellKnownToken(transport.WLTokenPingPacket)
	for {
		token, body, err := transport.ReadFrame(f.Conn, false)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if token != pingToken && f.armed.CompareAndSwap(true, false) {
			body = f.forwardBody // replace the OpenDatabaseCoordRequest reply with a forward
		}
		if err := transport.WriteFrame(pw, token, body, false); err != nil {
			return
		}
	}
}

type forwardInjectDialer struct {
	injectAddr  string
	forwardBody []byte
	enabled     bool
}

func (d *forwardInjectDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return newForwardInjectConn(c, d.forwardBody, d.enabled && addr == d.injectAddr), nil
}

// TestClusterFileRewatch_FollowsForward_E2E is the gold end-to-end proof that a
// coordinator-set rotation no longer strands the client (RFC-111 P0.1, test 6).
// Two real clusters A and B: the client opens against A's cluster file, but A's
// first coordinator reply is replaced with a `forward` to B (the wire shape of
// `coordinators change`). The client must adopt B's coordinators, persist them to
// the on-disk cluster file, connect to B, and run a transaction there.
//
// Revert-proof: remove the forward handling in bootstrap/followForward and the
// client stops at A — the active connection string stays A and the assertions on
// B fail. (The negative control TestClusterFileRewatch_NoForward_E2E confirms that
// without an injected forward the same setup connects to A unchanged.)
func TestClusterFileRewatch_FollowsForward_E2E(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfA := startProxyFDB(t, ctx)
	cfB := startProxyFDB(t, ctx)

	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	if err := os.WriteFile(path, []byte(cfA.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	forwardBody := (&types.ClientDBInfo{HasForward: true, Forward: []byte(cfB.String())}).MarshalFDB()
	d := &forwardInjectDialer{injectAddr: cfA.Coordinators[0], forwardBody: forwardBody, enabled: true}

	db, err := OpenDatabase(ctx, path, WithDialFunc(d.dial), WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase (should follow forward to B): %v", err)
	}
	defer db.Close()

	// Active connection string is now B's.
	if got := db.db.connRecord.get().String(); got != cfB.String() {
		t.Fatalf("did not adopt forward: active=%q, want B=%q", got, cfB.String())
	}
	// The on-disk cluster file was rewritten to B's connection string (cross-tool
	// shared state), with the C++-faithful header.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# DO NOT EDIT!\n") || !strings.Contains(string(raw), cfB.String()) {
		t.Fatalf("cluster file not rewritten to B: %q", raw)
	}
	// And the client genuinely operates against B.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("rfc111_forward_key"), []byte("on_B"))
		return nil, nil
	}); err != nil {
		t.Fatalf("Transact against adopted cluster B: %v", err)
	}
	got, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("rfc111_forward_key"))
	})
	if err != nil {
		t.Fatalf("read-back against B: %v", err)
	}
	if string(got.([]byte)) != "on_B" {
		t.Fatalf("read-back = %q, want on_B", got)
	}
}

// TestClusterFileRewatch_NoForward_E2E is the negative control: same setup, no
// injected forward → the client connects to A and leaves the cluster file
// untouched. Together with the positive test this isolates the forward-follow as
// the cause of the coordinator switch.
func TestClusterFileRewatch_NoForward_E2E(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfA := startProxyFDB(t, ctx)

	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	if err := os.WriteFile(path, []byte(cfA.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &forwardInjectDialer{injectAddr: cfA.Coordinators[0], enabled: false}

	db, err := OpenDatabase(ctx, path, WithDialFunc(d.dial), WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close()

	if got := db.db.connRecord.get().String(); got != cfA.String() {
		t.Fatalf("active connection string changed without a forward: %q", got)
	}
}
