package client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// duplicateServers repeats every cached location's replica list n times, so a
// read hedges and falls back across copies of the one storage server.
func duplicateServers(db *Database, n int) {
	lc := &db.db.locCache
	lc.mu.Lock()
	defer lc.mu.Unlock()
	for i := range lc.entries {
		var servers []ServerInfo
		for range n {
			servers = append(servers, lc.entries[i].servers...)
		}
		lc.entries[i].servers = servers
	}
}

// setServers replaces the cached replica list of every location.
func setServers(db *Database, servers []ServerInfo) {
	lc := &db.db.locCache
	lc.mu.Lock()
	defer lc.mu.Unlock()
	for i := range lc.entries {
		lc.entries[i].servers = servers
	}
}

// alternatingDraws scripts n draws that sample, then not, alternately.
func alternatingDraws(n int) []float64 {
	vals := make([]float64, n)
	for i := 1; i < n; i += 2 {
		vals[i] = drawMiss
	}
	return vals
}

// checkNewRequests requires at least min requests in got, each built anew: one
// draw apiece, so their tags follow alternatingDraws.
func checkNewRequests(t *testing.T, got []taggedRequest, draws, min int, tagged taggedRequest) {
	t.Helper()
	want := make([]taggedRequest, len(got))
	for i := range want {
		if i%2 == 0 {
			want[i] = tagged
		}
	}
	if len(got) < min || fmt.Sprint(got) != fmt.Sprint(want) || draws != len(got) {
		t.Errorf("requests %v after %d draws, want at least %d alternating from tagged, one draw each", got, draws, min)
	}
}

// replyPlan injects faults into replies to requests the recorder saw. Each
// entry, in order, drops (a nil body) or replaces the next reply to a request
// of its kind; it is bound to that reply by token, so coordinator, proxy and
// other replies on the same process's connections pass through.
type replyPlan struct {
	rec     *sendRecorder
	entries []plannedReply
	mu      sync.Mutex
	hit     []string // the kinds of the requests whose replies were injected
}

type plannedReply struct {
	kind string
	body []byte
}

func dropReply(kind string) plannedReply                 { return plannedReply{kind: kind} }
func replaceReply(kind string, body []byte) plannedReply { return plannedReply{kind, body} }

func (p *replyPlan) intercept(_ int, token transport.UID, body []byte) ([]byte, bool) {
	kind := p.rec.replyKind(token)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hit) == len(p.entries) || kind != p.entries[len(p.hit)].kind {
		return body, false
	}
	e := p.entries[len(p.hit)]
	p.hit = append(p.hit, kind)
	if e.body == nil {
		return nil, true
	}
	return e.body, false
}

// waitInjected waits until n entries have been injected.
func (p *replyPlan) waitInjected(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for {
		p.mu.Lock()
		hit := len(p.hit)
		p.mu.Unlock()
		if hit >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d of %d planned faults injected", hit, n)
		case <-time.After(time.Millisecond):
		}
	}
}

// check requires every entry to have been injected into a reply of its kind.
func (p *replyPlan) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	want := make([]string, len(p.entries))
	for i, e := range p.entries {
		want[i] = e.kind
	}
	if fmt.Sprint(p.hit) != fmt.Sprint(want) {
		t.Errorf("injected into replies to %v, want %v", p.hit, want)
	}
}

// deadEndpoint is a replica entry at addr whose stream token the server does
// not have: it answers ENDPOINT_NOT_FOUND, which loadBalance treats as a
// failed alternative and retries with the same request.
func deadEndpoint(addr string) ServerInfo {
	return ServerInfo{Address: addr, Token: transport.UID{First: transport.NewUID().First | 1, Second: transport.NewUID().Second}}
}

// One draw per native request: loadBalance sends it to every alternative and
// hedge (LoadBalance.actor.h:446,662,689), and a reply timeout or failed
// endpoint resends it; C++'s own read loop builds a new one, after
// wrong_shard_server or all_alternatives_failed and for every continuation.
// Scripted draws alternate sample, no sample, sample, ..., so a second draw
// for one request, or a kept draw for a new one, shows in the tags.
func TestReadTagSamplingOneDrawPerRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cf := startProxyFDB(t, ctx)
	prefix := t.Name() + "/"
	mapped := mappedFixture{prefix: []byte(prefix + "mapped"), n: 24}
	{
		db := newTestDatabase(t, ctx, cf, plainDial)
		for _, k := range []string{"a", "b", "c"} {
			setKey(t, ctx, db, []byte(prefix+k), []byte("v"))
		}
		// About 200 KB of index entries: a mapped scan takes several replies.
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := range mapped.n {
				tx.Set(mapped.indexEntryKey(i), bytes.Repeat([]byte{'p'}, 8*1024))
				tx.Set(mapped.recordKey(i), []byte(fmt.Sprintf("record-%d", i)))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed mapped: %v", err)
		}
		db.Close()
	}
	key, begin, end := []byte(prefix+"a"), []byte(prefix), []byte(prefix+"\xff")
	mappedBegin, mappedEnd := mapped.indexRange()
	wrongShard := inlineErrorReply(uint16(ErrWrongShardServer), injectPenalty)
	tagged := taggedRequest{has: true, tags: string(encodeTagSet([]string{"rt"}))}
	untagged := taggedRequest{}

	type env struct {
		aliases sync.Map // dial address → real address
		db      *Database
		tx      *Transaction
		rec     *sendRecorder
		sim     *simDialer
		draws   *readTagDraws
		plan    *replyPlan
		rv      int64
		addr    string
	}
	setup := func(t *testing.T) *env {
		e := &env{sim: newSimDialer(), draws: &readTagDraws{}}
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			if real, ok := e.aliases.Load(addr); ok {
				addr = real.(string)
			}
			return e.sim.dial(ctx, network, addr)
		}
		e.db, e.rec = openReadTagDB(t, ctx, cf, dial, 0.5, e.draws.draw)
		e.tx = e.db.CreateTransaction()
		t.Cleanup(e.tx.Cancel)
		if err := e.tx.SetAutoThrottleTag("rt"); err != nil {
			t.Fatal(err)
		}
		var err error
		if e.rv, err = e.tx.GetReadVersion(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.db.locCache.locateRange(e.db.db, ctx, begin, end, 100, false, NoTenantID, types.SpanContext{}); err != nil {
			t.Fatal(err)
		}
		e.addr = storageAddrFor(t, e.db, ctx, key)
		return e
	}
	// start arms the plan and starts recording, then makes a read version
	// request on the same process's connection: a fault not bound to its
	// request's reply would land on that reply first.
	start := func(t *testing.T, e *env, entries ...plannedReply) {
		t.Helper()
		e.plan = &replyPlan{rec: e.rec, entries: entries}
		e.sim.setIntercept(e.plan.intercept)
		e.sim.armAddr(e.addr)
		e.rec.start()
		decoyCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		_, _ = e.db.CreateTransaction().GetReadVersion(decoyCtx)
		e.draws.script(alternatingDraws(100)...)
	}
	requests := func(t *testing.T, e *env, kind string) []taggedRequest {
		t.Helper()
		var got []taggedRequest
		for _, r := range taggedRequests(t, e.rec.stop()) {
			if r.kind == kind {
				got = append(got, taggedRequest{has: r.has, tags: r.tags})
			}
		}
		e.plan.check(t)
		return got
	}
	check := func(t *testing.T, e *env, kind string, draws int, want ...taggedRequest) {
		t.Helper()
		got := requests(t, e, kind)
		if fmt.Sprint(got) != fmt.Sprint(want) || e.draws.count() != draws {
			t.Errorf("%s requests %v after %d draws, want %v after %d", kind, got, e.draws.count(), want, draws)
		}
	}

	reads := []struct {
		kind string
		read func(e *env) error
	}{
		{"GetValueRequest", func(e *env) error { _, err := e.tx.getValue(ctx, key); return err }},
		{"GetKeyRequest", func(e *env) error { _, _, err := e.tx.getKeyImpl(ctx, e.rv, key, false, 1); return err }},
		{"GetKeyValuesRequest", func(e *env) error {
			_, _, _, err := e.tx.getRangeImpl(ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), e.rv, false)
			return err
		}},
		{"GetMappedKeyValuesRequest", func(e *env) error {
			_, _, _ = e.tx.getMappedRangeImpl(ctx, begin, end, []byte("\x01m\x00"), 0, ByteLimitUnlimited, e.rv, false)
			return nil // storage may reject the mapper; the requests are under test
		}},
	}
	for _, rd := range reads {
		t.Run(rd.kind, func(t *testing.T) {
			for _, sc := range []struct {
				name    string
				prepare func(e *env)
				fault   []byte
				draws   int
				want    []taggedRequest
			}{
				// The primary's reply is lost, so the paid hedge to a second replica wins.
				{"hedge", func(e *env) { duplicateServers(e.db, 2); setSecondBudget(e.db.db.queueModel, 1) }, nil, 1, []taggedRequest{tagged, tagged}},
				{"reply-timeout", func(e *env) { e.tx.rpcTimeoutOverride = 300 * time.Millisecond }, nil, 1, []taggedRequest{tagged, tagged}},
				{"wrong-shard", func(*env) {}, wrongShard, 2, []taggedRequest{tagged, untagged}},
			} {
				t.Run(sc.name, func(t *testing.T) {
					e := setup(t)
					sc.prepare(e)
					start(t, e, replaceReply(rd.kind, sc.fault))
					if err := rd.read(e); err != nil {
						t.Fatalf("read: %v", err)
					}
					check(t, e, rd.kind, sc.draws, sc.want...)
				})
			}
		})
	}

	t.Run("GetValueRequest/all-alternatives-failed", func(t *testing.T) {
		e := setup(t)
		start(t, e, replaceReply("GetValueRequest", inlineErrorReply(uint16(ErrAllAlternativesFailed), injectPenalty)))
		if _, err := e.tx.getValue(ctx, key); err != nil {
			t.Fatalf("getValue: %v", err)
		}
		check(t, e, "GetValueRequest", 2, tagged, untagged)
	})

	// Both the primary and the paid hedge time out; the sequential fallback tries
	// the remaining replica with the same request (resolveFallback).
	t.Run("GetValueRequest/alternatives", func(t *testing.T) {
		e := setup(t)
		duplicateServers(e.db, 3)
		setSecondBudget(e.db.db.queueModel, 1)
		e.tx.rpcTimeoutOverride = 300 * time.Millisecond
		start(t, e, dropReply("GetValueRequest"), dropReply("GetValueRequest"))
		if _, err := e.tx.getValue(ctx, key); err != nil {
			t.Fatalf("getValue: %v", err)
		}
		check(t, e, "GetValueRequest", 1, tagged, tagged, tagged)
	})

	// A pipelined read continued after a failure loadBalance retries itself (a
	// dead endpoint, a reply timeout, a connection closed before its flush)
	// keeps its draw; wrong_shard_server draws again.
	pipelined := []struct {
		name    string
		prepare func(e *env)
		plan    []plannedReply
		resolve func(t *testing.T, e *env)
		draws   int
		want    []taggedRequest
	}{
		{"pipelined-dead-endpoint", func(e *env) {
			loc, _ := e.db.db.locCache.locate(e.db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
			setServers(e.db, []ServerInfo{deadEndpoint(e.addr), loc.Servers[0]})
		}, nil, nil, 1, []taggedRequest{tagged, tagged}},
		{"pipelined-reply-timeout", func(e *env) { e.tx.rpcTimeoutOverride = 300 * time.Millisecond }, []plannedReply{dropReply("GetValueRequest")}, nil, 1, []taggedRequest{tagged, tagged}},
		// The read goes to the first replica, an alias of the storage server; its
		// reply lost, that connection closes before Resolve flushes it, so the
		// flush fails and the reply carries the connection's failure. The
		// fallback read goes to the other replica.
		{"pipelined-flush-error", func(e *env) {
			loc, _ := e.db.db.locCache.locate(e.db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
			alias := "pipelined-alias.invalid:1"
			e.aliases.Store(alias, e.addr)
			setServers(e.db, []ServerInfo{{Address: alias, Token: loc.Servers[0].Token}, loc.Servers[0]})
		}, []plannedReply{dropReply("GetValueRequest")}, func(t *testing.T, e *env) {
			e.plan.waitInjected(t, ctx, 1) // the reply is gone before the close
			conn, err := e.db.db.getOrDial(ctx, "pipelined-alias.invalid:1")
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		}, 1, []taggedRequest{tagged, tagged}},
		{"pipelined-wrong-shard", func(*env) {}, []plannedReply{replaceReply("GetValueRequest", wrongShard)}, nil, 2, []taggedRequest{tagged, untagged}},
	}
	for _, sc := range pipelined {
		t.Run("GetValueRequest/"+sc.name, func(t *testing.T) {
			e := setup(t)
			sc.prepare(e)
			start(t, e, sc.plan...)
			_, pending, err := e.tx.GetPipelined(ctx, key)
			if err != nil || pending == nil {
				t.Fatalf("GetPipelined: %v %v", pending, err)
			}
			if sc.resolve != nil {
				sc.resolve(t, e)
			}
			if _, err := pending.Resolve(); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			check(t, e, "GetValueRequest", sc.draws, sc.want...)
		})
	}

	// Continuations are new requests, each with its own draw.
	continuations := []struct {
		name string
		kind string
		plan []plannedReply
		read func(e *env) error
	}{
		// A one-byte target returns one row with more set; three rows are needed.
		{"GetKeyValuesRequest/more", "GetKeyValuesRequest", nil, func(e *env) error {
			_, _, _, err := e.tx.getRangeImpl(ctx, begin, end, rangeLimits{rows: -1, minRows: 3, bytes: 1}, e.rv, false)
			return err
		}},
		{"GetMappedKeyValuesRequest/selector-more", "GetMappedKeyValuesRequest", nil, func(e *env) error {
			_, err := e.tx.getMappedRangeSelectors(ctx, firstGreaterOrEqual(mappedBegin), firstGreaterOrEqual(mappedEnd), mapped.getValueMapper(), newMappedRangeLimits(0, ByteLimitUnlimited), e.rv, false)
			return err
		}},
		// wrong_shard_server on the selector request falls back to the exact
		// loop, which continues while storage reports more.
		{"GetMappedKeyValuesRequest/exact-fallback", "GetMappedKeyValuesRequest", []plannedReply{replaceReply("GetMappedKeyValuesRequest", wrongShard)}, func(e *env) error {
			_, err := e.tx.getMappedRangeSelectors(ctx, firstGreaterOrEqual(mappedBegin), firstGreaterOrEqual(mappedEnd), mapped.getValueMapper(), newMappedRangeLimits(0, ByteLimitUnlimited), e.rv, false)
			return err
		}},
	}
	for _, sc := range continuations {
		t.Run(sc.name, func(t *testing.T) {
			e := setup(t)
			start(t, e, sc.plan...)
			if err := sc.read(e); err != nil {
				t.Fatalf("read: %v", err)
			}
			min := 2
			if len(sc.plan) > 0 {
				min = 3 // the selector request, then at least two exact ones
			}
			checkNewRequests(t, requests(t, e, sc.kind), e.draws.count(), min, tagged)
		})
	}

	// A watch registered at "setup", then tagged "commit" before its commit and
	// "after" once committed, sends the set as of the commit
	// (setupWatches, NativeAPI.actor.cpp:6418-6432) on every request: to each
	// alternative of one iteration, and after wrong_shard_server, where the
	// next iteration draws again (:3970).
	watch := func(t *testing.T, e *env, wkey []byte) func() error {
		t.Helper()
		if err := e.tx.SetAutoThrottleTag("setup"); err != nil {
			t.Fatal(err)
		}
		value, rv, span, wctx, wcancel, err := e.tx.WatchSetup(ctx, wkey)
		if err != nil {
			t.Fatalf("WatchSetup: %v", err)
		}
		act := e.tx.WatchActivationFor(wctx)
		if err := e.tx.SetAutoThrottleTag("commit"); err != nil {
			t.Fatal(err)
		}
		e.tx.Set([]byte(prefix+"other"), []byte("x"))
		if err := e.tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := e.tx.SetAutoThrottleTag("after"); err != nil {
			t.Fatal(err)
		}
		version, err := e.tx.AwaitWatchCommit(wctx, act, rv)
		if err != nil {
			t.Fatalf("AwaitWatchCommit: %v", err)
		}
		// Change the key first, so every watch request is answered at once.
		setKey(t, ctx, e.db, wkey, []byte("changed"))
		return func() error { return e.tx.WatchPoll(wctx, wcancel, wkey, value, version, span) }
	}
	committed := taggedRequest{has: true, tags: string(encodeTagSet([]string{"rt", "setup", "commit"}))}

	t.Run("WatchValueRequest/alternatives", func(t *testing.T) {
		e := setup(t)
		wkey := []byte(prefix + "watch-alternatives")
		poll := watch(t, e, wkey)
		loc, err := e.db.db.locCache.locate(e.db.db, ctx, wkey, NoTenantID, types.SpanContext{}, false)
		if err != nil {
			t.Fatal(err)
		}
		// The dead endpoint is at an alias address that dials the storage
		// server: its queue is empty, so loadBalance tries it first.
		alias := "dead-endpoint.invalid:1"
		e.aliases.Store(alias, e.addr)
		e.db.db.queueModel.startRequest(e.addr)
		setServers(e.db, []ServerInfo{deadEndpoint(alias), loc.Servers[0]})
		start(t, e)
		if err := poll(); err != nil {
			t.Fatalf("WatchPoll: %v", err)
		}
		check(t, e, "WatchValueRequest", 1, committed, committed)
	})

	t.Run("WatchValueRequest/wrong-shard", func(t *testing.T) {
		e := setup(t)
		wkey := []byte(prefix + "watch-wrong-shard")
		poll := watch(t, e, wkey)
		start(t, e, replaceReply("WatchValueRequest", (&types.ErrorOrError{ErrorCode: uint16(ErrWrongShardServer)}).MarshalFDB()))
		if err := poll(); err != nil {
			t.Fatalf("WatchPoll: %v", err)
		}
		check(t, e, "WatchValueRequest", 2, committed, untagged)
	})

	// Reset before the commit abandons the watch: it never sends a request.
	t.Run("WatchValueRequest/reset-before-commit", func(t *testing.T) {
		e := setup(t)
		wkey := []byte(prefix + "watch-reset")
		value, rv, span, wctx, wcancel, err := e.tx.WatchSetup(ctx, wkey)
		if err != nil {
			t.Fatalf("WatchSetup: %v", err)
		}
		act := e.tx.WatchActivationFor(wctx)
		start(t, e)
		e.tx.Reset()
		if _, err := e.tx.AwaitWatchCommit(wctx, act, rv); err == nil {
			_ = e.tx.WatchPoll(wctx, wcancel, wkey, value, rv, span)
			t.Fatal("a watch reset before its commit was activated")
		}
		check(t, e, "WatchValueRequest", 0)
	})
}

// Partial getKey resolutions and range reads crossing shards are new requests,
// each with its own draw (NativeAPI.actor.cpp:3806, 4303). Needs real shard
// boundaries; hedging is off so every request is a draw's only one.
func TestReadTagSamplingShardContinuations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	env := setupMultiShardEnv(t, ctx)
	if err := crossShardPrecondition(env.numShards); err != nil {
		t.Fatal(err)
	}
	connStr, err := env.container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatal(err)
	}
	tagged := taggedRequest{has: true, tags: string(encodeTagSet([]string{"rt"}))}
	begin, end := []byte(env.prefix), []byte(env.prefix+"\xff")
	for _, tc := range []struct {
		kind string
		read func(tx *Transaction, rv int64) error
	}{
		// The 100th key from the first crosses every shard boundary in the prefix.
		{"GetKeyRequest", func(tx *Transaction, rv int64) error {
			_, _, err := tx.getKeyImpl(ctx, rv, []byte(env.prefix+"0000"), false, 100)
			return err
		}},
		{"GetKeyValuesRequest", func(tx *Transaction, rv int64) error {
			_, _, _, err := tx.getRangeImpl(ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), rv, false)
			return err
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			draws := &readTagDraws{}
			db, rec := openReadTagDB(t, ctx, cf, plainDial, 0.5, draws.draw)
			db.db.hedgeEnabled.Store(false)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			if err := tx.SetAutoThrottleTag("rt"); err != nil {
				t.Fatal(err)
			}
			rv, err := tx.GetReadVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.locCache.locateRange(db.db, ctx, begin, end, 100, false, NoTenantID, types.SpanContext{}); err != nil {
				t.Fatal(err)
			}
			rec.start()
			draws.script(alternatingDraws(100)...)
			if err := tc.read(tx, rv); err != nil {
				t.Fatalf("read: %v", err)
			}
			var got []taggedRequest
			for _, r := range taggedRequests(t, rec.stop()) {
				if r.kind == tc.kind {
					got = append(got, taggedRequest{has: r.has, tags: r.tags})
				}
			}
			checkNewRequests(t, got, draws.count(), 2, tagged)
		})
	}
	for _, route := range []string{"native-range", "native-fallback-partial-getKey"} {
		t.Run(route, func(t *testing.T) {
			draws := &readTagDraws{}
			db, rec := openReadTagDB(t, ctx, cf, plainDial, 0.5, draws.draw)
			db.db.hedgeEnabled.Store(false)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			if err := tx.SetAutoThrottleTag("rt"); err != nil {
				t.Fatal(err)
			}
			rv, err := tx.GetReadVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.locCache.locateRange(db.db, ctx, begin, end, 100, false, NoTenantID, types.SpanContext{}); err != nil {
				t.Fatal(err)
			}
			draws.script(alternatingDraws(100)...)
			rec.start()
			if route == "native-range" {
				r, err := tx.nativeGetRange(ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), newRangeLimits(0, -1), true, false)
				if err != nil || len(r.kvs) != 100 {
					t.Fatalf("native range = %d rows, %v; want 100", len(r.kvs), err)
				}
			} else {
				r, err := tx.nativeGetRangeFallback(ctx, KeySelector{Key: []byte(env.prefix + "0000"), Offset: 100}, firstGreaterOrEqual(end), newRangeLimits(0, -1), rv, false)
				if err != nil || len(r.kvs) != 1 || string(r.kvs[0].Key) != env.prefix+"0099" {
					t.Fatalf("fallback partial resolution = %+v, %v; want key 0099", r.kvs, err)
				}
			}
			got := taggedRequests(t, rec.stop())
			kinds := map[string]int{}
			for i := range got {
				kinds[got[i].kind]++
				got[i].kind = ""
			}
			if route == "native-fallback-partial-getKey" && (kinds["GetKeyRequest"] < 2 || kinds["GetKeyValuesRequest"] < 1) {
				t.Fatalf("fallback request kinds %v, want multiple getKey steps and an exact range", kinds)
			}
			if route == "native-range" && kinds["GetKeyValuesRequest"] < 2 {
				t.Fatalf("native request kinds %v, want multiple shards", kinds)
			}
			checkNewRequests(t, got, draws.count(), 2, tagged)
		})
	}
}
