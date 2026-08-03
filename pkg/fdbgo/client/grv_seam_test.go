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

// TestSeam_MinAcceptableFromAPreviousEpochIsIgnored is the third arm of the same
// cluster-scoping rule, and the one the gate did not cover.
//
// minAcceptableReadVersion is a claim about a VERSION SPACE. A straggler from the
// previous cluster writes that cluster's numbers into this one's floor, and
// validateVersion then rejects every user-set version below the previous
// cluster's space with transaction_too_old.
//
// What makes it worse than the cooldowns or the contact instant is that the
// pollution DISARMS ITS OWN RECOVERY. The handoff resets the floor to 0 exactly
// so ensureReadVersion's bootstrap GRV fires and re-derives it from this
// cluster; that path is gated on the floor reading 0, so a straggler landing
// afterwards closes it again and nothing reopens it. Same shape as the durable
// arm the epoch gate originally missed.
func TestSeam_MinAcceptableFromAPreviousEpochIsIgnored(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	b := &grvBatcher{priority: grvPriorityDefault}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	// A GRV dispatched against cluster A, whose version space is high.
	straggler := db.grvCache.token()

	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})
	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("precondition: the handoff must reset the floor to 0, got %d", got)
	}

	// A's reply lands after the handoff.
	b.applyGRVReply(db, straggler, time.Now(), 9_000_000, false, false, nil)

	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("a reply from the PREVIOUS cluster set the minimum-acceptable floor "+
			"to %d.\n"+
			"That is cluster A's version space written into cluster B's floor. Every "+
			"user-set read version on B below it now fails with transaction_too_old — "+
			"and the pollution disarms its own recovery, because the bootstrap GRV that "+
			"would re-derive the floor fires only while it reads 0", got)
	}
	if err := db.validateVersion(500); err != nil {
		t.Fatalf("validateVersion(500) = %v on the new cluster after a straggler from "+
			"the previous one. A version this cluster mints must not be rejected on the "+
			"strength of a version space it has nothing to do with", err)
	}
}

// TestSeam_ResetClearsTheProxyContactInstant closes the gap between what
// resetForNewCoordinators claims and what it did. Every other cluster-scoped
// fact rides the entry and is made inert by the epoch stamp; the contact instant
// is a bare atomic, so nothing about the new epoch retires it. Carried across, it
// grants a proxy-contact budget that was never earned against the cluster now
// being served, delaying the refresh that repopulates the cache under the new
// epoch.
func TestSeam_ResetClearsTheProxyContactInstant(t *testing.T) {
	t.Parallel()

	c := &grvCache{}
	c.lastProxyContact.Store(time.Now().UnixNano())

	c.resetForNewCoordinators()

	if got := c.lastProxyContact.Load(); got != 0 {
		t.Fatalf("the proxy-contact instant survived a coordinator handoff (%v).\n"+
			"It records when the PREVIOUS cluster's proxies were last reached. The "+
			"refresher subtracts it from MAX_PROXY_CONTACT_LAG, so carrying it grants a "+
			"contact budget never earned against this cluster and delays the very "+
			"refresh that would repopulate the cache under the new epoch",
			time.Unix(0, got))
	}
}

// TestSeam_StaleEpochStragglerAtAnEmptyCacheIsNotAPublication keeps the
// publication counter EXACT. The counter is not a metric here — it is the proof
// that everything tryCache consults arrives in one publication, a property
// otherwise observable only by catching a window nanoseconds wide. A count that
// is approximately right cannot carry that proof.
//
// A stale-epoch straggler against an empty cache merges nothing, so the entry it
// would install is the zero value: no observable changes, and every accessor
// still reads empty — but the counter moved.
func TestSeam_StaleEpochStragglerAtAnEmptyCacheIsNotAPublication(t *testing.T) {
	t.Parallel()

	c := &grvCache{}
	straggler := c.token()
	c.resetForNewCoordinators()

	before := c.publications.Load()
	c.publish(straggler, time.Now(), 9_000_000)

	if got := c.publications.Load(); got != before {
		t.Fatalf("publications advanced %d -> %d for a stale-epoch reply that changed "+
			"nothing.\n"+
			"The counter is the exact-accounting proof that everything tryCache "+
			"consults arrives in ONE publication; a straggler installing a zero entry "+
			"where there was none makes that accounting approximate, and an approximate "+
			"count cannot carry the proof", before, got)
	}
	if c.cachedVersion() != 0 || c.entryFloor() != 0 {
		t.Fatalf("the straggler left state behind: version %d, floor %d",
			c.cachedVersion(), c.entryFloor())
	}
}

// TestSeam_FloorRefusesAStragglerWritingAfterTheHandoff is the interleaving the
// outer sameEpoch gate cannot cover, because that gate's verdict is reached in
// publishReplyAt and acted on a few instructions later in applyGRVReply.
//
// A reply from the previous cluster is judged current, the handoff lands in the
// gap, and the store then writes the previous cluster's version space into this
// one's floor. That is not a delay: the write restores a non-zero floor after the
// reset deliberately cleared it, and ensureReadVersion's bootstrap GRV — the
// repair that would re-derive the floor from the cluster now being served —
// fires only while the floor reads unset. A handle that only ever uses
// SetReadVersion takes no other GRV, so for it the suppression is permanent.
//
// The close is that the STORE carries its own check: updateMinAcceptable
// re-reads the epoch at its own CAS and stamps what it writes, so the decision
// is made where the write happens rather than inherited from an earlier instant.
func TestSeam_FloorRefusesAStragglerWritingAfterTheHandoff(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	// A reply dispatched against cluster A, whose version space is high.
	straggler := db.grvCache.token()

	// The reply is judged SAME-EPOCH here, exactly as publishReplyAt would judge
	// it a few instructions before the store.
	verdict := straggler.epoch() == db.grvCache.epochNow()
	if !verdict {
		t.Fatal("precondition: the straggler must be judged current, or this test " +
			"reproduces the wrong interleaving")
	}

	// THE WINDOW: the handoff lands between that verdict and the store below.
	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})
	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("precondition: the handoff must leave the floor unset, got %d", got)
	}

	// The store proceeds on the stale verdict, as applyGRVReply's would.
	db.updateMinAcceptable(straggler.epoch(), 9_000_000)

	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("a reply from the PREVIOUS cluster wrote %d into this cluster's floor "+
			"through the gap between the epoch verdict and the store.\n"+
			"The store must carry its own epoch check and stamp what it writes. "+
			"Inheriting a verdict reached a few instructions earlier lets the previous "+
			"cluster's version space land after the reset that cleared it", got)
	}
	if err := db.validateVersion(500); err != nil {
		t.Fatalf("validateVersion(500) = %v: a version this cluster mints is rejected "+
			"on the strength of the previous cluster's space", err)
	}

	// And the repair the suppression targeted is armed again: a floor reading
	// unset is exactly what makes ensureReadVersion take its bootstrap GRV.
	if db.minAcceptable() != 0 {
		t.Fatal("the floor must read unset so the bootstrap GRV re-derives it from " +
			"the cluster now being served — the repair a polluted floor suppresses")
	}
}

// TestSeam_FloorStampIsInertAfterAHandoff is the other half of the stamp: a
// value written legitimately, BEFORE any handoff, must stop applying the moment
// the fence moves. Nothing has to clear it — the reader validates the epoch in
// the same load that produced the version, so a floor from another version space
// simply reads as unset.
func TestSeam_FloorStampIsInertAfterAHandoff(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	db.updateMinAcceptable(db.grvCache.epochNow(), 9_000_000)
	if got := db.minAcceptable(); got != 9_000_000 {
		t.Fatalf("precondition: floor = %d, want 9000000", got)
	}

	// The fence alone — no installProxySet, so nothing republishes the stamp.
	// The stamp must go inert on its own.
	db.grvCache.resetForNewCoordinators()

	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("a floor stamped in the previous epoch still reads as %d.\n"+
			"The stamp exists so a reader validates the epoch in the same load that "+
			"produced the version. Relying on someone republishing an empty stamp "+
			"instead leaves a window — between the fence moving and that republish — "+
			"in which another cluster's version space is still enforced", got)
	}
	if err := db.validateVersion(500); err != nil {
		t.Fatalf("validateVersion(500) = %v after the fence moved", err)
	}
}

// TestSeam_StragglerCannotDestroyTheCurrentFloor pins what the epoch check ON
// THE STORE defends, which is NOT what the reader's stamp check defends. The two
// are independent and neither substitutes for the other:
//
//   - the READER's check stops a foreign-epoch floor being ENFORCED. A straggler's
//     value is stamped with its own epoch, so it never rejects anything.
//   - the STORE's check stops a foreign-epoch reply DESTROYING the floor this
//     cluster legitimately established. Without it the straggler's CAS succeeds,
//     the current epoch's stamp is overwritten with an inert one, and a floor that
//     was doing real work is silently gone.
//
// Losing it fails open — validateVersion stops rejecting genuinely ancient
// pinned versions, and the handle pays an unnecessary bootstrap GRV to rebuild
// what it already knew.
func TestSeam_StragglerCannotDestroyTheCurrentFloor(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	straggler := db.grvCache.token() // dispatched against cluster A

	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})

	// Cluster B establishes a real floor, and it does real work.
	db.updateMinAcceptable(db.grvCache.epochNow(), 500)
	if got := db.minAcceptable(); got != 500 {
		t.Fatalf("precondition: floor = %d, want 500", got)
	}
	if err := db.validateVersion(100); err == nil {
		t.Fatal("precondition: 100 must be rejected against B's floor of 500")
	}

	// A's reply lands late and attempts the write.
	db.updateMinAcceptable(straggler.epoch(), 9_000_000)

	if got := db.minAcceptable(); got != 500 {
		t.Fatalf("floor = %d after a PREVIOUS cluster's reply attempted a write, want "+
			"500 preserved.\n"+
			"The straggler's value is inert to readers because it carries its own "+
			"epoch — but the write still landed and overwrote the floor this cluster "+
			"established. The epoch check belongs ON THE STORE as well: a reply from "+
			"another version space must not be able to destroy this one's floor, or "+
			"validateVersion silently stops rejecting the ancient versions it exists "+
			"to reject", got)
	}
	if err := db.validateVersion(100); err == nil {
		t.Fatal("100 is accepted again: the floor that was rejecting it was destroyed " +
			"by a reply from a cluster whose version space is unrelated")
	}
}
