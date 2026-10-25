package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestNativeGetRange is RFC-262 S2 "nativeGetRange (phase 2)" on multiShardEnv:
// requests are recorded at the client's send path (sendRecorder) and checked
// against NativeAPI getRange (NativeAPI.actor.cpp:4596-4893), with the shard
// map the client used read back from its location cache. Replies are held or
// rewritten by the simulated transport (simDialer). Subtests share one
// recorder and dialer, so they run in sequence. Assertions fail immediately:
// topology churn is not a reason to retry an arbitrary semantic failure.
func TestNativeGetRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ne := newNativeRangeEnv(t, ctx, setupMultiShardEnv(t, ctx))
	for _, c := range []struct {
		name string
		run  func(t *testing.T) error
	}{
		{"getRangeInternal-checks-take-no-read-version", ne.checksTakeNoReadVersion},
		{"selectors/forward", ne.selectorsForward},
		{"selectors/reverse", ne.selectorsReverse},
		{"selectors/shard-aligned", ne.selectorsShardAligned},
		{"readThrough/forward", func(t *testing.T) error { return ne.readThrough(t, false) }},
		{"readThrough/reverse", func(t *testing.T) error { return ne.readThrough(t, true) }},
		{"continuation/FGT-and-bytes0-minRows", ne.fgtContinuation},
		{"first-reply/more-from-reply", ne.firstReplyMore},
		{"readToBegin/prologue", ne.readToBeginPrologue},
		{"read-version/one-per-call", ne.oneReadVersion},
		{"read-version/threaded-through-fallback", ne.versionThreaded},
		{"fallback/empty-reply", ne.emptyReplyFallback},
		{"fallback/empty-reply-both-flags", ne.emptyReplyBothFlags},
		{"fallback/schedule-cached", func(t *testing.T) error { return ne.fallbackSchedule(t, true) }},
		{"fallback/schedule-uncached-begin", func(t *testing.T) error { return ne.fallbackSchedule(t, false) }},
		{"fallback/one-nontrivial-bound", ne.fallbackOneNontrivial},
		{"fallback/trivial-bounds", ne.fallbackTrivialBounds},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exercise the resolver's own goroutine backstop through a real location send,
// not a panic in the parent range worker after a storage reply.
func TestNativeResolveKeyLocationPanic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sim, rec := newSimDialer(), &sendRecorder{}
	db := newTestDatabase(t, ctx, startProxyFDB(t, ctx), rec.wrap(sim.dial))
	defer db.Close()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if rec.replyKind(token) == "GetKeyServerLocationsRequest" {
			heldOnce.Do(func() { close(held) })
			<-release
		}
		return body, false
	})
	sim.armEverything()
	rec.start()
	var pendingToken transport.UID
	var pendingConn *transport.Conn
	var pendingAddr string
	hook := func() {
		// A send returns once its frame is queued (DESIGN.md T1), so the request
		// may reach the wire after the hook runs: wait for the writer to write it.
		for deadline := time.Now().Add(5 * time.Second); pendingToken == (transport.UID{}) && time.Now().Before(deadline); {
			for _, msg := range rec.messages() {
				if q, ok := msg.(*types.GetKeyServerLocationsRequest); ok {
					pendingToken = transport.UID{First: binary.LittleEndian.Uint64(q.Reply.Token[:8]), Second: binary.LittleEndian.Uint64(q.Reply.Token[8:])}
				}
			}
			if pendingToken == (transport.UID{}) {
				time.Sleep(time.Millisecond)
			}
		}
		db.db.connMu.Lock()
		for addr, conn := range db.db.connPool {
			if conn.HasPendingReply(pendingToken) {
				pendingConn, pendingAddr = conn, addr
				break
			}
		}
		db.db.connMu.Unlock()
		panic("resolver afterLocationSend")
	}
	db.db.afterLocationSend.Store(&hook)
	defer db.db.afterLocationSend.Store(nil)
	done := make(chan error, 1)
	go func() {
		_, err := tx.nativeGetRangeFallback(ctx, KeySelector{Key: []byte("rfc262/resolver-panic"), Offset: 2}, firstGreaterOrEqual(allKeysEnd), newRangeLimits(1, -1), rv, false)
		done <- err
	}()
	select {
	case err := <-done:
		if code, _ := fdbErrorCode(err); code != ErrInternalError {
			t.Fatalf("resolver panic = %v, want internal_error (4100)", err)
		}
	case <-ctx.Done():
		t.Fatal("resolver panic stranded fallback")
	}
	if n := db.db.resolveKeyPanics.consecutive.Load(); n != 1 {
		t.Fatalf("resolver backstop caught %d panics, want exactly one", n)
	}
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("location reply was not held")
	}
	if pendingConn == nil {
		t.Fatal("positive control: location send never had a pending reply registration")
	}
	if pendingConn.HasPendingReply(pendingToken) {
		t.Fatal("resolver panic retained its reply token while the reply is held")
	}
	if pendingConn.IsClosed() {
		t.Fatal("panic cleanup closed the shared connection")
	}
	db.db.afterLocationSend.Store(nil)
	unblock()
	loc, err := db.db.locCache.locate(db.db, ctx, []byte("rfc262/resolver-panic"), NoTenantID, types.SpanContext{}, false)
	if err != nil || len(loc.Servers) == 0 {
		t.Fatalf("location lookup after panic = %+v, %v", loc, err)
	}
	if reused, err := db.db.getOrDial(ctx, pendingAddr); err != nil || reused != pendingConn {
		t.Fatalf("connection was not reusable after panic: same=%t err=%v", reused == pendingConn, err)
	}
	if n := nrCount(rec.stop().msgs, "GetKeyServerLocationsRequest"); n < 2 {
		t.Fatalf("location sends = %d, want the panicking attempt and a successful reuse", n)
	}
}

type nativeRangeEnv struct {
	ctx    context.Context
	db     *Database
	sd     *simDialer
	rec    *sendRecorder
	prefix []byte
	keys   [][]byte // the seeded keys, ascending
}

func newNativeRangeEnv(t *testing.T, ctx context.Context, env *multiShardEnv) *nativeRangeEnv {
	t.Helper()
	connStr, err := env.container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatal(err)
	}
	ne := &nativeRangeEnv{ctx: ctx, sd: newSimDialer(), rec: &sendRecorder{}, prefix: []byte(env.prefix)}
	ne.db = newTestDatabase(t, ctx, cf, ne.rec.wrap(ne.sd.dial))
	t.Cleanup(func() { ne.db.Close() })
	ne.db.db.hedgeEnabled.Store(false) // a hedge is a second request no path decides
	ne.sd.armEverything()              // no intercept yet: everything passes
	for i := 0; i < 100; i++ {
		ne.keys = append(ne.keys, []byte(fmt.Sprintf("%s%04d", env.prefix, i)))
	}
	ne.freezeShards(t, env)
	return ne
}

// freezeShards turns data distribution off once no shard of the seeded range
// can fill a reply: that shard's continuation is empty and getRange falls back
// to getExactRange (NativeAPI.actor.cpp:4828-4836), which the transcripts exclude.
func (ne *nativeRangeEnv) freezeShards(t *testing.T, env *multiShardEnv) {
	t.Helper()
	shards := func() []LocationResult {
		locs, err := env.db.db.locCache.locateRange(env.db.db, ne.ctx, ne.prefix, ne.prefixEnd(), 1000, false, NoTenantID, types.SpanContext{})
		if err != nil {
			t.Fatal(err)
		}
		env.db.db.locCache.invalidateRange(ne.prefix, ne.prefixEnd(), NoTenantID)
		return locs
	}
	for start := time.Now(); ; time.Sleep(time.Second) {
		full := nrFullReplyShard(shards(), ne.keys)
		if full == nil {
			break
		}
		if time.Since(start) > 2*time.Minute {
			t.Fatalf("shard [%q, %q) still fills a reply after 2m of data distribution", full.ShardBegin, full.ShardEnd)
		}
	}
	if out, err := env.container.FDBCLIExec(ne.ctx, "datadistribution off"); err != nil {
		t.Fatalf("freeze data distribution: %v, output=%s", err, out)
	}
	last := nrBoundaries(shards())
	for stable, start := 0, time.Now(); stable < 3; {
		if time.Since(start) > 30*time.Second {
			t.Fatal("shard map did not settle after data distribution off")
		}
		time.Sleep(time.Second)
		if b := nrBoundaries(shards()); slices.EqualFunc(b, last, bytes.Equal) {
			stable++
		} else {
			last, stable = b, 0
		}
	}
	if full := nrFullReplyShard(shards(), ne.keys); full != nil {
		t.Fatalf("frozen shard [%q, %q) fills a reply", full.ShardBegin, full.ShardEnd)
	}
}

// nrFullReplyShard is the first shard holding enough seeded keys to fill a
// storage reply's byte limit, or nil.
func nrFullReplyShard(locs []LocationResult, keys [][]byte) *LocationResult {
	for i := range locs {
		n := 0
		for _, k := range keys {
			if bytes.Compare(k, locs[i].ShardBegin) >= 0 && (locs[i].ShardEnd == nil || bytes.Compare(k, locs[i].ShardEnd) < 0) {
				n++
			}
		}
		if n*multiShardValueBytes >= replyByteLimit {
			return &locs[i]
		}
	}
	return nil
}

func nrBoundaries(locs []LocationResult) [][]byte {
	out := make([][]byte, len(locs))
	for i, l := range locs {
		out[i] = l.ShardBegin
	}
	return out
}

// nrRangeLocates counts getExactRange's range lookups, which only a fallback sends here.
func nrRangeLocates(msgs []any) int {
	n := 0
	for _, m := range msgs {
		if q, ok := m.(*types.GetKeyServerLocationsRequest); ok && q.HasEnd {
			n++
		}
	}
	return n
}

func TestNrFullReplyShard(t *testing.T) {
	t.Parallel()
	keys := make([][]byte, 20)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("k%02d", i))
	}
	perReply := (replyByteLimit + multiShardValueBytes - 1) / multiShardValueBytes
	split := []byte(fmt.Sprintf("k%02d", perReply))
	small := []LocationResult{{ShardBegin: []byte("k"), ShardEnd: []byte(fmt.Sprintf("k%02d", perReply-1))}, {ShardBegin: []byte(fmt.Sprintf("k%02d", perReply-1)), ShardEnd: []byte("k10")}, {ShardBegin: []byte("k10"), ShardEnd: []byte("k17")}, {ShardBegin: []byte("k17")}}
	if got := nrFullReplyShard(small, keys); got != nil {
		t.Fatalf("shards under %d keys: got [%q, %q)", perReply, got.ShardBegin, got.ShardEnd)
	}
	full := []LocationResult{{ShardBegin: []byte("k"), ShardEnd: split}, {ShardBegin: split}}
	if got := nrFullReplyShard(full, keys); got == nil || !bytes.Equal(got.ShardEnd, split) {
		t.Fatalf("a %d-key shard fills a reply: got %v", perReply, got)
	}
	unbounded := []LocationResult{{ShardBegin: []byte("k")}}
	if got := nrFullReplyShard(unbounded, keys); got == nil || got.ShardEnd != nil {
		t.Fatalf("the open last shard holds all %d keys: got %v", len(keys), got)
	}
}

func (ne *nativeRangeEnv) prefixEnd() []byte { return append(append([]byte{}, ne.prefix...), 0xff) }

// tx is a transaction with its read version taken outside any recording.
func (ne *nativeRangeEnv) tx(t *testing.T) (*Transaction, int64) {
	t.Helper()
	tx := ne.db.CreateTransaction()
	rv, err := tx.GetReadVersion(ne.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx, rv
}

// locate is the client's cached location for key.
func (ne *nativeRangeEnv) locate(t *testing.T, key []byte, backward bool) LocationResult {
	t.Helper()
	loc, err := ne.db.db.locCache.locate(ne.db.db, ne.ctx, key, NoTenantID, types.SpanContext{}, backward)
	if err != nil {
		t.Fatal(err)
	}
	if loc.ShardEnd == nil {
		loc.ShardEnd = allKeysEnd
	}
	return loc
}

// shards are the shards of the seeded range holding at least minKeys keys,
// strictly inside it (so a range from the prefix's start or to its end
// crosses them), with their keys.
func (ne *nativeRangeEnv) shards(t *testing.T, minKeys int) []nrShard {
	t.Helper()
	locs, err := ne.db.db.locCache.locateRange(ne.db.db, ne.ctx, ne.prefix, ne.prefixEnd(), 1000, false, NoTenantID, types.SpanContext{})
	if err != nil {
		t.Fatal(err)
	}
	var out []nrShard
	for _, l := range locs {
		if l.ShardEnd == nil || bytes.Compare(l.ShardBegin, ne.prefix) <= 0 || bytes.Compare(l.ShardEnd, ne.prefixEnd()) >= 0 {
			continue
		}
		s := nrShard{begin: l.ShardBegin, end: l.ShardEnd}
		for _, k := range ne.keys {
			if bytes.Compare(k, l.ShardBegin) >= 0 && bytes.Compare(k, l.ShardEnd) < 0 {
				s.keys = append(s.keys, k)
			}
		}
		if len(s.keys) >= minKeys {
			out = append(out, s)
		}
	}
	if len(out) < 2 {
		t.Fatalf("precondition: want two interior shards with >= %d keys, have %d of %d shards", minKeys, len(out), len(locs))
	}
	return out
}

type nrShard struct {
	begin, end []byte
	keys       [][]byte
}

// hold makes the simulated transport keep every reply until release.
func (ne *nativeRangeEnv) hold() (release func()) {
	ch := make(chan struct{})
	ne.sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		<-ch
		return body, false
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			ne.sd.setIntercept(nil)
			close(ch)
		})
	}
}

// wrongShardOnce replaces the first reply to a recorded storage request with
// wrong_shard_server.
func (ne *nativeRangeEnv) wrongShardOnce() (injected *atomic.Int32, disarm func()) {
	injected = &atomic.Int32{}
	ne.sd.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if ne.rec.isStorageReply(token) && injected.CompareAndSwap(0, 1) {
			return inlineErrorReply(ErrWrongShardServer, injectPenalty), false
		}
		return body, false
	})
	return injected, func() { ne.sd.setIntercept(nil) }
}

func nrRecord(rec *sendRecorder, fn func()) recording {
	rec.start()
	fn()
	return rec.stop()
}

func nrProblems(rec recording) error {
	if len(rec.problems) > 0 {
		return fmt.Errorf("recorder problems:\n%s", strings.Join(rec.problems, "\n"))
	}
	return nil
}

func nrGetKeyValues(msgs []any) []*types.GetKeyValuesRequest {
	var out []*types.GetKeyValuesRequest
	for _, m := range msgs {
		if q, ok := m.(*types.GetKeyValuesRequest); ok {
			out = append(out, q)
		}
	}
	return out
}

func nrKind(m any) string { return fmt.Sprintf("%T", m)[len("*types."):] }

func nrCount(msgs []any, kind string) int {
	n := 0
	for _, m := range msgs {
		if nrKind(m) == kind {
			n++
		}
	}
	return n
}

func nrRefIs(r types.KeySelectorRef, s KeySelector) bool {
	return bytes.Equal(r.Key, s.Key) && r.OrEqual == s.OrEqual && r.Offset == s.Offset
}

func nrSel(r types.KeySelectorRef) KeySelector {
	return KeySelector{Key: r.Key, OrEqual: r.OrEqual, Offset: r.Offset}
}

func nrKeys(kvs []KeyValue) string {
	s := make([]string, len(kvs))
	for i := range kvs {
		s[i] = string(kvs[i].Key)
	}
	return strings.Join(s, ",")
}

func nrJoin(keys [][]byte, reverse bool) string {
	s := make([]string, len(keys))
	for i, k := range keys {
		if reverse {
			s[len(keys)-1-i] = string(k)
		} else {
			s[i] = string(k)
		}
	}
	return strings.Join(s, ",")
}

// keysBetween are the seeded keys in [lo, hi).
func (ne *nativeRangeEnv) keysBetween(lo, hi []byte) [][]byte {
	var out [][]byte
	for _, k := range ne.keys {
		if bytes.Compare(k, lo) >= 0 && bytes.Compare(k, hi) < 0 {
			out = append(out, k)
		}
	}
	return out
}

// storageVersions reports a storage request at a version other than rv.
func storageVersions(msgs []any, rv int64) error {
	for _, m := range msgs {
		var v int64
		switch q := m.(type) {
		case *types.GetKeyValuesRequest:
			v = q.Version
		case *types.GetKeyRequest:
			v = q.Version
		default:
			continue
		}
		if v != rv {
			return fmt.Errorf("%s at version %d, want %d", nrKind(m), v, rv)
		}
	}
	return nil
}

// The checks in getRangeInternal (:5810-5833) answer before startTransaction
// (:4617), so they send nothing, not even a GetReadVersionRequest, and register
// no conflict range (:5841).
func (ne *nativeRangeEnv) checksTakeNoReadVersion(t *testing.T) error {
	tx := ne.db.CreateTransaction()
	defer tx.Cancel()
	a, b := ne.keys[10], ne.keys[20]
	before := ne.db.db.metrics.getRangeRequests.Load()
	var errs []string
	got := nrRecord(ne.rec, func() {
		for _, c := range []struct {
			name       string
			begin, end KeySelector
			limits     rangeLimits
			wantErr    int
		}{
			{"reached", firstGreaterOrEqual(a), firstGreaterOrEqual(b), rangeLimits{0, 0, -1}, 0},
			{"invalid", firstGreaterOrEqual(a), firstGreaterOrEqual(b), rangeLimits{-2, 1, -1}, ErrRangeLimitsInvalid},
			// removeOrEqual(FGT(a)) is FGE(keyAfter(a)) = end: inverted.
			{"inverted-after-removeOrEqual", firstGreaterThan(a), firstGreaterOrEqual(keyAfterBytes(a)), rangeLimits{-1, 1, -1}, 0},
			{"inverted", firstGreaterOrEqual(b), firstGreaterOrEqual(a), rangeLimits{-1, 1, -1}, 0},
		} {
			r, err := tx.nativeGetRange(ne.ctx, c.begin, c.end, c.limits, false, false)
			code, _ := fdbErrorCode(err)
			if (err != nil || c.wantErr != 0) && code != c.wantErr {
				errs = append(errs, fmt.Sprintf("%s: err %v, want code %d", c.name, err, c.wantErr))
			}
			if len(r.kvs) != 0 || r.conflict != nil || r.more {
				errs = append(errs, fmt.Sprintf("%s: %+v, want an empty result without conflict", c.name, r))
			}
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	if len(got.requests) != 0 || len(got.problems) != 0 {
		return fmt.Errorf("sent %d frames, want none:\n%s", len(got.requests)+len(got.problems), strings.Join(append(got.requests, got.problems...), "\n"))
	}
	if n := ne.db.db.metrics.getRangeRequests.Load() - before; n != 4 {
		return fmt.Errorf("getRangeRequests counted %d, want 4 (:5808 counts before the checks)", n)
	}
	return nil
}

// Forward, each request's end is FGE(shard.end) when the call's end resolves
// past the shard (:4672-4677); the next request starts at the shard boundary
// (:4842-4843) or after the last row (:4850).
func (ne *nativeRangeEnv) selectorsForward(t *testing.T) error {
	tx, rv := ne.tx(t)
	defer tx.Cancel()
	begin, end := firstGreaterOrEqual(ne.prefix), firstGreaterOrEqual(ne.prefixEnd())
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), false, false)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	if k, want := nrKeys(r.kvs), nrJoin(ne.keys, false); k != want || r.more || r.readThrough != nil || r.readToBegin || r.readThroughEnd {
		return fmt.Errorf("result %s more=%t readThrough=%q flags=%t,%t; want every key, no more", k, r.more, r.readThrough, r.readToBegin, r.readThroughEnd)
	}
	// :4562-4578: forward, done, offsets 1: the selector keys.
	if r.conflict == nil || !bytes.Equal(r.conflict[0], ne.prefix) || !bytes.Equal(r.conflict[1], ne.prefixEnd()) {
		return fmt.Errorf("conflict %q, want [%q, %q)", r.conflict, ne.prefix, ne.prefixEnd())
	}
	if n := nrCount(got.msgs, "GetKeyRequest") + nrRangeLocates(got.msgs); n != 0 {
		return fmt.Errorf("unexpected fallback (%d GetKeyRequests and range locates):\n%s", n, transcript)
	}
	reqs := nrGetKeyValues(got.msgs)
	if len(reqs) < 2 {
		return fmt.Errorf("%d storage requests, want one per shard (>= 2):\n%s", len(reqs), transcript)
	}
	for i, q := range reqs {
		loc := ne.locate(t, q.Begin.Key, false)
		wantEnd := end
		if end.isDefinitelyGreater(loc.ShardEnd) {
			wantEnd = firstGreaterOrEqual(loc.ShardEnd)
		}
		if !nrRefIs(q.End, wantEnd) {
			return fmt.Errorf("request %d end %+v, want %+v for shard [%q, %q):\n%s", i, q.End, wantEnd, loc.ShardBegin, loc.ShardEnd, transcript)
		}
		switch {
		case i == 0 && !nrRefIs(q.Begin, begin):
			return fmt.Errorf("request 0 begin %+v, want %+v", q.Begin, begin)
		case i > 0 && !nrRefIs(q.Begin, firstGreaterOrEqual(reqs[i-1].End.Key)) && !(q.Begin.OrEqual && q.Begin.Offset == 1):
			return fmt.Errorf("request %d begin %+v continues neither at the shard boundary nor after a row:\n%s", i, q.Begin, transcript)
		}
		if q.Version != rv || q.Limit != replyByteLimit || q.LimitBytes != replyByteLimit {
			return fmt.Errorf("request %d version %d limits (%d, %d), want %d (%d, %d)", i, q.Version, q.Limit, q.LimitBytes, rv, replyByteLimit, replyByteLimit)
		}
	}
	return nil
}

// Reverse, each request's begin is FGE(shard.begin) when the call's begin
// resolves before the shard (:4656-4669); the next request ends at the shard
// boundary (:4840-4841) or at the last row (:4848).
func (ne *nativeRangeEnv) selectorsReverse(t *testing.T) error {
	tx, rv := ne.tx(t)
	defer tx.Cancel()
	begin, end := firstGreaterOrEqual(ne.prefix), firstGreaterOrEqual(ne.prefixEnd())
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), true, true)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	if k, want := nrKeys(r.kvs), nrJoin(ne.keys, true); k != want || r.more || r.readThrough != nil || r.conflict != nil {
		return fmt.Errorf("result %s more=%t readThrough=%q conflict=%v; want every key descending, no more, no conflict", k, r.more, r.readThrough, r.conflict)
	}
	if n := nrCount(got.msgs, "GetKeyRequest") + nrRangeLocates(got.msgs); n != 0 {
		return fmt.Errorf("unexpected fallback (%d GetKeyRequests and range locates):\n%s", n, transcript)
	}
	reqs := nrGetKeyValues(got.msgs)
	if len(reqs) < 2 {
		return fmt.Errorf("%d storage requests, want one per shard (>= 2):\n%s", len(reqs), transcript)
	}
	beginLess := KeySelector{Key: begin.Key, Offset: begin.Offset - 1}
	for i, q := range reqs {
		loc := ne.locate(t, q.End.Key, keySelectorIsBackward(q.End.OrEqual, q.End.Offset-1))
		wantBegin := begin
		if beginLess.isDefinitelyLess(loc.ShardBegin) && !bytes.Equal(begin.Key, loc.ShardBegin) {
			wantBegin = firstGreaterOrEqual(loc.ShardBegin)
		}
		if !nrRefIs(q.Begin, wantBegin) {
			return fmt.Errorf("request %d begin %+v, want %+v for shard [%q, %q):\n%s", i, q.Begin, wantBegin, loc.ShardBegin, loc.ShardEnd, transcript)
		}
		switch {
		case i == 0 && !nrRefIs(q.End, end):
			return fmt.Errorf("request 0 end %+v, want %+v", q.End, end)
		case i > 0 && (q.End.OrEqual || q.End.Offset != 1):
			return fmt.Errorf("request %d end %+v is not an FGE continuation:\n%s", i, q.End, transcript)
		}
		if q.Version != rv || q.Limit != -replyByteLimit || q.LimitBytes != replyByteLimit {
			return fmt.Errorf("request %d version %d limits (%d, %d), want %d (%d, %d)", i, q.Version, q.Limit, q.LimitBytes, rv, -replyByteLimit, replyByteLimit)
		}
	}
	return nil
}

// Selectors exactly at the shard's edges are not modified: forward FGE(shard.end)
// is not definitely greater (:4672), and reverse begin FGE(shard.begin) is the
// exception at :4657-4658. One request, finished without more or readThrough.
func (ne *nativeRangeEnv) selectorsShardAligned(t *testing.T) error {
	s := ne.shards(t, 1)[0]
	for _, reverse := range []bool{false, true} {
		tx, _ := ne.tx(t)
		var r nativeRangeResult
		var err error
		got := nrRecord(ne.rec, func() {
			r, err = tx.nativeGetRange(ne.ctx, firstGreaterOrEqual(s.begin), firstGreaterOrEqual(s.end), newRangeLimits(0, ByteLimitUnlimited), true, reverse)
		})
		tx.Cancel()
		if err != nil {
			return err
		}
		if err := nrProblems(got); err != nil {
			return err
		}
		reqs := nrGetKeyValues(got.msgs)
		if len(reqs) != 1 || !nrRefIs(reqs[0].Begin, firstGreaterOrEqual(s.begin)) || !nrRefIs(reqs[0].End, firstGreaterOrEqual(s.end)) {
			return fmt.Errorf("reverse=%t: want one request [FGE(%q), FGE(%q)):\n%s", reverse, s.begin, s.end, strings.Join(got.requests, "\n"))
		}
		if k, want := nrKeys(r.kvs), nrJoin(s.keys, reverse); k != want || r.more || r.readThrough != nil {
			return fmt.Errorf("reverse=%t: result %s more=%t readThrough=%q, want %s, no more, no readThrough", reverse, k, r.more, r.readThrough, want)
		}
	}
	return nil
}

// A byte-limited read whose first reply exhausts a modified shard stops
// there with more and readThrough at the shard edge (:4761-4762, 4796-4800).
func (ne *nativeRangeEnv) readThrough(t *testing.T, reverse bool) error {
	s := ne.shards(t, 1)[0]
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	begin, end := firstGreaterOrEqual(s.keys[len(s.keys)-1]), firstGreaterOrEqual(ne.prefixEnd())
	wantKey, wantRT := s.keys[len(s.keys)-1], s.end
	wantReq := [2]KeySelector{begin, firstGreaterOrEqual(s.end)}
	if reverse {
		begin, end = firstGreaterOrEqual(ne.prefix), firstGreaterOrEqual(keyAfterBytes(s.keys[0]))
		wantKey, wantRT = s.keys[0], s.begin
		wantReq = [2]KeySelector{firstGreaterOrEqual(s.begin), end}
	}
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, 1_000_000), true, reverse)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	reqs := nrGetKeyValues(got.msgs)
	if len(reqs) != 1 || !nrRefIs(reqs[0].Begin, wantReq[0]) || !nrRefIs(reqs[0].End, wantReq[1]) || reqs[0].LimitBytes != replyByteLimit {
		return fmt.Errorf("want one request [%+v, %+v) at byte limit %d:\n%s", wantReq[0], wantReq[1], replyByteLimit, strings.Join(got.requests, "\n"))
	}
	if nrKeys(r.kvs) != string(wantKey) || !r.more || !bytes.Equal(r.readThrough, wantRT) {
		return fmt.Errorf("result %s more=%t readThrough=%q, want %s, more, readThrough %q", nrKeys(r.kvs), r.more, r.readThrough, wantKey, wantRT)
	}
	return nil
}

// A byte limit below one row with minRows 3: the first reply (one row, more)
// spends the bytes before minRows, so the read continues at
// firstGreaterThan(row), OrEqual=true on the wire (:4850), and the second
// request takes transformRangeLimits' bytes == 0 branch: limit = minRows = 2,
// limitBytes = REPLY_BYTE_LIMIT (:4236-4238).
func (ne *nativeRangeEnv) fgtContinuation(t *testing.T) error {
	s := ne.shards(t, 3)[0]
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	begin, end := firstGreaterOrEqual(s.keys[0]), firstGreaterOrEqual(ne.prefixEnd())
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, rangeLimits{rows: -1, minRows: 3, bytes: 5000}, true, false)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	reqs := nrGetKeyValues(got.msgs)
	shardEnd := firstGreaterOrEqual(s.end)
	if len(reqs) != 2 {
		return fmt.Errorf("%d storage requests, want 2:\n%s", len(reqs), transcript)
	}
	if q := reqs[0]; !nrRefIs(q.Begin, begin) || !nrRefIs(q.End, shardEnd) || q.Limit != replyByteLimit || q.LimitBytes != 5000 {
		return fmt.Errorf("request 0 %+v %+v (%d, %d), want [%+v, %+v) (%d, 5000):\n%s", q.Begin, q.End, q.Limit, q.LimitBytes, begin, shardEnd, replyByteLimit, transcript)
	}
	if q := reqs[1]; !nrRefIs(q.Begin, firstGreaterThan(s.keys[0])) || !nrRefIs(q.End, shardEnd) || q.Limit != 2 || q.LimitBytes != replyByteLimit {
		return fmt.Errorf("request 1 %+v %+v (%d, %d), want [FGT(%q), %+v) (2, %d):\n%s", q.Begin, q.End, q.Limit, q.LimitBytes, s.keys[0], shardEnd, replyByteLimit, transcript)
	}
	if k, want := nrKeys(r.kvs), nrJoin(s.keys[:3], false); k != want || !r.more {
		return fmt.Errorf("result %s more=%t, want %s, more", k, r.more, want)
	}
	return nil
}

// A byte-limited read inside one shard whose first reply the server cuts
// short (one row, more): minRows is satisfied but the bytes are not spent, so
// the call finishes on that reply with more taken from the reply alone
// (:4761, 4774); the selectors were not modified, so no readThrough.
func (ne *nativeRangeEnv) firstReplyMore(t *testing.T) error {
	s := ne.shards(t, 3)[0]
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	var cut atomic.Int32
	ne.sd.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if ne.rec.isStorageReply(token) && cut.CompareAndSwap(0, 1) {
			return partialBatchReply(body, 1), false
		}
		return body, false
	})
	begin, end := firstGreaterOrEqual(s.keys[0]), firstGreaterOrEqual(s.end)
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, 1_000_000), true, false)
	})
	ne.sd.setIntercept(nil)
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	if reqs := nrGetKeyValues(got.msgs); cut.Load() != 1 || len(reqs) != 1 || !nrRefIs(reqs[0].End, end) {
		return fmt.Errorf("want one unmodified request, its reply cut to one row:\n%s", strings.Join(got.requests, "\n"))
	}
	if nrKeys(r.kvs) != string(s.keys[0]) || !r.more || r.readThrough != nil {
		return fmt.Errorf("result %s more=%t readThrough=%q, want %s, more, no readThrough", nrKeys(r.kvs), r.more, r.readThrough, s.keys[0])
	}
	return nil
}

// begin lastLessThan(""): readToBegin and FGE("") (:4623-4626), so a
// non-snapshot read's conflict range starts at "" (:4562-4563).
func (ne *nativeRangeEnv) readToBeginPrologue(t *testing.T) error {
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, KeySelector{Key: []byte{}}, firstGreaterOrEqual(ne.prefixEnd()), newRangeLimits(0, 1), false, false)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	if reqs := nrGetKeyValues(got.msgs); len(reqs) == 0 || !nrRefIs(reqs[0].Begin, firstGreaterOrEqual([]byte{})) {
		return fmt.Errorf("want the first request from FGE(\"\"):\n%s", strings.Join(got.requests, "\n"))
	}
	if len(r.kvs) == 0 || !r.readToBegin || r.conflict == nil || len(r.conflict[0]) != 0 {
		return fmt.Errorf("rows=%d readToBegin=%t conflict=%q, want rows, readToBegin, conflict from \"\"", len(r.kvs), r.readToBegin, r.conflict)
	}
	return nil
}

// fallbackSelectors are two non-trivial selectors in different shards that
// each resolve inside their shard (the second key of a shard with >= 3 keys),
// and the exact range they resolve to.
func (ne *nativeRangeEnv) fallbackSelectors(t *testing.T) (begin, end KeySelector, b, e []byte) {
	shards := ne.shards(t, 3)
	s1, s2 := shards[0], shards[len(shards)-1]
	return KeySelector{Key: s1.keys[0], Offset: 2}, KeySelector{Key: s2.keys[0], Offset: 2}, s1.keys[1], s2.keys[1]
}

// One read version per call (:4617-4618): a fresh transaction sends exactly
// one GetReadVersionRequest, and every request, the fallback's getKeys and
// exact range included, carries it.
func (ne *nativeRangeEnv) oneReadVersion(t *testing.T) error {
	begin, end, b, e := ne.fallbackSelectors(t)
	tx := ne.db.CreateTransaction()
	defer tx.Cancel()
	injected, disarm := ne.wrongShardOnce()
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), true, false)
	})
	disarm()
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	if injected.Load() != 1 {
		return fmt.Errorf("wrong_shard_server injected %d times, want 1", injected.Load())
	}
	if n := nrCount(got.msgs, "GetReadVersionRequest"); n != 1 {
		return fmt.Errorf("%d GetReadVersionRequests, want 1:\n%s", n, transcript)
	}
	if nrCount(got.msgs, "GetKeyRequest") < 2 {
		return fmt.Errorf("want the fallback's two GetKeyRequests:\n%s", transcript)
	}
	rv, err := tx.GetReadVersion(ne.ctx)
	if err != nil {
		return err
	}
	if err := storageVersions(got.msgs, rv); err != nil {
		return fmt.Errorf("%v:\n%s", err, transcript)
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(b, e), false); k != want || r.more {
		return fmt.Errorf("result %s more=%t, want %s", k, r.more, want)
	}
	return nil
}

// The version is the call's, not the transaction's: given an older one, every
// request through the fallback carries it.
func (ne *nativeRangeEnv) versionThreaded(t *testing.T) error {
	begin, end, b, e := ne.fallbackSelectors(t)
	tx, own := ne.tx(t)
	defer tx.Cancel()
	older := own - 1
	injected, disarm := ne.wrongShardOnce()
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRangeAt(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), older, false)
	})
	disarm()
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	if injected.Load() != 1 || nrCount(got.msgs, "GetKeyRequest") < 2 {
		return fmt.Errorf("want the injected fallback:\n%s", transcript)
	}
	if err := storageVersions(got.msgs, older); err != nil {
		return fmt.Errorf("%v (the transaction's is %d):\n%s", err, own, transcript)
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(b, e), false); k != want {
		return fmt.Errorf("result %s, want %s", k, want)
	}
	return nil
}

// An empty, final reply on a modified shard falls back (:4831-4836) with the
// original selectors; FGE bounds resolve without a request, and the exact
// range starts again at the begin.
func (ne *nativeRangeEnv) emptyReplyFallback(t *testing.T) error {
	s := ne.shards(t, 1)[0]
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	gap := keyAfterBytes(s.keys[len(s.keys)-1]) // no key in [gap, s.end)
	begin, end := firstGreaterOrEqual(gap), firstGreaterOrEqual(ne.prefixEnd())
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), true, false)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	transcript := strings.Join(got.requests, "\n")
	reqs := nrGetKeyValues(got.msgs)
	if nrCount(got.msgs, "GetKeyRequest") != 0 || len(reqs) < 3 ||
		!nrRefIs(reqs[0].Begin, begin) || !nrRefIs(reqs[0].End, firstGreaterOrEqual(s.end)) ||
		!nrRefIs(reqs[1].Begin, begin) {
		return fmt.Errorf("want [FGE(%q), FGE(%q)), then the exact range from FGE(%q) with no GetKeyRequest:\n%s", gap, s.end, gap, transcript)
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(gap, ne.prefixEnd()), false); k != want || r.more || r.readToBegin || r.readThroughEnd || r.readThrough != nil {
		return fmt.Errorf("result %s more=%t flags=%t,%t readThrough=%q, want %s and nothing else", k, r.more, r.readToBegin, r.readThroughEnd, r.readThrough, want)
	}
	return nil
}

// emptyBatchReply is a GetKeyValues reply rewritten to no rows and no more:
// the reply of a shard with nothing in the requested range.
func emptyBatchReply(body []byte) []byte {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(body, &r); err != nil {
		return body
	}
	var reply types.GetKeyValuesReply
	reply.UnmarshalFromReader(&r)
	reply.Data = packKeyValueRefVector(nil)
	reply.More = false
	return types.MarshalErrorOrValueGetKeyValuesReply(&reply)
}

// The whole database, with the first shard's reply empty: its end was
// rewritten to the shard's (:4672-4677), so getRange falls back (:4831-4836);
// FGE("") and FGE(\xff\xff) resolve without a request, and the exact range
// ["", \xff\xff) read to the end sets both flags (:4482-4491). The conflict
// range is then ["", \xff\xff) (:4562-4563, 4573-4574).
func (ne *nativeRangeEnv) emptyReplyBothFlags(t *testing.T) error {
	first := ne.locate(t, []byte{}, false)
	tx, _ := ne.tx(t)
	defer tx.Cancel()
	var injected atomic.Int32
	ne.sd.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if ne.rec.isStorageReply(token) && injected.CompareAndSwap(0, 1) {
			return emptyBatchReply(body), false
		}
		return body, false
	})
	begin, end := firstGreaterOrEqual([]byte{}), firstGreaterOrEqual(allKeysEnd)
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRange(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), false, false)
	})
	ne.sd.setIntercept(nil)
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	reqs := nrGetKeyValues(got.msgs)
	if injected.Load() != 1 || nrCount(got.msgs, "GetKeyRequest") != 0 || len(reqs) < 2 ||
		!nrRefIs(reqs[0].Begin, begin) || !nrRefIs(reqs[0].End, firstGreaterOrEqual(first.ShardEnd)) || !nrRefIs(reqs[1].Begin, begin) {
		return fmt.Errorf("want [FGE(\"\"), FGE(%q)) emptied, then the exact range from FGE(\"\") with no GetKeyRequest:\n%s", first.ShardEnd, nrDescribe(got.msgs))
	}
	var user [][]byte
	for _, kv := range r.kvs {
		if len(kv.Key) > 0 && kv.Key[0] == 0xff {
			continue
		}
		user = append(user, kv.Key)
	}
	if k, want := nrJoin(user, false), nrJoin(ne.keys, false); k != want || len(r.kvs) == len(user) {
		return fmt.Errorf("user rows %s (of %d), want every seeded key then system rows", k, len(r.kvs))
	}
	if r.more || !r.readToBegin || !r.readThroughEnd || r.readThrough != nil {
		return fmt.Errorf("more=%t readToBegin=%t readThroughEnd=%t readThrough=%q, want false, true, true, nil", r.more, r.readToBegin, r.readThroughEnd, r.readThrough)
	}
	if r.conflict == nil || len(r.conflict[0]) != 0 || !bytes.Equal(r.conflict[1], allKeysEnd) {
		return fmt.Errorf("conflict %q, want [\"\", \\xff\\xff)", r.conflict)
	}
	return nil
}

// awaitRequests waits until n requests are recorded or 3s pass, then 200ms
// more for any request that should not be there, and returns what was sent.
func (ne *nativeRangeEnv) awaitRequests(n int) []any {
	deadline := time.Now().Add(3 * time.Second)
	for len(ne.rec.messages()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return ne.rec.messages()
}

func nrDescribe(msgs []any) string {
	var s []string
	for _, m := range msgs {
		switch q := m.(type) {
		case *types.GetKeyRequest:
			s = append(s, fmt.Sprintf("GetKeyRequest(%q,%t,%d)", q.Sel.Key, q.Sel.OrEqual, q.Sel.Offset))
		case *types.GetKeyServerLocationsRequest:
			s = append(s, fmt.Sprintf("GetKeyServerLocationsRequest(%q)", q.Begin))
		case *types.GetKeyValuesRequest:
			s = append(s, fmt.Sprintf("GetKeyValuesRequest(%q..%q)", q.Begin.Key, q.End.Key))
		default:
			s = append(s, nrKind(m))
		}
	}
	return strings.Join(s, "\n")
}

// The fallback's scheduling (:4465-4469), with every reply held by the
// simulated transport: begin's resolution runs to its first network wait
// before end's starts, and both are on the wire before either reply. With
// cached locations that is GetKeyRequest(begin) then GetKeyRequest(end); with
// begin's location uncached, begin's location request then end's
// GetKeyRequest. After the release, the exact range starts at FGE(b).
func (ne *nativeRangeEnv) fallbackSchedule(t *testing.T, cached bool) error {
	begin, end, b, e := ne.fallbackSelectors(t)
	ne.locate(t, begin.Key, false)
	ne.locate(t, end.Key, false)
	first := "GetKeyRequest"
	if !cached {
		ne.db.db.locCache.invalidate(begin.Key, NoTenantID, false)
		first = "GetKeyServerLocationsRequest"
	}
	tx, rv := ne.tx(t)
	defer tx.Cancel()
	var r nativeRangeResult
	var err error
	ne.rec.start()
	release := ne.hold()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, err = tx.nativeGetRangeFallback(ne.ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), rv, false)
	}()
	held := ne.awaitRequests(2)
	release()
	<-done
	got := ne.rec.stop()
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	if len(held) != 2 || nrKind(held[0]) != first || nrKind(held[1]) != "GetKeyRequest" {
		return fmt.Errorf("sent before any reply:\n%s\nwant %s for begin %q, then GetKeyRequest(end)", nrDescribe(held), first, begin.Key)
	}
	if q, ok := held[0].(*types.GetKeyRequest); ok && !nrRefIs(q.Sel, begin) {
		return fmt.Errorf("first GetKeyRequest %+v, want begin %+v", q.Sel, begin)
	}
	if q, ok := held[0].(*types.GetKeyServerLocationsRequest); ok && !bytes.Equal(q.Begin, begin.Key) {
		return fmt.Errorf("location request for %q, want begin's %q", q.Begin, begin.Key)
	}
	if q := held[1].(*types.GetKeyRequest); !nrRefIs(q.Sel, end) {
		return fmt.Errorf("second GetKeyRequest %+v, want end %+v", q.Sel, end)
	}
	if err := storageVersions(got.msgs, rv); err != nil {
		return err
	}
	if reqs := nrGetKeyValues(got.msgs); len(reqs) == 0 || !nrRefIs(reqs[0].Begin, firstGreaterOrEqual(b)) {
		return fmt.Errorf("want the exact range from FGE(%q):\n%s", b, nrDescribe(got.msgs))
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(b, e), false); k != want || r.more {
		return fmt.Errorf("result %s more=%t, want %s", k, r.more, want)
	}
	return nil
}

// An FGE begin resolves without a request (:4448-4449), so only end's
// GetKeyRequest is sent, at once.
func (ne *nativeRangeEnv) fallbackOneNontrivial(t *testing.T) error {
	_, end, _, e := ne.fallbackSelectors(t)
	b := ne.keys[1]
	ne.locate(t, end.Key, false)
	tx, rv := ne.tx(t)
	defer tx.Cancel()
	var r nativeRangeResult
	var err error
	ne.rec.start()
	release := ne.hold()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, err = tx.nativeGetRangeFallback(ne.ctx, firstGreaterOrEqual(b), end, newRangeLimits(0, ByteLimitUnlimited), rv, false)
	}()
	held := ne.awaitRequests(1)
	release()
	<-done
	got := ne.rec.stop()
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	if len(held) != 1 || nrKind(held[0]) != "GetKeyRequest" || !nrRefIs(held[0].(*types.GetKeyRequest).Sel, end) {
		return fmt.Errorf("sent before any reply:\n%s\nwant GetKeyRequest(end) alone", nrDescribe(held))
	}
	if n := nrCount(got.msgs, "GetKeyRequest"); n != 1 {
		return fmt.Errorf("%d GetKeyRequests, want 1:\n%s", n, nrDescribe(got.msgs))
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(b, e), false); k != want {
		return fmt.Errorf("result %s, want %s", k, want)
	}
	return nil
}

// FGE and FGT bounds resolve without a request (:4448-4452): the fallback goes
// straight to the exact range [b, keyAfter(e)).
func (ne *nativeRangeEnv) fallbackTrivialBounds(t *testing.T) error {
	b, e := ne.keys[5], ne.keys[60]
	tx, rv := ne.tx(t)
	defer tx.Cancel()
	var r nativeRangeResult
	var err error
	got := nrRecord(ne.rec, func() {
		r, err = tx.nativeGetRangeFallback(ne.ctx, firstGreaterOrEqual(b), firstGreaterThan(e), newRangeLimits(0, ByteLimitUnlimited), rv, false)
	})
	if err != nil {
		return err
	}
	if err := nrProblems(got); err != nil {
		return err
	}
	reqs := nrGetKeyValues(got.msgs)
	if nrCount(got.msgs, "GetKeyRequest") != 0 || len(reqs) == 0 || !nrRefIs(reqs[0].Begin, firstGreaterOrEqual(b)) {
		return fmt.Errorf("want only the exact range from FGE(%q):\n%s", b, nrDescribe(got.msgs))
	}
	if k, want := nrKeys(r.kvs), nrJoin(ne.keysBetween(b, keyAfterBytes(e)), false); k != want {
		return fmt.Errorf("result %s, want %s", k, want)
	}
	return nil
}
