package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
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

// replyPlan drops or replaces storage replies by their order on the armed
// connections; replies past the plan pass through.
func replyPlan(plan ...[]byte) frameIntercept {
	var n atomic.Int64
	return func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		i := int(n.Add(1) - 1)
		if i >= len(plan) {
			return body, false
		}
		if plan[i] == nil {
			return nil, true
		}
		return plan[i], false
	}
}

var dropReply []byte // a nil entry in a replyPlan drops that reply

// deadEndpoint is a replica entry at addr whose stream token the server does
// not have: it answers ENDPOINT_NOT_FOUND, which loadBalance treats as a
// failed alternative and retries with the same request.
func deadEndpoint(addr string) ServerInfo {
	return ServerInfo{Address: addr, Token: transport.UID{First: transport.NewUID().First | 1, Second: transport.NewUID().Second}}
}

// One draw per native request: loadBalance sends it to every alternative and
// hedge (LoadBalance.actor.h:446,662,689), a reply timeout or failed endpoint
// resends it, and only C++'s own read loop (wrong_shard_server) builds a new
// one. The script makes the first draw sample and every later one not, so a
// second draw shows as a request without tags.
func TestReadTagSamplingOneDrawPerRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cf := startProxyFDB(t, ctx)
	prefix := t.Name() + "/"
	{
		db := newTestDatabase(t, ctx, cf, plainDial)
		for _, k := range []string{"a", "b", "c"} {
			setKey(t, ctx, db, []byte(prefix+k), []byte("v"))
		}
		db.Close()
	}
	key, begin, end := []byte(prefix+"a"), []byte(prefix), []byte(prefix+"\xff")
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
	check := func(t *testing.T, e *env, reqs []taggedRequest, kind string, draws int, want ...taggedRequest) {
		t.Helper()
		var got []taggedRequest
		for _, r := range reqs {
			if r.kind == kind {
				got = append(got, taggedRequest{has: r.has, tags: r.tags})
			}
		}
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
				plan    [][]byte
				draws   int
				want    []taggedRequest
			}{
				// The primary's reply is lost, so the hedge to a second replica wins.
				{"hedge", func(e *env) { duplicateServers(e.db, 2) }, [][]byte{dropReply}, 1, []taggedRequest{tagged, tagged}},
				{"reply-timeout", func(e *env) { e.tx.rpcTimeoutOverride = 300 * time.Millisecond }, [][]byte{dropReply}, 1, []taggedRequest{tagged, tagged}},
				{"wrong-shard", func(*env) {}, [][]byte{wrongShard}, 2, []taggedRequest{tagged, untagged}},
			} {
				t.Run(sc.name, func(t *testing.T) {
					e := setup(t)
					sc.prepare(e)
					e.sim.setIntercept(replyPlan(sc.plan...))
					e.sim.armAddr(e.addr)
					e.draws.script(drawHit)
					e.rec.start()
					if err := rd.read(e); err != nil {
						t.Fatalf("read: %v", err)
					}
					check(t, e, taggedRequests(t, e.rec.stop()), rd.kind, sc.draws, sc.want...)
				})
			}
		})
	}

	// Both the primary and the hedge time out; the sequential fallback tries the
	// remaining replica with the same request (resolveFallback).
	t.Run("GetValueRequest/alternatives", func(t *testing.T) {
		e := setup(t)
		duplicateServers(e.db, 3)
		e.tx.rpcTimeoutOverride = 300 * time.Millisecond
		e.sim.setIntercept(replyPlan(dropReply, dropReply))
		e.sim.armAddr(e.addr)
		e.draws.script(drawHit)
		e.rec.start()
		if _, err := e.tx.getValue(ctx, key); err != nil {
			t.Fatalf("getValue: %v", err)
		}
		check(t, e, taggedRequests(t, e.rec.stop()), "GetValueRequest", 1, tagged, tagged, tagged)
	})

	// A pipelined read continued after a failure loadBalance retries itself (a
	// dead endpoint, a reply timeout) keeps its draw; wrong_shard_server draws again.
	pipelined := []struct {
		name    string
		prepare func(e *env)
		plan    [][]byte
		draws   int
		want    []taggedRequest
	}{
		{"pipelined-dead-endpoint", func(e *env) {
			loc, _ := e.db.db.locCache.locate(e.db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
			setServers(e.db, []ServerInfo{deadEndpoint(e.addr), loc.Servers[0]})
		}, nil, 1, []taggedRequest{tagged, tagged}},
		{"pipelined-reply-timeout", func(e *env) { e.tx.rpcTimeoutOverride = 300 * time.Millisecond }, [][]byte{dropReply}, 1, []taggedRequest{tagged, tagged}},
		{"pipelined-wrong-shard", func(*env) {}, [][]byte{wrongShard}, 2, []taggedRequest{tagged, untagged}},
	}
	for _, sc := range pipelined {
		t.Run("GetValueRequest/"+sc.name, func(t *testing.T) {
			e := setup(t)
			sc.prepare(e)
			e.sim.setIntercept(replyPlan(sc.plan...))
			e.sim.armAddr(e.addr)
			e.draws.script(drawHit)
			e.rec.start()
			_, pending, err := e.tx.GetPipelined(ctx, key)
			if err != nil || pending == nil {
				t.Fatalf("GetPipelined: %v %v", pending, err)
			}
			if _, err := pending.Resolve(); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			check(t, e, taggedRequests(t, e.rec.stop()), "GetValueRequest", sc.draws, sc.want...)
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
		e.draws.script(drawHit)
		e.rec.start()
		if err := poll(); err != nil {
			t.Fatalf("WatchPoll: %v", err)
		}
		check(t, e, taggedRequests(t, e.rec.stop()), "WatchValueRequest", 1, committed, committed)
	})

	t.Run("WatchValueRequest/wrong-shard", func(t *testing.T) {
		e := setup(t)
		wkey := []byte(prefix + "watch-wrong-shard")
		poll := watch(t, e, wkey)
		e.sim.setIntercept(replyPlan((&types.ErrorOrError{ErrorCode: uint16(ErrWrongShardServer)}).MarshalFDB()))
		e.sim.armAddr(e.addr)
		e.draws.script(drawHit)
		e.rec.start()
		if err := poll(); err != nil {
			t.Fatalf("WatchPoll: %v", err)
		}
		check(t, e, taggedRequests(t, e.rec.stop()), "WatchValueRequest", 2, committed, untagged)
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
		e.draws.script(drawHit)
		e.rec.start()
		e.tx.Reset()
		if _, err := e.tx.AwaitWatchCommit(wctx, act, rv); err == nil {
			_ = e.tx.WatchPoll(wctx, wcancel, wkey, value, rv, span)
			t.Fatal("a watch reset before its commit was activated")
		}
		check(t, e, taggedRequests(t, e.rec.stop()), "WatchValueRequest", 0)
	})
}
