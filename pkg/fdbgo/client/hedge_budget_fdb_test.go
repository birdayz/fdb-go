package client

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// recordedCount is the number of requests of kind recorded in rec's window.
func recordedCount(rec *sendRecorder, kind string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, r := range rec.rec.requests {
		if strings.HasPrefix(r, kind+" ") {
			n++
		}
	}
	return n
}

// holdFirstReply holds the first reply to a request of kind until release
// returns, then forwards it; the connection's later replies wait behind it.
type holdFirstReply struct {
	rec     *sendRecorder
	kind    string
	release func()
	held    atomic.Bool
}

func (h *holdFirstReply) intercept(_ int, token transport.UID, body []byte) ([]byte, bool) {
	if h.rec.replyKind(token) == h.kind && h.held.CompareAndSwap(false, true) {
		h.release()
	}
	return body, false
}

// A read with two replicas whose first reply is slow sends a second request
// only when the database's second-request budget pays for it
// (LoadBalance.actor.h:710-716). A new database has no budget
// (QueueModel.h:121), so it sends one request per read, as libfdb_c does.
func TestHedgeBudgetRequests(t *testing.T) {
	t.Parallel()
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancelSeed)
	prefix := t.Name() + "/"
	mapped := mappedFixture{prefix: []byte(prefix + "mapped"), n: 2}
	seed := newTestDatabase(t, seedCtx, sharedClusterFile, plainDial)
	for _, k := range []string{"a", "b", "c"} {
		setKey(t, seedCtx, seed, []byte(prefix+k), []byte("v"))
	}
	mapped.seed(t, seedCtx, seed, 0, func(i int) []byte { return []byte(fmt.Sprintf("record-%d", i)) })
	seed.Close()
	cancelSeed()
	key, begin, end := []byte(prefix+"a"), []byte(prefix), []byte(prefix+"\xff")
	mappedBegin, mappedEnd := mapped.indexRange()

	type env struct {
		tx  *Transaction
		q   *QueueModel
		rec *sendRecorder
		sim *simDialer
		rv  int64
	}
	setup := func(t *testing.T, ctx context.Context) *env {
		t.Helper()
		e := &env{sim: newSimDialer()}
		aliases := &sync.Map{}
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			if real, ok := aliases.Load(addr); ok {
				addr = real.(string)
			}
			return e.sim.dial(ctx, network, addr)
		}
		e.rec = &sendRecorder{}
		db := newTestDatabase(t, ctx, sharedClusterFile, e.rec.wrap(dial))
		t.Cleanup(func() { db.Close() })
		e.q = db.db.queueModel
		e.tx = db.CreateTransaction()
		t.Cleanup(e.tx.Cancel)
		e.tx.rpcTimeoutOverride = time.Minute // no reply-timeout resend within a hold
		var err error
		if e.rv, err = e.tx.GetReadVersion(ctx); err != nil {
			t.Fatal(err)
		}
		for _, r := range [][2][]byte{{begin, end}, {mappedBegin, mappedEnd}} {
			if _, err := db.db.locCache.locateRange(db.db, ctx, r[0], r[1], 100, false, NoTenantID, types.SpanContext{}); err != nil {
				t.Fatal(err)
			}
		}
		// Two replicas, each an alias of the one storage server on its own connection.
		real := storageAddrFor(t, db, ctx, key)
		loc, _ := db.db.locCache.locate(db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
		var servers []ServerInfo
		for _, alias := range []string{"hedge-replica-1.invalid:1", "hedge-replica-2.invalid:1"} {
			aliases.Store(alias, real)
			servers = append(servers, ServerInfo{Address: alias, Token: loc.Servers[0].Token})
		}
		setServers(db, servers)
		e.sim.armEverything()
		return e
	}
	reads := []struct {
		kind string
		read func(ctx context.Context, e *env) error
	}{
		{"GetValueRequest", func(ctx context.Context, e *env) error { _, err := e.tx.getValue(ctx, key); return err }},
		{"GetKeyRequest", func(ctx context.Context, e *env) error {
			_, _, err := e.tx.getKeyImpl(ctx, e.rv, key, false, 1)
			return err
		}},
		{"GetKeyValuesRequest", func(ctx context.Context, e *env) error {
			_, _, _, err := e.tx.getRangeImpl(ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), e.rv, false)
			return err
		}},
		{"GetMappedKeyValuesRequest", func(ctx context.Context, e *env) error {
			rows, _, err := e.tx.getMappedRangeImpl(ctx, mappedBegin, mappedEnd, mapped.getValueMapper(), 0, ByteLimitUnlimited, e.rv, false)
			if err == nil && len(rows) != mapped.n {
				return fmt.Errorf("%d mapped rows, want %d", len(rows), mapped.n)
			}
			return err
		}},
	}
	// read runs one read with the first reply of its kind held by release, and
	// returns how many requests of that kind it sent.
	read := func(t *testing.T, ctx context.Context, e *env, kind string, do func(context.Context, *env) error, release func()) int {
		t.Helper()
		e.sim.setIntercept((&holdFirstReply{rec: e.rec, kind: kind, release: release}).intercept)
		e.rec.start()
		err := do(ctx, e)
		n := recordedCount(e.rec, kind)
		e.rec.stop()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return n
	}
	// Far past the 1.5 ms delay of a new model: the delay fires first. That no
	// second request follows does not depend on it; the budget is 0.
	pastDelay := func() { time.Sleep(100 * time.Millisecond) }
	// Until the second request is sent: the delay always fires first.
	untilSent := func(e *env, kind string, n int) func() {
		return func() {
			for deadline := time.Now().Add(30 * time.Second); recordedCount(e.rec, kind) < n && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		}
	}
	for _, rd := range reads {
		t.Run(rd.kind, func(t *testing.T) {
			t.Parallel()
			t.Run("new-database", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				e := setup(t, ctx)
				if n := read(t, ctx, e, rd.kind, rd.read, pastDelay); n != 1 {
					t.Fatalf("a new database sent %d %s for one slow read, want 1", n, rd.kind)
				}
				checkSecondModel(t, e.q, 0, 1, false)
			})
			t.Run("budget", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				e := setup(t, ctx)
				setSecondBudget(e.q, 1)
				if n := read(t, ctx, e, rd.kind, rd.read, untilSent(e, rd.kind, 2)); n != 2 {
					t.Fatalf("with budget 1.0 a slow read sent %d %s, want the request and its hedge", n, rd.kind)
				}
				checkSecondModel(t, e.q, 1, 0, true)
			})
			// Hedging off sends no second request, and the reply still earns budget,
			// which the next read spends once hedging is on again.
			t.Run("disabled", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				e := setup(t, ctx)
				setSecondBudget(e.q, 1)
				e.tx.db.hedgeEnabled.Store(false)
				if n := read(t, ctx, e, rd.kind, rd.read, pastDelay); n != 1 {
					t.Fatalf("hedging off, a slow read sent %d %s, want 1", n, rd.kind)
				}
				checkSecondModel(t, e.q, 1, 1, false)
				e.tx.db.hedgeEnabled.Store(true)
				if n := read(t, ctx, e, rd.kind, rd.read, untilSent(e, rd.kind, 2)); n != 2 {
					t.Fatalf("hedging on again, a slow read sent %d %s, want 2", n, rd.kind)
				}
				checkSecondModel(t, e.q, 1.05, 0, true)
			})
		})
	}
}

// With no budget the backup was never sent the read, so getValue's sequential
// fallback tries it after the primary's reply timeout, and its reply earns
// budget like any lone request's.
func TestGetValueFallbackTriesUnhedgedBackup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := []byte(t.Name())
	seed := newTestDatabase(t, ctx, sharedClusterFile, plainDial)
	setKey(t, ctx, seed, key, []byte("v"))
	seed.Close()

	const lost, backup = "fallback-lost.invalid:1", "fallback-backup.invalid:1"
	sim, aliases := newSimDialer(), &sync.Map{}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		real, ok := aliases.Load(addr)
		if !ok {
			return sim.dial(ctx, network, addr)
		}
		c, err := sim.dial(ctx, network, real.(string))
		if err == nil && addr == lost {
			c.(*simConn).armed.Store(true)
		}
		return c, err
	}
	rec := &sendRecorder{}
	db := newTestDatabase(t, ctx, sharedClusterFile, rec.wrap(dial))
	defer db.Close()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.rpcTimeoutOverride = 300 * time.Millisecond
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	real := storageAddrFor(t, db, ctx, key)
	loc, _ := db.db.locCache.locate(db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
	aliases.Store(lost, real)
	aliases.Store(backup, real)
	servers := []ServerInfo{{Address: lost, Token: loc.Servers[0].Token}, {Address: backup, Token: loc.Servers[0].Token}}
	setServers(db, servers)
	// The backup's outstanding request makes the lost replica the primary of every
	// selection, so a retry instead of the fallback would hit it again.
	q := db.db.queueModel
	delta := q.startRequest(backup)
	defer q.endRequest(backup, delta, 0, false)
	q.mu.Lock()
	q.getOrCreate(backup).smoothOutstanding.estimate = 1
	q.mu.Unlock()
	if best, next := q.chooseTopTwo(servers); best != 0 || next != 1 {
		t.Fatalf("selection = %d,%d; want the lost replica first", best, next)
	}

	var dropped atomic.Int32
	sim.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
		if rec.replyKind(token) == "GetValueRequest" {
			dropped.Add(1)
			return nil, true
		}
		return body, false
	})
	rec.start()
	v, err := tx.getValue(ctx, key)
	n := recordedCount(rec, "GetValueRequest")
	rec.stop()
	if err != nil || string(v) != "v" {
		t.Fatalf("getValue = %q, %v; want v", v, err)
	}
	if n != 2 || dropped.Load() != 1 {
		t.Fatalf("%d GetValueRequests with %d lost; want the primary's and the fallback's, one lost", n, dropped.Load())
	}
	checkSecondModel(t, q, 0, 1, false)
}

// watchValue's loadBalance has no QueueModel (NativeAPI.actor.cpp:3962-3972): a
// watch leaves the storage server's latency, its outstanding count and the
// second-request model as they were.
func TestWatchLeavesQueueModel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := []byte(t.Name())
	rec := &sendRecorder{}
	db := newTestDatabase(t, ctx, sharedClusterFile, rec.wrap(plainDial))
	defer db.Close()
	setKey(t, ctx, db, key, []byte("v1"))
	addr := storageAddrFor(t, db, ctx, key)
	type snapshot struct{ latency, outstanding, budget, multiplier float64 }
	snap := func() snapshot {
		q := db.db.queueModel
		q.mu.Lock()
		defer q.mu.Unlock()
		d := q.getOrCreate(addr)
		return snapshot{d.latency, d.smoothOutstanding.total, q.secondBudget, q.secondMultiplier}
	}

	rec.start()
	watched := make(chan error, 1)
	go func() {
		tx := db.CreateTransaction()
		defer tx.Cancel()
		watched <- tx.Watch(ctx, key)
	}()
	waitForRecorded(t, ctx, rec, "WatchValueRequest")
	rec.stop()
	before := snap()
	time.Sleep(50 * time.Millisecond) // a watch duration no read would take
	setKey(t, ctx, db, key, []byte("v2"))
	if err := <-watched; err != nil {
		t.Fatalf("watch: %v", err)
	}
	if after := snap(); after != before {
		t.Fatalf("a watch changed the QueueModel: %+v, was %+v", after, before)
	}
}
