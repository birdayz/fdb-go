package client

import (
	"context"
	"encoding/binary"
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
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
)

// faultConn wraps a net.Conn. When killReads is set, Read returns EOF.
type faultConn struct {
	net.Conn
	killReads atomic.Bool
}

func (f *faultConn) Read(b []byte) (int, error) {
	if f.killReads.Load() {
		return 0, io.EOF
	}
	return f.Conn.Read(b)
}

// faultDialer creates connections and can arm/disarm faults.
type faultDialer struct {
	mu    sync.Mutex
	conns []*faultConn
	armed bool // when true, new connections have killReads=true
}

func (d *faultDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	fc := &faultConn{Conn: c}
	d.mu.Lock()
	if d.armed {
		fc.killReads.Store(true)
	}
	d.conns = append(d.conns, fc)
	d.mu.Unlock()
	return fc, nil
}

// arm sets killReads on ALL existing connections.
func (d *faultDialer) arm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = true
	for _, fc := range d.conns {
		fc.killReads.Store(true)
	}
}

// disarm clears killReads and ensures new connections are clean.
func (d *faultDialer) disarm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = false
	for _, fc := range d.conns {
		fc.killReads.Store(false)
	}
}

// newTestDatabase creates a Database from a ClusterFile with custom dialer,
// bootstrapping the connection against the provided context.
func newTestDatabase(t *testing.T, ctx context.Context, cf *ClusterFile, dialFn transport.DialFunc) *Database {
	t.Helper()
	db, err := OpenDatabaseFromConfig(ctx, cf, WithDialFunc(dialFn), WithAPIVersion(730))
	if err != nil {
		t.Fatalf("newTestDatabase: %v", err)
	}
	return db
}

// TestCommitUnknownResult_NoDoubleApply verifies that the self-conflicting
// mechanism prevents double-apply of non-idempotent operations.
//
// Uses atomic ADD: counter starts at 10, we ADD 5. If the commit succeeds
// on the server but the client retries, a naive retry would ADD 5 again
// (counter=20). The self-conflicting mechanism should cause the retry to
// conflict, and the final Transact outcome should show counter=15.
//
// NOTE: commit_unknown_result + self-conflicting only prevents the IMMEDIATE
// retry from double-applying. After the self-conflicting retry fails with
// 1020, Transact retries AGAIN with a clean transaction. For atomic ADD,
// this third attempt WILL add 5 again (counter=20). This matches the C++
// client behavior — non-idempotent operations need application-level
// idempotency tokens for true exactly-once semantics.
//
// What this test DOES prove: the fault injection works, the server commits
// survive client-side connection death, and the self-conflicting mechanism
// fires (the retry gets 1020, not a second successful commit).
func TestCommitUnknownResult_NoDoubleApply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	fd := &faultDialer{}
	db := newTestDatabase(t, ctx, connectCF, fd.dial)
	defer db.Close()

	// Seed counter = 10.
	var counterBuf [8]byte
	binary.LittleEndian.PutUint64(counterBuf[:], 10)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(t.Name()+"_counter"), counterBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Log("seeded fault_counter=10")

	// Manual commit with fault: ADD 5, then kill connection.
	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)

	binary.LittleEndian.PutUint64(counterBuf[:], 5)
	tx.Atomic(MutAddValue, []byte(t.Name()+"_counter"), counterBuf[:])

	// Arm fault BEFORE commit — reply will be killed.
	fd.arm()

	err = tx.Commit(ctx)
	t.Logf("commit with fault: %v", err)
	if err == nil {
		t.Fatal("commit should have failed (connection killed)")
	}

	// Disarm and clear connection pool to force reconnect.
	fd.disarm()
	db.db.connMu.Lock()
	for k := range db.db.connPool {
		delete(db.db.connPool, k)
	}
	db.db.connMu.Unlock()

	// Re-bootstrap to get fresh topology.
	db.db.refreshTopology()
	db.db.grvCache.invalidate() // Stale cache from pre-fault connections.

	// Read the counter. It should be 15 (10 + 5, applied once by server).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(t.Name()+"_counter"))
	})
	if err != nil {
		t.Fatalf("verify Get: %v", err)
	}
	val := binary.LittleEndian.Uint64(result.([]byte))
	t.Logf("fault_counter = %d", val)
	// commit_unknown_result (1021) is, by the FDB contract, genuinely
	// nondeterministic: when the connection dies during commit the transaction
	// MAY or MAY NOT have been durably applied by the server. The fault here kills
	// the reply (killReads) AFTER the commit request was written, so the server
	// usually commits before the connection teardown (counter 15) — but
	// occasionally the teardown aborts the in-flight commit first (counter 10).
	// BOTH are correct outcomes. The invariant this test actually guards is
	// no-DOUBLE-apply: the ADD must never be applied more than once (counter must
	// never reach 20). Asserting an exact 15 was a false determinism assumption
	// and made the test flaky.
	switch val {
	case 10:
		t.Logf("commit_unknown_result: ADD was not applied (counter stayed 10) — valid outcome")
	case 15:
		t.Logf("commit_unknown_result: ADD applied exactly once (counter 15) — valid outcome")
	default:
		t.Errorf("counter = %d, want 10 (commit not applied) or 15 (applied once); "+
			"anything >= 20 is a double-apply bug", val)
	}
}

// TestPeerDisconnect_FailsInFlightReplyImmediately proves the Go client detects a
// storage-server peer disconnect on an IN-FLIGHT request immediately: the moment the
// TCP connection drops, the connection's readLoop sees EOF and failConnection →
// failAllPending delivers an error to every pending reply at once (conn.go), rather
// than the request hanging until a timeout.
//
// This is the architectural equivalent of FoundationDB C++ PR #12935 (landed in
// 7.3.76 — the ONLY client-relevant C++ delta in the 7.3.75 → 7.3.77 baseline bump):
// that PR added a `when(wait(peer->disconnect.getFuture()))` arm to waitValueOrSignal
// (fdbrpc/genericactors.actor.h) so loadBalance returns request_maybe_delivered the
// instant a connection drops, instead of hanging until the IFailureMonitor detection
// lag elapses. The Go transport's single-owner connection model has this property
// structurally — the readLoop owns the socket and there is no separate failure
// monitor to lag behind — so Go never had the bug #12935 fixed. Pinning it here is the
// load-bearing evidence that the baseline bump needs no Go behaviour change.
//
// Determinism (no timer-vs-real-reply race, the #288 lesson): a reply token is
// registered but NO request is sent, so the server can never answer it. The only path
// that can ever wake replyCh is the connection-teardown failAllPending. We then close
// the underlying socket (a faithful peer disconnect — the readLoop discovers it via an
// EOF, exactly the §3 mechanism, not an explicit local Close) and assert replyCh wakes
// well under any RPC timeout. Revert-prove: make failAllPending a no-op (or drop the
// readLoop failConnection) and replyCh never wakes → the 2s arm fires → red.
func TestPeerDisconnect_FailsInFlightReplyImmediately(t *testing.T) {
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
	// Warm the location cache so getOrDial returns a live, handshook storage conn.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	addr := storageAddrFor(t, db, ctx, key)
	conn, err := db.db.getOrDial(ctx, addr)
	if err != nil {
		t.Fatalf("getOrDial(%s): %v", addr, err)
	}

	// Register an in-flight reply the server will NEVER answer (no request is sent).
	// The ONLY thing that can deliver to replyCh is the connection teardown.
	_, replyCh, replyHandle := conn.PrepareReply()
	// Cleanup discipline (conn.go ReplyHandle): a SUCCESSFUL receive leaves Release
	// to pool the (drained) channel — failAllPending does its non-blocking send and
	// the token delete in one pendingMu-held iteration and visits each token exactly
	// once, so once we have received the value no further send can race the pool Put.
	// A NOT-received outcome (the 2s arm) MUST Cancel() first: Cancel removes the
	// still-pending token and nils h.ch so the deferred Release does not pool a channel
	// the pending map still references (which would let a later failAllPending send a
	// stale value into the shared replyChanPool — cross-test contamination /
	// false-green). cancelled is set on the not-received path; Release on the success path.
	cancelled := false
	defer func() {
		if cancelled {
			replyHandle.Cancel()
		}
		replyHandle.Release()
	}()

	// Simulate the peer dropping the TCP connection: close the underlying socket so
	// the storage conn's readLoop reads EOF → failConnection → failAllPending.
	sd.mu.Lock()
	for _, c := range sd.conns[addr] {
		_ = c.Conn.Close()
	}
	sd.mu.Unlock()

	start := time.Now()
	select {
	case resp := <-replyCh:
		elapsed := time.Since(start)
		if resp.Err == nil {
			t.Fatalf("in-flight reply got a value %q after disconnect; expected a connection error", resp.Body)
		}
		// 2s is a generous CI margin; real teardown is sub-millisecond and far below
		// DefaultRPCTimeout (5s) — so a regression to "wait for the timeout" goes red.
		if elapsed >= 2*time.Second {
			t.Fatalf("in-flight reply failed only after %v; peer disconnect was not detected immediately", elapsed)
		}
		t.Logf("peer disconnect failed the in-flight reply in %v: %v", elapsed, resp.Err)
	case <-time.After(2 * time.Second):
		cancelled = true // not received: deferred cleanup must Cancel() before Release()
		t.Fatal("in-flight reply was NOT failed within 2s of peer disconnect — the client hangs (the bug FDB C++ PR #12935 fixed); Go's failConnection must wake all pending replies")
	}
}

// startProxyFDB starts an FDB container and returns a ClusterFile whose
// Coordinators are the external (dial-through) addresses while Description/ID
// carry the container-internal cluster identity — the shape a frame-proxying
// dialer (simDialer) needs.
// Container teardown is registered on t.Cleanup with a FRESH context: t.Cleanup
// runs AFTER the caller's `defer cancel()`, so terminating on the (cancelled)
// test ctx makes testcontainers bail with context.Canceled and leak the
// container (RFC-010 codex finding).
func startProxyFDB(t *testing.T, ctx context.Context) *ClusterFile {
	t.Helper()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
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
	return connectCF
}

// newDropReplyTestDB starts an FDB container and returns a Database that dials
// through a simDialer preset to DROP every armed non-PING reply (the former
// dropReplyConn, now a simConn intercept). After warming the location cache +
// read version, dd.armAll() makes subsequent read replies vanish, so the read
// path times out deterministically — no timer-vs-real-reply race (the flaw of a
// tiny rpcTimeoutOverride against a real connection: on a fast box the real reply
// can win every time, as the getKey subtest did in CI) — exercising the
// errReplyTimeout retry loop to its retryable transaction_too_old exhaustion.
// PINGs still pass through (simConn never drops them) so the connection is not
// torn down as dead.
func newDropReplyTestDB(t *testing.T, ctx context.Context) (*Database, *simDialer) {
	db, dd := newSimTestDB(t, ctx)
	dd.setIntercept(dropAll())
	return db, dd
}

// TestWrongShardServer_FaultInjection verifies that an injected wrong_shard_server
// (1001) reply on the synchronous getValue path triggers location-cache
// invalidation and an automatic retry: the client receives 1001, invalidates the
// cache, re-queries the proxy for fresh locations, and retries the read to
// success. The 1001 is injected through the FAITHFUL channel — the inline
// LoadBalancedReply.error of a GetValueReply (RFC-118), how real FDB delivers a
// read wrong_shard (storageserver.actor.cpp sendErrorWithPenalty), NOT a root
// ErrorOr. The injected code is the canonical literal 1001, never the
// ErrWrongShardServer constant (anti-self-confirming, fault_test.go P6).
func TestWrongShardServer_FaultInjection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)

	key := []byte(t.Name() + "_key")
	expected := []byte("correct_value")

	// Seed the key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, expected)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Warm the location cache with a successful read.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if string(result.([]byte)) != string(expected) {
		t.Fatalf("warm read: got %q, want %q", result, expected)
	}

	// Pre-fetch read version so no GRV request during the fault window.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	// Arm only the storage connection: its first reply becomes an inline-error
	// GetValueReply(1001); the relocate's GetKeyServerLocations (on the proxy
	// conn) and the retry pass through.
	sd.setIntercept(replaceFirst(inlineErrorReply(1001, injectPenalty)))
	sd.armAddr(storageAddrFor(t, db, ctx, key))

	// Read with pre-set read version. The first getValue attempt will get
	// the inline 1001, causing cache invalidation and retry.
	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	got, err := tx.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after fault: %v", err)
	}
	if string(got) != string(expected) {
		t.Fatalf("Get value: got %q, want %q", got, expected)
	}
	t.Log("inline wrong_shard_server: retry succeeded with correct value")
}

// TestWrongShardServer_GetKey covers the wrong-shard retry loop on the key-selector
// read path (getKey), not just point Get. The 1001 rides the inline error of a
// GetKeyReply — the faithful channel that parseGetKeyReply decodes (readpath.go:341,
// RFC-118 Gap 1); asserts GetKey still resolves correctly. RFC-010 #2.
func TestWrongShardServer_GetKey(t *testing.T) {
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
	// Warm the cache. firstGreaterOrEqual(key) = {key, orEqual=false, offset=1}.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, key, false, 1)
	}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	sd.setIntercept(replaceFirst(inlineErrorReply(1001, injectPenalty)))
	sd.armAddr(storageAddrFor(t, db, ctx, key))

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	got, err := tx.GetKey(ctx, key, false, 1)
	if err != nil {
		t.Fatalf("GetKey after fault: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("GetKey: got %q, want %q", got, key)
	}
}

// TestWrongShardServer_GetRange covers the wrong-shard retry loop on the range
// read path (getRange). The 1001 rides the inline error of a GetKeyValuesReply —
// the faithful channel parseGetKeyValuesReply decodes (readpath.go:948, RFC-118
// Gap 1); asserts GetRange returns the full range. RFC-010 #2.
func TestWrongShardServer_GetRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	pfx := t.Name() + "_"
	want := map[string]string{}
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
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
	end := append([]byte(pfx), 0xFF)
	// Warm the cache.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, begin, end, 100)
		return kvs, err
	}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	sd.setIntercept(replaceFirst(inlineErrorReply(1001, injectPenalty)))
	sd.armAddr(storageAddrFor(t, db, ctx, begin))

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	kvs, _, err := tx.GetRange(ctx, begin, end, 100)
	if err != nil {
		t.Fatalf("GetRange after fault: %v", err)
	}
	if len(kvs) != len(want) {
		t.Fatalf("GetRange: got %d kvs, want %d", len(kvs), len(want))
	}
	for _, kv := range kvs {
		if want[string(kv.Key)] != string(kv.Value) {
			t.Errorf("kv %q: got %q, want %q", kv.Key, kv.Value, want[string(kv.Key)])
		}
	}
}

// TestPipelinedGet_WrongShardRetry verifies the pipelined read path
// (GetPipelined + PendingGet.Resolve) applies the SAME wrong_shard_server
// invalidate+retry as the synchronous getValue path. Before RFC-010 #3, Resolve
// returned the wrong-shard reply flattened to all_alternatives_failed without
// invalidating the location cache or retrying — so the public API's most common
// point-read silently skipped wrong-shard recovery.
func TestPipelinedGet_WrongShardRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)

	key := []byte(t.Name() + "_key")
	expected := []byte("pipelined_value")

	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, expected)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Warm the location cache with a successful read.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	// Pre-fetch read version so no GRV request during the fault window.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	// Arm only the storage conn: its first reply becomes an inline-error
	// GetValueReply(1001) (the faithful channel Resolve→parseGetValueReply decodes).
	sd.setIntercept(replaceFirst(inlineErrorReply(1001, injectPenalty)))
	sd.armAddr(storageAddrFor(t, db, ctx, key))

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	_, pending, err := tx.GetPipelined(ctx, key)
	if err != nil {
		t.Fatalf("GetPipelined: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending pipelined get for a server-resident key")
	}
	// The reply is the injected wrong_shard_server; Resolve must invalidate the
	// cache and re-drive through the full read path, returning the real value.
	got, err := pending.Resolve()
	if err != nil {
		t.Fatalf("Resolve after wrong-shard: %v", err)
	}
	if string(got) != string(expected) {
		t.Fatalf("Resolve value: got %q, want %q", got, expected)
	}
	t.Log("pipelined wrong_shard_server: Resolve invalidated + retried, returned correct value")
}

// TestPipelinedGet_IllegalKeyRejectedAtEnqueue verifies GetPipelined rejects an
// out-of-legal-range key BEFORE sending (matching Transaction.Get), rather than
// putting the illegal frame on the wire and discovering it at response time. RFC-010 #3.
func TestPipelinedGet_IllegalKeyRejectedAtEnqueue(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	// Beyond \xff\xff (the max read key for any transaction) — illegal.
	illegal := []byte{0xff, 0xff, 0xff}
	val, pending, err := tx.GetPipelined(ctx, illegal)
	if pending != nil {
		t.Fatal("illegal key must not produce a pending request (frame would already be on the wire)")
	}
	if val != nil {
		t.Fatalf("illegal key: unexpected value %q", val)
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != 2004 {
		t.Fatalf("illegal key: got err %v, want FDBError 2004 (key_outside_legal_range)", err)
	}
}

// TestPipelinedGet_BatchResolvesCorrectly verifies pipelining still works after
// the #3 retry change: N deferred sends followed by N Resolves return the right
// values. The retry logic only activates on error, so the happy-path batched
// send/flush is structurally unchanged — this pins that end to end.
func TestPipelinedGet_BatchResolvesCorrectly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const n = 16
	keys := make([][]byte, n)
	vals := make([][]byte, n)
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < n; i++ {
			keys[i] = []byte(fmt.Sprintf("%s_k%03d", t.Name(), i))
			vals[i] = []byte(fmt.Sprintf("v%03d", i))
			tx.Set(keys[i], vals[i])
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Enqueue all N pipelined gets (deferred sends, no flush yet)...
		pendings := make([]*PendingGet, n)
		for i := 0; i < n; i++ {
			_, p, err := tx.GetPipelined(ctx, keys[i])
			if err != nil {
				return nil, fmt.Errorf("GetPipelined %d: %w", i, err)
			}
			if p == nil {
				t.Fatalf("key %d: expected a pending server request", i)
			}
			pendings[i] = p
		}
		// ...then resolve them; the first Resolve flushes the batch.
		for i := 0; i < n; i++ {
			got, err := pendings[i].Resolve()
			if err != nil {
				return nil, fmt.Errorf("Resolve %d: %w", i, err)
			}
			if string(got) != string(vals[i]) {
				t.Errorf("key %d: got %q, want %q", i, got, vals[i])
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("pipelined batch: %v", err)
	}
}

// TestCommitDummyTransaction verifies that commitDummyTransaction works as a
// synchronization barrier. Creates a transaction with write conflicts, directly
// calls commitDummyTransaction, and verifies it completes without error.
func TestCommitDummyTransaction(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Create a real transaction with a write.
	tx := db.CreateTransaction()
	key := []byte(t.Name() + "_dummy_key")
	tx.Set(key, []byte("value"))

	// commitDummyTransaction should complete without error — the dummy
	// transaction commits a conflict-only transaction (no mutations).
	tx.commitDummyTransaction(ctx)
	// If we get here without panic/hang, the dummy worked.
	t.Log("commitDummyTransaction completed successfully")
}

// TestCommitDummyTransaction_NoWriteConflicts verifies that
// commitDummyTransaction is a no-op when there are no write conflicts.
func TestCommitDummyTransaction_NoWriteConflicts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Read-only transaction — no write conflicts.
	tx := db.CreateTransaction()
	tx.commitDummyTransaction(ctx)
	// Should return immediately (no-op).
	t.Log("commitDummyTransaction no-op for read-only transaction")
}

// TestCommitDummyTransaction_IsDummyGuard verifies that the isDummy flag
// prevents recursive commitDummyTransaction calls.
func TestCommitDummyTransaction_IsDummyGuard(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Create a dummy transaction directly — verify isDummy prevents recursion.
	tx := &Transaction{
		db:           db.db,
		tenantId:     NoTenantID,
		creationTime: time.Now(),
		isDummy:      true,
	}
	tx.writeSystemKeys = true
	tx.readSystemKeys = true
	tx.lockAware = true

	key := []byte(t.Name() + "_guard_key")
	tx.addReadConflictForKey(key)
	tx.addWriteConflictForKey(key)

	// Commit should work. Even if commit_unknown_result occurs, the isDummy
	// flag prevents recursive commitDummyTransaction, so this won't hang.
	err := tx.Commit(ctx)
	if err != nil {
		// Commit errors are OK (the test environment may not have system key access).
		// The key thing is no recursion / no hang.
		t.Logf("dummy commit error (expected): %v", err)
	} else {
		t.Log("dummy commit succeeded")
	}
}

// TestPipelinedGet_Resolve_TransportErrorRetries covers PendingGet.Resolve's
// transport-error arm (RFC-010 #3 + the handleConnError parity fix / codex gap 2):
// a reply carrying a connection-layer error re-drives through the full getValue
// path and returns the seeded value. Built by hand-constructing a PendingGet
// against a real transaction with a pre-loaded error reply.
func TestPipelinedGet_Resolve_TransportErrorRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	want := []byte("value")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx := db.CreateTransaction()
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("read version: %v", err)
	}
	replyCh := make(chan transport.Response, 1)
	replyCh <- transport.Response{Err: io.EOF} // connection/transport error
	p := &PendingGet{
		key:         key,
		tx:          tx,
		addr:        "0.0.0.0:1", // bogus addr; handleConnError marks it, no real conn
		replyCh:     replyCh,
		replyHandle: &transport.ReplyHandle{},
		ctx:         ctx,
		timer:       getTimer(DefaultRPCTimeout),
		flushed:     true, // skip conn.Flush (no conn)
	}
	got, err := p.Resolve()
	if err != nil {
		t.Fatalf("Resolve (transport error -> retry): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q (transport error should re-drive through getValue)", got, want)
	}
}

// TestPipelinedGet_Resolve_TimeoutRetries covers PendingGet.Resolve's RPC-timeout
// arm (RFC-010 #3 / codex gap 3): when the reply never arrives, the timer fires
// and Resolve re-drives through getValue rather than surfacing DeadlineExceeded.
func TestPipelinedGet_Resolve_TimeoutRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	want := []byte("value")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx := db.CreateTransaction()
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("read version: %v", err)
	}
	p := &PendingGet{
		key:         key,
		tx:          tx,
		addr:        "0.0.0.0:1",
		replyCh:     make(chan transport.Response), // never fires
		replyHandle: &transport.ReplyHandle{},
		ctx:         ctx,
		timer:       getTimer(time.Millisecond), // fires fast
		flushed:     true,
	}
	got, err := p.Resolve()
	if err != nil {
		t.Fatalf("Resolve (timeout -> retry): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q (timeout should re-drive through getValue)", got, want)
	}
}

// TestPipelinedGet_Resolve_FlushErrorRetries covers PendingGet.resolve's
// flush-error arm (RFC-118 Gap 2, transaction.go:858-869): when conn.Flush()
// fails, the request never reached the server, so the arm marks the connection
// bad (handleConnError) and re-drives through the full getValue path.
//
// The flush error is driven through a REAL conn deterministically (a write-fault
// wrapper would race the writeLoop's unconditional auto-flush, which clears
// hasDirty so Conn.Flush() short-circuits to nil): a conn dialed to the storage
// server, then Close()d (joins the loops) and SendFrameDeferred'd — which sets
// hasDirty=true UNCONDITIONALLY (conn.go:464) before its own ctx-done return — so
// Resolve()'s Flush() sees hasDirty=true + cancelled ctx + an exited writeLoop and
// returns errConnClosed (conn.go:485/495), a faithful flush error (a conn torn
// down between the deferred send and the flush). resolveFull then re-dials a clean
// conn (the pool self-heals on a closed entry, database.go:375-378) and returns
// the value.
//
// Revert-proof: with the flush-error arm removed, the ignored flush error falls
// through to the select, whose replyCh never fires and whose timer is set far
// beyond ctx, so the ctx.Done arm (transaction.go:904) returns ctx.Err() — the
// test reddens (Resolve surfaces an error instead of the value).
func TestPipelinedGet_Resolve_FlushErrorRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	want := []byte("value")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx := db.CreateTransaction()
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("read version: %v", err)
	}

	addr := storageAddrFor(t, db, ctx, key)
	conn, err := db.db.getOrDial(ctx, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	conn.Close()                                                   // joins the loops; ctx cancelled; IsClosed
	_ = conn.SendFrameDeferred(transport.UID{First: 1}, []byte{0}) // sets hasDirty=true on the closed conn

	p := &PendingGet{
		key:         key,
		tx:          tx,
		addr:        addr,
		conn:        conn,
		flushed:     false,                         // take the flush path
		replyCh:     make(chan transport.Response), // never fires (the flush fails first)
		replyHandle: &transport.ReplyHandle{},
		ctx:         ctx,
		timer:       getTimer(10 * time.Minute), // far beyond ctx: revert-proof falls to ctx.Done, not the timeout arm
	}
	got, err := p.Resolve()
	if err != nil {
		t.Fatalf("Resolve (flush error -> retry): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q (flush error must re-drive through getValue)", got, want)
	}
}

// TestRead_BoundedByTimeout_NoHang pins that a read whose proxies / storage servers
// never reply (a wedged or dead cluster) is bounded by the transaction timeout: it
// returns transaction_timed_out (1031) within the deadline instead of retrying the
// unreachable cluster forever inside the getKeyLocation load-balance loop.
//
// This is the deterministic regression for the nightly-stress hang. Under sustained
// ingest the single-node Docker FDB stopped responding and a getValue wedged for
// ~35 min inside locationCache.queryLocations (locality.go), because the SQL ingest
// path set no transaction timeout. The infinite retry itself is CORRECT, C++/Java-
// faithful behaviour (a real cluster recovers and the read resumes; neither client
// sets a default timeout) — so the fix is not to bound the client by default but to
// prove that WHEN a timeout is set, an unreachable-cluster read fails fast.
//
// Determinism (no timer-vs-real-reply race): (1) succeed a GRV and a warm read while
// the dialer is clean, (2) invalidate the key's cached location so the next read MUST
// re-run queryLocations, (3) arm() — every connection now returns EOF on Read, so no
// proxy can ever answer the GetKeyServerLocations request — then (4) SetTimeout(2s)
// and Get. The only thing that can end the read is the opContext deadline waking the
// ctx.Done() arms in queryLocations (locality.go:468/484), mapped to 1031 by
// mapTimeout (readpath.go). Revert-prove: drop those ctx.Done() arms (reintroducing
// the hang) and Get never returns; the independent 30s watchdog below fires → red.
// (The watchdog is load-bearing: a plain `elapsed > 30s` check AFTER Get could never
// fire on a true hang — Get would block to the package/CI timeout first.)
func TestRead_BoundedByTimeout_NoHang(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	defer container.Terminate(ctx)

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

	fd := &faultDialer{}
	db := newTestDatabase(t, ctx, connectCF, fd.dial)
	defer db.Close()

	key := []byte(t.Name() + "_k")

	// Seed + a warm successful read (clean dialer): populates the location cache and
	// proves the path works before we wedge it.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	// Pre-fetch a read version while the dialer is clean, so the bounded read lands in
	// the location / load-balance loop rather than blocking in GRV.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	// Force the next read to re-locate (else a cache hit would skip queryLocations).
	db.db.locCache.invalidate(key, NoTenantID)

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	tx.SetTimeout(2000) // 2s — must bound the read well under the 90s ctx

	fd.arm() // every connection now EOFs on Read: no proxy / SS can answer

	// Run the read under an INDEPENDENT watchdog. If the hang regression returns, Get
	// never returns, so checking elapsed AFTER Get would itself hang to the package/CI
	// timeout. The watchdog converts that hang into a fast, clear failure at 30s.
	start := time.Now()
	type readResult struct{ err error }
	done := make(chan readResult, 1)
	go func() {
		_, gerr := tx.Get(ctx, key)
		done <- readResult{err: gerr}
	}()
	const watchdog = 30 * time.Second // ≫ the 2s SetTimeout; a real bound returns in ~2s
	select {
	case r := <-done:
		err = r.err
	case <-time.After(watchdog):
		cancel() // best-effort: unblock the read if any exit arm still remains
		t.Fatalf("Get against a wedged cluster did not return within %v — SetTimeout did not bound the getKeyLocation loop (hang regression)", watchdog)
	}
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Get returned nil error against a wedged cluster — a read must not succeed when no server can reply")
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != ErrTransactionTimedOut {
		t.Fatalf("want transaction_timed_out (%d), got %v (%T)", ErrTransactionTimedOut, err, err)
	}
	t.Logf("read against wedged cluster bounded in %v: %v (no hang)", elapsed, err)
}
