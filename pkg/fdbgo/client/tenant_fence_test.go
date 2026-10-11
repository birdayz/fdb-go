package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Preserve an outstanding commit across client-side disconnect so the server
// can receive it after the client reports commit_unknown_result.
type commitHoldDialer struct {
	commitTokens map[transport.UID]bool // protected by mu
	armed        atomic.Bool

	mu      sync.Mutex
	conns   []*commitHoldConn
	sent    []types.CommitTransactionRequest // every commit request, in send order
	heldIdx int                              // index into sent of the withheld request; -1 until held
	heldOn  *commitHoldConn
	held    struct {
		token transport.UID
		body  []byte
		reply chan []byte
	}
}

func newCommitHoldDialer() *commitHoldDialer {
	return &commitHoldDialer{heldIdx: -1}
}

func (d *commitHoldDialer) dial(_ context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	hc := newCommitHoldConn(c, d)
	d.mu.Lock()
	d.conns = append(d.conns, hc)
	d.mu.Unlock()
	return hc, nil
}

// observe records a client→server commit frame and reports whether it is the
// one to withhold.
func (d *commitHoldDialer) observe(c *commitHoldConn, token transport.UID, body []byte) (hold bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.commitTokens[token] {
		return false, nil
	}
	var req types.CommitTransactionRequest
	body = bytes.Clone(body)
	if err := req.UnmarshalFDB(body); err != nil {
		return false, fmt.Errorf("decode commit request: %w", err)
	}
	d.sent = append(d.sent, req)
	if !d.armed.Load() || d.heldIdx >= 0 {
		return false, nil
	}
	d.heldIdx = len(d.sent) - 1
	d.heldOn = c
	d.held.token, d.held.body = token, body
	d.held.reply = make(chan []byte, 1)
	return true, nil
}

// replyChFor returns where to deliver a server→client frame addressed to the
// withheld commit's reply token, or nil.
func (d *commitHoldDialer) replyChFor(c *commitHoldConn, token transport.UID) chan []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.heldOn != c || d.heldIdx < 0 {
		return nil
	}
	rt := d.sent[d.heldIdx].Reply.Token
	if token != (transport.UID{First: binary.LittleEndian.Uint64(rt[:8]), Second: binary.LittleEndian.Uint64(rt[8:])}) {
		return nil
	}
	return d.held.reply
}

// commitsAfterHold returns the withheld request and every commit request sent
// after it.
func (d *commitHoldDialer) commitsAfterHold() (held types.CommitTransactionRequest, after []types.CommitTransactionRequest, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.heldIdx < 0 {
		return types.CommitTransactionRequest{}, nil, false
	}
	return d.sent[d.heldIdx], append([]types.CommitTransactionRequest(nil), d.sent[d.heldIdx+1:]...), true
}

// release delivers the withheld commit to the server and waits for its reply.
func (d *commitHoldDialer) release(ctx context.Context) ([]byte, error) {
	d.mu.Lock()
	c, token, body, reply := d.heldOn, d.held.token, d.held.body, d.held.reply
	d.mu.Unlock()
	if c == nil {
		return nil, errors.New("no commit was withheld")
	}
	if err := transport.WriteFrame(c.Conn, token, body, false); err != nil {
		return nil, fmt.Errorf("deliver withheld commit: %w", err)
	}
	select {
	case b := <-reply:
		return b, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("no reply to the released commit: %w", ctx.Err())
	}
}

func (d *commitHoldDialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.Conn.Close()
	}
}

// Close preserves a withheld commit's server socket for late delivery.
// closeAll owns final cleanup.
type commitHoldConn struct {
	net.Conn
	d       *commitHoldDialer
	wpw     *io.PipeWriter // client writes → proxyWrites
	rpr     *io.PipeReader // proxyReads → client reads
	rpw     *io.PipeWriter
	holding atomic.Bool
}

func newCommitHoldConn(c net.Conn, d *commitHoldDialer) *commitHoldConn {
	wpr, wpw := io.Pipe()
	rpr, rpw := io.Pipe()
	hc := &commitHoldConn{Conn: c, d: d, wpw: wpw, rpr: rpr, rpw: rpw}
	go hc.proxyWrites(wpr)
	go hc.proxyReads()
	return hc
}

func (c *commitHoldConn) Write(b []byte) (int, error) { return c.wpw.Write(b) }
func (c *commitHoldConn) Read(b []byte) (int, error)  { return c.rpr.Read(b) }

func (c *commitHoldConn) Close() error {
	c.wpw.Close()
	c.rpr.Close()
	if !c.holding.Load() {
		return c.Conn.Close()
	}
	return nil
}

func (c *commitHoldConn) proxyWrites(pr *io.PipeReader) {
	var cp [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(pr, cp[:]); err != nil {
		return
	}
	if _, err := c.Conn.Write(cp[:]); err != nil {
		pr.CloseWithError(err)
		return
	}
	for {
		token, body, err := transport.ReadFrame(pr, false)
		if err != nil {
			pr.CloseWithError(err)
			return
		}
		if c.holding.Load() {
			continue // nothing the client sends after the withheld commit reaches the server
		}
		hold, err := c.d.observe(c, token, body)
		if err != nil {
			pr.CloseWithError(err)
			return
		}
		if hold {
			c.holding.Store(true)
			c.rpw.CloseWithError(io.EOF) // the client sees the connection die mid-commit
			continue
		}
		if err := transport.WriteFrame(c.Conn, token, body, false); err != nil {
			pr.CloseWithError(err)
			return
		}
	}
}

func (c *commitHoldConn) proxyReads() {
	var cp [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(c.Conn, cp[:]); err != nil {
		c.rpw.CloseWithError(err)
		return
	}
	if _, err := c.rpw.Write(cp[:]); err != nil {
		return
	}
	for {
		token, body, err := transport.ReadFrame(c.Conn, false)
		if err != nil {
			c.rpw.CloseWithError(err)
			return
		}
		if ch := c.d.replyChFor(c, token); ch != nil {
			select {
			case ch <- bytes.Clone(body):
			default:
			}
			continue
		}
		// Fails once the client side is closed; keep draining the socket so
		// the released commit's reply can still be seen.
		_ = transport.WriteFrame(c.rpw, token, body, false)
	}
}

// createTestTenant creates a tenant on the shared container and returns its ID.
func createTestTenant(t *testing.T, ctx context.Context, name string) int64 {
	t.Helper()
	if out, err := sharedContainer.FDBCLIExec(ctx, "tenant create "+name); err != nil {
		t.Fatalf("tenant create %s: %v: %s", name, err, out)
	}
	out, err := sharedContainer.FDBCLIExec(ctx, "tenant get "+name+" JSON")
	if err != nil {
		t.Fatalf("tenant get %s: %v: %s", name, err, out)
	}
	var got struct {
		Tenant struct {
			ID *int64 `json:"id"`
		} `json:"tenant"`
		Type string `json:"type"`
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("tenant get %s: no JSON in output: %s", name, out)
	}
	if err := json.Unmarshal([]byte(out[start:]), &got); err != nil || got.Type != "success" || got.Tenant.ID == nil {
		t.Fatalf("tenant get %s: unparseable output (%v): %s", name, err, out)
	}
	return *got.Tenant.ID
}

func singleKeyRangeIs(r types.KeyRangeRef, key []byte) bool {
	return bytes.Equal(r.Begin, key) && bytes.Equal(r.End, keyAfterBytes(key))
}

// C++ fences late delivery in the original tenant before returning 1021
// (NativeAPI.actor.cpp:6306-6344, 6730-6750), including write-only commits (:6858).
func TestTenantCommitUnknownResult_FenceBlocksLateCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		readFirst bool // read the key first: conflict ranges intersect, no self-conflict key
	}{
		{name: "write_only"},
		{name: "read_modify_write", readFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			tenantID := createTestTenant(t, ctx, fmt.Sprintf("fence_%s_%d", tc.name, time.Now().UnixNano()))
			prefix := tenantPrefix(tenantID)

			d := newCommitHoldDialer()
			t.Cleanup(d.closeAll)
			db := newTestDatabase(t, ctx, sharedClusterFile, d.dial)
			t.Cleanup(func() { db.Close() })

			key := []byte("fence-key")
			tenantTx := func() *Transaction {
				tx := db.CreateTransaction()
				tx.SetTenantId(tenantID)
				return tx
			}
			seed := tenantTx()
			seed.Set(key, []byte("v0"))
			if err := seed.Commit(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			d.mu.Lock()
			d.commitTokens = map[transport.UID]bool{}
			for _, p := range db.db.getCommitProxies() {
				d.commitTokens[p.Token] = true
			}
			nTokens := len(d.commitTokens)
			d.mu.Unlock()
			if nTokens == 0 {
				t.Fatal("no commit proxies known after a successful commit")
			}

			tx := tenantTx()
			if tc.readFirst {
				if _, err := tx.Get(ctx, key); err != nil {
					t.Fatalf("read: %v", err)
				}
			} else if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatalf("GRV: %v", err)
			}
			tx.Set(key, []byte("late"))

			d.armed.Store(true)
			err := tx.Commit(ctx)
			var fe *wire.FDBError
			if !errors.As(err, &fe) || fe.Code != 1021 {
				t.Fatalf("commit with its connection killed: got %v, want commit_unknown_result (1021)", err)
			}

			held, after, ok := d.commitsAfterHold()
			if !ok {
				t.Fatal("the original commit request was never withheld")
			}
			if held.TenantInfo.TenantId != tenantID {
				t.Fatalf("withheld commit tenant = %d, want %d", held.TenantInfo.TenantId, tenantID)
			}
			// The key both of the original's conflict sets share is the one the
			// fence must conflict on, in the tenant's keyspace.
			fenceKey := append(bytes.Clone(prefix), key...)
			if !tc.readFirst {
				scPrefix := append(bytes.Clone(prefix), selfConflictPrefix...)
				var sc []byte
				for _, r := range held.Transaction.ReadConflictRanges {
					if bytes.HasPrefix(r.Begin, scPrefix) && len(r.Begin) == len(scPrefix)+16 && singleKeyRangeIs(r, r.Begin) {
						sc = bytes.Clone(r.Begin)
					}
				}
				if sc == nil {
					t.Fatalf("write-only tenant commit carries no self-conflict read range under the tenant prefix: reads=%x", held.Transaction.ReadConflictRanges)
				}
				var inWrites bool
				for _, r := range held.Transaction.WriteConflictRanges {
					inWrites = inWrites || singleKeyRangeIs(r, sc)
				}
				if !inWrites {
					t.Fatalf("self-conflict key %x is not in the write conflict ranges: %x", sc, held.Transaction.WriteConflictRanges)
				}
				fenceKey = sc
			}

			if len(after) == 0 {
				t.Fatal("commit_unknown_result was returned with no fence commit sent")
			}
			for i, f := range after {
				if f.TenantInfo.TenantId != tenantID {
					t.Errorf("fence commit %d runs in tenant %d, want %d", i, f.TenantInfo.TenantId, tenantID)
				}
				if len(f.Transaction.Mutations) != 0 {
					t.Errorf("fence commit %d carries %d mutations, want none", i, len(f.Transaction.Mutations))
				}
				rs, ws := f.Transaction.ReadConflictRanges, f.Transaction.WriteConflictRanges
				if len(rs) != 1 || len(ws) != 1 || !singleKeyRangeIs(rs[0], fenceKey) || !singleKeyRangeIs(ws[0], fenceKey) {
					t.Errorf("fence commit %d conflict ranges reads=%x writes=%x, want exactly [%x] in both", i, rs, ws, fenceKey)
				}
			}

			reply, err := d.release(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Version expiry is also safe; the wire assertions above independently
			// require the fence even when a busy server ages this request out.
			if _, verdict := parseCommitOutcome(reply); !errors.As(verdict, &fe) || (fe.Code != 1020 && fe.Code != 1007) {
				t.Fatalf("the original commit, delivered after commit_unknown_result: got %v, want not_committed (1020) or transaction_too_old (1007)", verdict)
			}

			check := tenantTx()
			got, err := check.Get(ctx, key)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if string(got) != "v0" {
				t.Fatalf("tenant key = %q after the late delivery, want the seeded %q", got, "v0")
			}
		})
	}
}

// A deleted tenant cannot commit the original either (NativeAPI.actor.cpp:6332-6335).
func TestCommitDummyTransaction_TenantNotFoundEndsFence(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	const missing int64 = 1 << 40 // far above any tenant this cluster allocates
	probe := db.CreateTransaction()
	probe.SetTenantId(missing)
	probe.Set([]byte("k"), []byte("v"))
	var fe *wire.FDBError
	if err := probe.Commit(ctx); !errors.As(err, &fe) || fe.Code != 2131 {
		t.Fatalf("commit in an absent tenant: got %v, want tenant_not_found (2131)", err)
	}

	tx := db.CreateTransaction()
	tx.SetTenantId(missing)
	tx.Set([]byte("k"), []byte("v"))
	tx.addReadConflictForKey([]byte("k"))
	if err := tx.commitDummyTransaction(ctx); err != nil {
		t.Fatalf("fence in a deleted tenant: got %v, want success (the original cannot commit)", err)
	}
}

// Fence errors escape tryCommit; retryable errors must complete a fresh dummy first.
func TestTenantCommitUnknownResult_FenceErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		original  int
		fence     int
		want      int
		wantCalls int64
	}{
		{name: "nonretryable", original: 1021, fence: 2000, want: 2000, wantCalls: 2},
		{name: "cluster_changed_nonretryable", original: 1039, fence: 2000, want: 2000, wantCalls: 2},
		{name: "conflict_retry", original: 1021, fence: 1020, want: 1021, wantCalls: 3},
		{name: "unknown_retry", original: 1021, fence: 1021, want: 1021, wantCalls: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			tenantID := createTestTenant(t, ctx, fmt.Sprintf("fence_errors_%s_%d", tc.name, time.Now().UnixNano()))
			d := newSimDialer()
			db := newTestDatabase(t, ctx, sharedClusterFile, d.dial)
			t.Cleanup(func() { db.Close() })
			tx := db.CreateTransaction()
			tx.SetTenantId(tenantID)
			if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatal(err)
			}
			tx.Set([]byte("k"), []byte("v"))
			var calls atomic.Int64
			d.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
				if !isCommitReplyBody(body) {
					return body, false
				}
				code := 0
				switch calls.Add(1) {
				case 1:
					code = tc.original
				case 2:
					code = tc.fence
				}
				if code != 0 {
					return (&types.ErrorOrError{ErrorCode: uint16(code)}).MarshalFDB(), false
				}
				return body, false
			})
			d.armAll()
			err := tx.Commit(ctx)
			var fe *wire.FDBError
			if !errors.As(err, &fe) || fe.Code != tc.want {
				t.Fatalf("commit after fence error %d = %v, want %d", tc.fence, err, tc.want)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("commit replies = %d, want %d (original + dummy attempts)", got, tc.wantCalls)
			}
		})
	}
}

// C++ propagates fence failure instead of reporting an unfenced 1021
// (NativeAPI.actor.cpp:6341, 6749-6750).
func TestCommit_FenceFailureNeverReportsUnknownResult(t *testing.T) {
	t.Parallel()
	db := newTestDatabaseStub().db
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	k := []byte("k")
	kr := []KeyRange{{Begin: k, End: keyAfterBytes(k)}}
	input := &commitInput{db: db, readVersion: 1, tenantID: NoTenantID, readConflicts: kr, writeConflicts: kr}
	_, err := input.commitNative(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("commit whose fence could not run: got %v, want the fence's own error (context.DeadlineExceeded), never 1021", err)
	}
}
