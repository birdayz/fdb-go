package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// simtransport_test.go is the consolidated frame-level fault-injection harness —
// the client/wire analog of FDB's Sim2/BUGGIFY (RFC-118). One proxy-frame loop
// (simConn) parametrized by a per-frame callback subsumes the previously bespoke
// wrongShardConn (replace a reply) and dropReplyConn (drop a reply); the callback
// owns ALL targeting (it closes over the non-PING frame index) and ALL mutation
// (replace / rewrite / drop / pass-through), so a new fault is a new closure, not
// a new conn type. faultConn (read-EOF, a transport-teardown fault, not a frame
// rewrite) stays in fault_test.go.

// frameIntercept runs on each non-PING reply frame in arrival order while the
// connection is armed. idx is the count of non-PING reply frames seen on THIS
// connection since it was armed (PINGs pass through untouched and never advance
// idx — RFC-118 impl contract (a), so a stray keepalive can't shift targeting).
// It returns the body to forward (possibly rewritten) and whether to drop the
// frame entirely. A nil intercept, or an unarmed conn, passes everything through.
type frameIntercept func(idx int, token transport.UID, body []byte) (newBody []byte, drop bool)

// simConn proxies server→client frames through an io.Pipe (the proven
// wrongShardConn pattern: client→server Write passes straight through the
// embedded net.Conn; only the Read direction — server replies — is intercepted).
// The intercept is owned by the simDialer and shared across its conns; arming is
// per-conn so a test can fault exactly one storage server's connection.
type simConn struct {
	net.Conn
	pr    *io.PipeReader
	d     *simDialer
	armed atomic.Bool
}

func newSimConn(c net.Conn, d *simDialer) *simConn {
	pr, pw := io.Pipe()
	s := &simConn{Conn: c, pr: pr, d: d}
	go s.proxyLoop(pw)
	return s
}

func (s *simConn) Read(b []byte) (int, error) { return s.pr.Read(b) }

func (s *simConn) proxyLoop(pw *io.PipeWriter) {
	defer pw.Close()

	// Forward the ConnectPacket verbatim before entering frame mode.
	var cpBuf [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(s.Conn, cpBuf[:]); err != nil {
		pw.CloseWithError(err)
		return
	}
	if _, err := pw.Write(cpBuf[:]); err != nil {
		return
	}

	pingToken := transport.WellKnownToken(transport.WLTokenPingPacket)
	idx := 0
	for {
		token, body, err := transport.ReadFrame(s.Conn, false)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if token != pingToken && s.armed.Load() {
			if fn := s.d.getIntercept(); fn != nil {
				newBody, drop := fn(idx, token, body)
				idx++
				if drop {
					continue // swallow the reply (dropReplyConn behavior)
				}
				body = newBody
			}
		}
		if err := transport.WriteFrame(pw, token, body, false); err != nil {
			return
		}
	}
}

// simDialer creates simConns and owns the (shared) intercept. arming is by addr
// so a test faults only the storage server's connection while the coordinator /
// proxy connections (locate, GRV) pass through cleanly — RFC-118's fix for the
// armAll() cross-connection corruption (a type-specific rewriter like the Gap-3
// More-flip must not touch a GetKeyServerLocations reply).
type simDialer struct {
	mu        sync.Mutex
	conns     map[string][]*simConn // by dial addr
	armedAddr map[string]bool       // addrs to arm, incl. conns dialed later
	intercept atomic.Pointer[frameIntercept]
}

func newSimDialer() *simDialer {
	return &simDialer{conns: map[string][]*simConn{}, armedAddr: map[string]bool{}}
}

func (d *simDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	sc := newSimConn(c, d)
	d.mu.Lock()
	d.conns[addr] = append(d.conns[addr], sc)
	if d.armedAddr[addr] {
		sc.armed.Store(true)
	}
	d.mu.Unlock()
	return sc, nil
}

// setIntercept installs the per-frame callback. Safe to call before arming
// (conns read it lock-free at frame time); set it before armAddr/armAll.
func (d *simDialer) setIntercept(fn frameIntercept) { d.intercept.Store(&fn) }

func (d *simDialer) getIntercept() frameIntercept {
	if p := d.intercept.Load(); p != nil {
		return *p
	}
	return nil
}

// armAddr arms only the connections dialed to addr (and any dialed later, e.g. a
// re-dial after the storage conn is torn down), leaving coordinator/proxy conns
// untouched. Discover addr via db.db.locCache.locate(...).Servers[0].Address.
func (d *simDialer) armAddr(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armedAddr[addr] = true
	for _, c := range d.conns[addr] {
		c.armed.Store(true)
	}
}

// armAll arms every connection. Used by the drop-reply timeout tests, where the
// intercept drops ALL replies (no type-specific rewrite, so cross-conn is moot)
// and the test has pre-warmed cache + read version so only the target read frames
// flow during the armed window.
func (d *simDialer) armAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for addr := range d.conns {
		d.armedAddr[addr] = true
		for _, c := range d.conns[addr] {
			c.armed.Store(true)
		}
	}
}

// newSimTestDB starts an FDB container (via startProxyFDB — external coordinators,
// container-internal cluster identity, the shape a frame-proxying dialer needs)
// and returns a Database dialing through a simDialer with no intercept set yet.
func newSimTestDB(t *testing.T, ctx context.Context) (*Database, *simDialer) {
	t.Helper()
	connectCF := startProxyFDB(t, ctx)
	d := newSimDialer()
	db := newTestDatabase(t, ctx, connectCF, d.dial)
	t.Cleanup(func() { db.Close() })
	return db, d
}

// storageAddrFor returns the storage server address serving key (Servers[0]),
// after the location cache has been warmed by a prior read. A test arms exactly
// this address (armAddr) so the fault hits only the storage connection while the
// coordinator/proxy connections — locate (GetKeyServerLocations) and GRV — pass
// through cleanly during the relocate-and-retry.
func storageAddrFor(t *testing.T, db *Database, ctx context.Context, key []byte) string {
	t.Helper()
	loc, err := db.db.locCache.locate(db.db, ctx, key, NoTenantID, types.SpanContext{})
	if err != nil {
		t.Fatalf("locate %q: %v", key, err)
	}
	if len(loc.Servers) == 0 {
		t.Fatalf("locate %q: no servers", key)
	}
	return loc.Servers[0].Address
}

// injectPenalty is a representative storage-server penalty for injected replies.
// Real FDB sets reply.penalty = getPenalty() (a load metric); the exact value is
// immaterial to these tests (the wrong-shard/future-version arms key off the
// error code, not the penalty), so a fixed plausible value keeps it faithful.
const injectPenalty = 1.0

// --- faithful injection primitives (RFC-118) ---

// inlineErrorReply builds the wire-faithful read-error frame that real FDB
// delivers: an ErrorOr<reply>(tag=value) — a SUCCESS union — whose nested reply
// carries the error in the inline LoadBalancedReply.error field (not the root
// ErrorOr), byte-shape-identical to sendErrorWithPenalty (storageserver.actor.cpp,
// 7.3.75; Reply{}; reply.error=err; reply.penalty=penalty; promise.send(reply)).
// The frame is type-agnostic: the inline-error slots (Penalty@0, HasError@1,
// Error@2) are identical across GetValueReply / GetKeyReply / GetKeyValuesReply, so
// one frame is decoded identically by all three production parsers via
// wire.ReadInlineReplyError (readpath.go:341/948/1007). See
// types.MarshalErrorOrInlineError + TestMarshalErrorOrInlineError_RoundTrip.
func inlineErrorReply(code uint16, penalty float64) []byte {
	return types.MarshalErrorOrInlineError(code, penalty)
}

// partialBatchReply decodes a real ErrorOr<GetKeyValuesReply> success frame (the
// envelope the storage server actually sends — NOT a bare FakeRoot reply), TRUNCATES
// it to the first `keep` rows, sets More=true, and re-emits the same envelope —
// faithfully simulating a server that chose to return a partial batch (legitimate
// behavior), which forces the getRange inner continuation loop without a
// knob-dependent 80KB scan. Truncating (not just flipping More) is load-bearing: it
// leaves real rows for the continuation, so the test exercises the no-DROP dimension
// (a buggy resume that skips the remainder is caught), not only no-dup. Returns the
// body unchanged if it is not a success ErrorOr<reply>, or if it already has ≤ keep
// rows (defensive — should not happen on an armAddr'd storage conn with > keep keys).
//
// The decode→truncate→re-encode does NOT re-emit the reply's Arena field (the
// generated GetKeyValuesReply.writeToBuffer omits it); harmless because the Go
// client never reads Arena (a server-side memory hint) — rows come from Data.
func partialBatchReply(body []byte, keep int) []byte {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(body, &r); err != nil {
		return body
	}
	var reply types.GetKeyValuesReply
	reply.UnmarshalFromReader(&r)
	kvs := types.ParseKeyValueRefStringVector(reply.Data)
	if len(kvs) <= keep {
		return body
	}
	reply.Data = packKeyValueRefVector(kvs[:keep])
	reply.More = true
	return types.MarshalErrorOrValueGetKeyValuesReply(&reply)
}

// packKeyValueRefVector re-encodes a KeyValueRef slice in the VecSerStrategy::String
// layout that types.ParseKeyValueRefStringVector decodes: a uint32 LE count, then per
// element a uint32 LE key length + key bytes + uint32 LE value length + value bytes.
// Used to truncate a reply's Data to a partial batch (pinned by TestPartialBatchReply_RoundTrip).
func packKeyValueRefVector(kvs []types.KeyValueRef) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(len(kvs)))
	var n [4]byte
	for _, kv := range kvs {
		binary.LittleEndian.PutUint32(n[:], uint32(len(kv.Key)))
		buf = append(buf, n[:]...)
		buf = append(buf, kv.Key...)
		binary.LittleEndian.PutUint32(n[:], uint32(len(kv.Value)))
		buf = append(buf, n[:]...)
		buf = append(buf, kv.Value...)
	}
	return buf
}

// replaceFirst returns an intercept that replaces ONLY the first armed non-PING
// reply frame with newBody, passing everything after through (so the post-fault
// retry succeeds). The canonical one-shot wrong-shard / future-version injection.
func replaceFirst(newBody []byte) frameIntercept {
	return func(idx int, _ transport.UID, body []byte) ([]byte, bool) {
		if idx == 0 {
			return newBody, false
		}
		return body, false
	}
}

// dropAll returns an intercept that drops every armed non-PING reply (the
// dropReplyConn behavior — the client's RPC reply timer fires deterministically).
func dropAll() frameIntercept {
	return func(int, transport.UID, []byte) ([]byte, bool) { return nil, true }
}

// TestPartialBatchReply_RoundTrip pins the Gap-3 partialBatchReply rewriter BEFORE
// the Gap-3 integration test relies on it (RFC-118 impl contract (c)): a re-marshal
// that corrupted the envelope or the Data re-pack would surface here as a clear
// equality failure, not as a confusing dup/drop downstream. It must accept a real
// ErrorOr<reply> SUCCESS frame (a bare FakeRoot reply would mask the envelope-decode
// bug), keep exactly the first `keep` rows, set More=true, and preserve the other
// scalar fields.
func TestPartialBatchReply_RoundTrip(t *testing.T) {
	t.Parallel()
	rows := []types.KeyValueRef{
		{Key: []byte("k0"), Value: []byte("v0")},
		{Key: []byte("k1"), Value: []byte("v1\x00\xff")},
		{Key: []byte("k2"), Value: []byte("v2")},
	}
	orig := &types.GetKeyValuesReply{
		Penalty: 1.5,
		Data:    packKeyValueRefVector(rows),
		Version: 0x123456789a,
		More:    false,
		Cached:  true,
	}
	// packKeyValueRefVector must round-trip through the production parser.
	if got := types.ParseKeyValueRefStringVector(orig.Data); len(got) != 3 {
		t.Fatalf("packed vector parses to %d rows, want 3", len(got))
	}

	// Input is an ErrorOr<reply> SUCCESS frame — the shape a storage server sends.
	in := types.MarshalErrorOrValueGetKeyValuesReply(orig)
	var r wire.Reader
	if err := wire.ReadErrorOrInto(partialBatchReply(in, 2), &r); err != nil {
		t.Fatalf("truncated frame is not a success ErrorOr<reply>: %v", err)
	}
	var got types.GetKeyValuesReply
	got.UnmarshalFromReader(&r)
	if !got.More {
		t.Error("More was not flipped to true")
	}
	kvs := types.ParseKeyValueRefStringVector(got.Data)
	if len(kvs) != 2 {
		t.Fatalf("truncated to %d rows, want 2", len(kvs))
	}
	for i := 0; i < 2; i++ {
		if !bytes.Equal(kvs[i].Key, rows[i].Key) || !bytes.Equal(kvs[i].Value, rows[i].Value) {
			t.Errorf("row %d: got %q=%q, want %q=%q", i, kvs[i].Key, kvs[i].Value, rows[i].Key, rows[i].Value)
		}
	}
	if got.Version != orig.Version {
		t.Errorf("Version changed: got %#x, want %#x", got.Version, orig.Version)
	}
	if got.Penalty != orig.Penalty {
		t.Errorf("Penalty changed: got %v, want %v", got.Penalty, orig.Penalty)
	}
	if got.Cached != orig.Cached {
		t.Errorf("Cached changed: got %v, want %v", got.Cached, orig.Cached)
	}
	if got.HasError {
		t.Error("spurious inline error after re-marshal")
	}

	// Defensive early-return: keep >= the row count returns the body unchanged
	// (nothing to truncate). Pins the len(kvs) <= keep arm of partialBatchReply.
	for _, keep := range []int{3, 5} {
		if out := partialBatchReply(in, keep); !bytes.Equal(out, in) {
			t.Errorf("partialBatchReply(in, %d) with 3 rows should return body unchanged, got %d bytes (want %d)", keep, len(out), len(in))
		}
	}
}

// TestSimRangeWrongShardMidScan is RFC-118 Gap 3: a wrong_shard_server (1001)
// injected on the SECOND GetKeyValues frame of a getRange scan (mid-continuation),
// forward and reverse. The first frame is forced to More=true (a partial batch —
// legitimate server behavior) so the client enters the inner continuation loop
// with the shard begin/end already NARROWED past frame 0's rows (readpath.go:762);
// the injected 1001 on the continuation then drives the relocate, which must
// invalidate only the narrowed range (:704) and resume from there — producing the
// full result set with NO duplicated and NO dropped key.
func TestSimRangeWrongShardMidScan(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db, sd := newSimTestDB(t, ctx)

			pfx := t.Name() + "_"
			const n = 5
			want := map[string]string{}
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				for i := 0; i < n; i++ {
					k := fmt.Sprintf("%s%02d", pfx, i)
					v := fmt.Sprintf("v%d", i)
					tx.Set([]byte(k), []byte(v))
					want[k] = v
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			begin := []byte(pfx)
			end := append(append([]byte{}, pfx...), 0xFF)
			read := func(tx *Transaction) ([]KeyValue, bool, error) {
				if reverse {
					return tx.GetRangeReverse(ctx, begin, end, 100)
				}
				return tx.GetRange(ctx, begin, end, 100)
			}

			// Warm the cache.
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				kvs, _, err := read(tx)
				return kvs, err
			}); err != nil {
				t.Fatalf("warm: %v", err)
			}
			rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
			if err != nil {
				t.Fatalf("GRV: %v", err)
			}

			// frame 0: force More=true (forces a continuation); frame 1: inline 1001
			// on the continuation; rest pass through (the post-relocate retry).
			sd.setIntercept(func(idx int, _ transport.UID, body []byte) ([]byte, bool) {
				switch idx {
				case 0:
					// Partial batch: keep 2 of n rows + More=true, so the
					// continuation genuinely carries the remaining n-2 rows (a buggy
					// resume that drops them is caught, not just a dup).
					return partialBatchReply(body, 2), false
				case 1:
					return inlineErrorReply(1001, injectPenalty), false
				default:
					return body, false
				}
			})
			// Wrong-shard invalidates the location cache, NOT the connection pool —
			// so the post-fault retry reuses THIS simConn at idx=2 (the "default:
			// pass-through" arm), not a fresh idx=0 conn that would re-trigger the
			// partial→1001 loop. The arm-by-addr keeps the rewrites off the
			// coordinator/proxy (locate) conns the relocate touches.
			sd.armAddr(storageAddrFor(t, db, ctx, begin))

			tx := db.CreateTransaction()
			tx.SetReadVersion(rv)
			kvs, _, err := read(tx)
			if err != nil {
				t.Fatalf("GetRange (reverse=%v) after mid-scan wrong-shard: %v", reverse, err)
			}
			// No drop and no duplicate: exactly n distinct keys, each its seeded value.
			seen := map[string]bool{}
			for _, kv := range kvs {
				k := string(kv.Key)
				if seen[k] {
					t.Errorf("duplicate key %q in result", k)
				}
				seen[k] = true
				if want[k] != string(kv.Value) {
					t.Errorf("kv %q: got %q, want %q", k, kv.Value, want[k])
				}
			}
			if len(kvs) != n {
				t.Fatalf("got %d kvs, want %d (dup or drop across the mid-scan relocate)", len(kvs), n)
			}
		})
	}
}

// TestSimInlineFutureVersion_QueueModelBackoff is RFC-118 Gap 4: an inline
// future_version (1009) / process_behind (1037) reply on a read advances the
// QueueModel's per-address backoff. C++ loadBalance reads errCode from the inline
// field, sets futureVersion (LoadBalance.actor.h:348), and threads it to
// QueueModel::endRequest, which grows futureVersionBackoff and sets failedUntil
// (QueueModel.cpp:36-46). Go wires this at readpath.go:323/548/885 via
// endRequestFull(..., isFutureVersionOrProcessBehind(err)). The read SURFACES the
// code (getValueImpl/getKey/getRange treat 1009/1037 as "other error" → bubble up,
// readpath.go:716), so the assertion is on QueueModel state (the cause) — a
// single-SS testcontainer cannot observe re-selection (the effect; pinned at
// loadbalance_test.go). Canonical literals (anti-self-confirming).
func TestSimInlineFutureVersion_QueueModelBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code uint16
		read func(ctx context.Context, tx *Transaction, key []byte) error
	}{
		{"getValue/1009", 1009, func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.Get(ctx, key)
			return err
		}},
		{"getKey/1009", 1009, func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.GetKey(ctx, key, false, 1)
			return err
		}},
		{"getKeyValues/1009", 1009, func(ctx context.Context, tx *Transaction, key []byte) error {
			_, _, err := tx.GetRange(ctx, key, append(append([]byte{}, key...), 0xFF), 100)
			return err
		}},
		{"getValue/1037", 1037, func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.Get(ctx, key)
			return err
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db, sd := newSimTestDB(t, ctx)

			key := []byte(t.Name() + "_key")
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Set(key, []byte("v"))
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// Warm cache + reset this address's backoff via a successful read.
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				return tx.Get(ctx, key)
			}); err != nil {
				t.Fatalf("warm: %v", err)
			}
			rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
			if err != nil {
				t.Fatalf("GRV: %v", err)
			}
			addr := storageAddrFor(t, db, ctx, key)
			before := nowSeconds()
			sd.setIntercept(replaceFirst(inlineErrorReply(tc.code, injectPenalty)))
			sd.armAddr(addr)

			tx := db.CreateTransaction()
			tx.SetReadVersion(rv)
			readErr := tc.read(ctx, tx, key)
			// The injected inline code is surfaced (not relocate/timeout-retried).
			var fe *wire.FDBError
			if !errors.As(readErr, &fe) || fe.Code != int(tc.code) {
				t.Fatalf("read err = %v, want FDBError %d surfaced from the inline channel", readErr, tc.code)
			}
			// The read-path wiring fired: the QueueModel recorded the backoff. Copy
			// the fields out UNDER the lock — prod reads them under queueModel.mu
			// (loadbalance.go), so the test must too (Torvalds lock-discipline).
			db.db.queueModel.mu.Lock()
			d := db.db.queueModel.servers[addr]
			var failedUntil, backoff float64
			found := d != nil
			if found {
				failedUntil, backoff = d.failedUntil, d.futureVersionBackoff
			}
			db.db.queueModel.mu.Unlock()
			if !found {
				t.Fatalf("no QueueModel entry for storage addr %s", addr)
			}
			if failedUntil <= before {
				t.Errorf("failedUntil = %f, want > %f (future_version must advance the backoff)", failedUntil, before)
			}
			if wantBackoff := futureVersionInitialBackoff * futureVersionBackoffGrowth; backoff != wantBackoff {
				t.Errorf("futureVersionBackoff = %f, want %f (first future_version doubles the initial backoff)", backoff, wantBackoff)
			}
		})
	}
}
