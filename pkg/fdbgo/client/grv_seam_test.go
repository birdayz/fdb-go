package client

// Every fence property in this package was previously asserted by calling
// grvCache directly, which proves the CACHE refuses what it should and nothing
// at all about whether the production publishers hand it the right token. Each
// of those publishers captures its token at a specific instant, and "at the
// wrong instant" is precisely the bug the fences exist to prevent — so a test
// that never drives the publisher cannot detect it. All the mutations below were
// green against the previous tests.
//
// The window each test needs — an invalidation landing between dispatch and
// reply, a handoff landing between two proxy attempts — is a real RPC round trip
// wide on the wire and a few instructions wide in the client. Racing goroutines
// cannot hit it reliably. The frame interceptor can: it runs synchronously on
// the reply frame, strictly after the request went out and strictly before the
// client sees the answer, which IS the window. Firing the event from inside it
// makes the interleaving exact rather than probable.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestSeam_FlushRefusesAReplyOvertakenByAnInvalidation drives grvBatcher.flush —
// the real batched GRV path — with InvalidateGRVCache landing between dispatch
// and reply processing.
//
// Mutation that must redden this: move `tok := db.grvCache.token()` in flush()
// to after sendGRVRequest returns. The token then carries the POST-invalidation
// generation, the reply installs, and an opted-in transaction reads a version
// obtained before the write the caller invalidated in order to observe.
func TestSeam_FlushRefusesAReplyOvertakenByAnInvalidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	b := db.db.grvBatchers[grvBatcherDefault]
	// Warm: learn the proxies and establish the connection the intercept arms.
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}

	// THE WINDOW: the reply frame has arrived from the proxy and has not yet
	// reached the client. An invalidation here is exactly "dispatched before,
	// landed after".
	var fired atomic.Int64
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		db.InvalidateGRVCache()
		fired.Add(1)
		return body, false
	})
	sd.armAddr(proxies[0].Address)

	db.db.grvCache.invalidate() // start from an empty cache
	v, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV through the armed connection: %v", err)
	}
	if fired.Load() == 0 {
		t.Fatal("the interceptor never ran: the reply did not pass through the window " +
			"this test exists to occupy, so a green result proves nothing")
	}

	if got := db.db.grvCache.cachedVersion(); got != 0 {
		t.Fatalf("the cache holds version %d after a GRV whose reply was overtaken by "+
			"InvalidateGRVCache (the reply carried %d).\n"+
			"flush() must capture its token BEFORE dispatch. Capturing it when the "+
			"reply is processed leaves the whole round trip as a window in which the "+
			"invalidation is defeated by the very reply it was meant to retire",
			got, v)
	}
	if _, _, ok := db.db.grvCache.tryCache(grvPriorityDefault); ok {
		t.Fatal("tryCache serves an entry the invalidation should have retired")
	}
}

// TestSeam_BackgroundRefresherRefusesAReplyOvertakenByAnInvalidation is the same
// window on the OTHER publisher. The refresher has no caller to surface an error
// to, so a token captured at the wrong instant there is invisible except through
// the cache it silently repopulates.
//
// Mutation that must redden this: move the refresher's `tok :=
// db.grvCache.token()` to after its sendGRVRequest returns.
func TestSeam_BackgroundRefresherRefusesAReplyOvertakenByAnInvalidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	b := db.db.grvBatchers[grvBatcherDefault]
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}

	var fired atomic.Int64
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		db.InvalidateGRVCache()
		fired.Add(1)
		return body, false
	})
	sd.armAddr(proxies[0].Address)

	// An opted-in request starts the background refresher (C++ launches its
	// updater inside the same gate). The cache stays empty, so the refresher's
	// budget is exhausted and it re-refreshes at its 1ms clamp.
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, true, false); err != nil {
		t.Fatalf("opted-in GRV: %v", err)
	}
	if !b.refresherStarted.Load() {
		t.Fatal("the background refresher never started: this test would be vacuous")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() < 5 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := fired.Load(); n < 5 {
		t.Fatalf("only %d replies crossed the window in 2s: the refresher is not "+
			"publishing, so this test cannot detect a token captured at the wrong "+
			"instant", n)
	}

	if got := db.db.grvCache.cachedVersion(); got != 0 {
		t.Fatalf("the cache holds version %d after %d background refreshes, every one "+
			"of them overtaken by an invalidation before its reply was processed.\n"+
			"The refresher must capture its token before dispatch like the foreground "+
			"path: it publishes with no caller watching, so a version it resurrects "+
			"after an InvalidateGRVCache is served silently",
			got, fired.Load())
	}
}

// TestSeam_GRVRetryCarriesTheEpochOfTheAttemptThatSucceeded drives
// sendGRVRequest's retry loop across a coordinator handoff: the first attempt's
// reply is dropped, the handoff fires while it is dropped, and the retry runs
// against the proxy set published under the NEW epoch.
//
// The reply belongs to the current cluster, so the cluster-scoped facts it
// carries must count. The one asserted here is the proxy-contact instant, which
// paces the background refresher: refusing it would leave the refresher pacing
// off a contact time from before the handoff and suppress the refresh that
// repopulates the cache under the new epoch.
//
// Mutation that must redden this: consume tok.epoch() instead of attemptEpoch at
// the two applyGRVReply call sites. The token then describes the cluster the
// FIRST attempt was dispatched against, not the one the reply came from.
func TestSeam_GRVRetryCarriesTheEpochOfTheAttemptThatSucceeded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	b := db.db.grvBatchers[grvBatcherDefault]
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}

	epochBefore := db.db.dbInfo.Load().Epoch
	var handoffs atomic.Int64
	// The first armed reply is dropped, and the handoff fires in its place: the
	// client's first attempt times out, and everything after it runs against the
	// proxy set published under the new epoch.
	sd.setIntercept(func(idx int, _ transport.UID, body []byte) ([]byte, bool) {
		if idx == 0 {
			cur := db.db.dbInfo.Load()
			next := *cur // same proxies, new cluster: only the epoch moves
			db.db.onCoordinatorSetAdopted()
			db.db.installProxySet(&next)
			handoffs.Add(1)
			return nil, true // drop, forcing the retry
		}
		return body, false
	})
	sd.armAddr(proxies[0].Address)

	db.db.grvCache.lastProxyContact.Store(0)
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false); err != nil {
		t.Fatalf("GRV across the handoff: %v", err)
	}

	if handoffs.Load() == 0 {
		t.Fatal("the handoff never fired: the retry did not cross an epoch boundary")
	}
	if got := db.db.dbInfo.Load().Epoch; got == epochBefore {
		t.Fatalf("the published epoch did not move (still %d)", got)
	}
	if db.db.grvCache.lastProxyContact.Load() == 0 {
		t.Fatalf("the reply from the retry recorded no proxy contact.\n" +
			"That attempt read its proxies — and its epoch — from the snapshot " +
			"published AFTER the handoff, so its reply is a fact about the CURRENT " +
			"cluster. Consuming the epoch the FIRST attempt was dispatched under " +
			"discards it, and the refresher then paces off a contact instant from " +
			"before the handoff, suppressing the refresh that would repopulate the " +
			"cache under the new epoch")
	}
}

// TestSeam_ProxyContactIsTheRequestInstant pins which of the two instants the
// contact time is, against C++ rather than against whichever one happened to be
// in scope. C++ sets lastProxyRequestTime = trState->startTime
// (NativeAPI.actor.cpp:7408) and reserves the reply instant for the ratekeeper
// cooldowns (:7411-7416).
//
// It matters because the refresher subtracts this from MAX_PROXY_CONTACT_LAG:
// dating it at the reply understates how long the proxy has gone uncontacted by
// a whole round trip, so the contact budget runs over by that much.
func TestSeam_ProxyContactIsTheRequestInstant(t *testing.T) {
	t.Parallel()

	db := &database{}
	b := &grvBatcher{priority: grvPriorityDefault}

	requestTime := time.Now().Add(-time.Second) // a round trip that took a while
	b.applyGRVReply(db, db.grvCache.token(), requestTime, 7000, false, false, nil)

	got := time.Unix(0, db.grvCache.lastProxyContact.Load())
	if !got.Equal(requestTime) {
		t.Fatalf("lastProxyContact = %v, want the REQUEST instant %v (delta %v).\n"+
			"C++ stores trState->startTime (NativeAPI.actor.cpp:7408); the reply "+
			"instant belongs to the ratekeeper cooldowns (:7411-7416). Dating contact "+
			"at the reply understates the uncontacted interval by a whole round trip, "+
			"and the refresher's proxy-contact budget overruns by exactly that much",
			got, requestTime, got.Sub(requestTime))
	}
}

// TestSeam_ProxyContactFromAPreviousEpochIsIgnored is the cluster-scoping arm:
// "when we last reached the proxies" is a fact about a CLUSTER. A straggler from
// the previous one must not refresh it, or the background refresher sits on a
// contact budget it never earned against the cluster it is now serving —
// suppressing the refresh that would repopulate the cache under the new epoch.
func TestSeam_ProxyContactFromAPreviousEpochIsIgnored(t *testing.T) {
	t.Parallel()

	db := &database{}
	b := &grvBatcher{priority: grvPriorityDefault}

	straggler := db.grvCache.token() // dispatched against the previous cluster
	db.grvCache.resetForNewCoordinators()

	b.applyGRVReply(db, straggler, time.Now(), 9_000_000, false, false, nil)

	if got := db.grvCache.lastProxyContact.Load(); got != 0 {
		t.Fatalf("a reply from the PREVIOUS cluster recorded proxy contact at %v.\n"+
			"Contact is cluster-scoped exactly like the ratekeeper cooldowns: this "+
			"reply proves nothing about when the current cluster's proxies were last "+
			"reached, and crediting it suppresses the refresh that repopulates the "+
			"cache under the new epoch", time.Unix(0, got))
	}
}
