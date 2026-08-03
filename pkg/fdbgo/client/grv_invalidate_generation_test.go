package client

// InvalidateGRVCache's guarantee: no version obtained before the call is
// served after it. A caller invalidates because an external write happened, so
// a version that predates the call answers with data the caller knows is
// superseded.
//
// Clearing the entry does not enforce that on its own, and the reason is
// perverse: the version-0 sentinel the clear installs makes every version look
// like an ADVANCE. A reply already inside the publish loop — correctly
// rejected as stale against the old version — loses its CAS to the
// invalidation, reloads the sentinel, no longer looks stale against 0, and
// installs the very version it was just rejected for. The invalidation
// converts a rejected reply into an accepted one.
//
// The generation counter is what enforces the guarantee: a reply may install
// its version only against the generation it was validated under. It is
// captured at REQUEST DISPATCH, not at reply processing, so the whole RPC
// round trip is covered too — otherwise an in-flight GRV that lands just after
// an invalidation repopulates the cache with exactly the version the caller
// invalidated to get rid of.
//
// The cooldown is deliberately exempt: a ratekeeper throttle signal is a fact
// about the cluster, not about the version that carried it, so a
// generation-blocked reply still contributes it.

import (
	"sync"
	"testing"
	"time"
)

// TestMergeReply_BlockedGenerationNeverInstallsAVersion is the DETERMINISTIC
// pin, and it targets the decision the bug lived in rather than racing the
// loop that makes it. The resurrection happened on a RETRY, where the reloaded
// entry is the sentinel — so that is exactly the input reproduced here.
func TestMergeReply_BlockedGenerationNeverInstallsAVersion(t *testing.T) {
	t.Parallel()

	base := time.Now()
	// What the retry sees after an invalidation: the version-0 sentinel,
	// carrying forward the cooldowns the invalidation preserved.
	sentinel := &grvCacheEntry{rkDefault: base.Add(-time.Hour)}

	got := mergeReply(sentinel, false /* generation moved */, true /* same epoch */, false /* not durable */, base, 300,
		base, true /* rkDefault */, false)

	if got.version != 0 {
		t.Fatalf("a reply whose generation was invalidated installed version %d against the "+
			"post-invalidation sentinel.\n"+
			"Against version 0 every version looks like an advance, which is exactly why "+
			"the sentinel alone cannot enforce InvalidateGRVCache: the reply that was "+
			"just rejected as stale becomes acceptable on the retry, and a caller who "+
			"invalidated after an external write is served the version they invalidated "+
			"to escape", got.version)
	}
	if !got.freshness.IsZero() || !got.anchor.IsZero() {
		t.Fatalf("the blocked reply moved the entry's instants (anchor %v, freshness %v): "+
			"they belong to the version it was not allowed to install, so serving them "+
			"would date the sentinel as if it were a real cached version",
			got.anchor, got.freshness)
	}
	// The cooldown is the one thing it may still contribute.
	if !got.rkDefault.Equal(base) {
		t.Fatalf("rkDefault = %v, want %v: a ratekeeper signal describes the CLUSTER, not "+
			"the version that carried it, so a generation-blocked reply must still "+
			"deliver it", got.rkDefault, base)
	}
}

// TestMergeReply_CurrentGenerationStillInstalls keeps the pin above honest: it
// would pass against a merge that never installs anything.
func TestMergeReply_CurrentGenerationStillInstalls(t *testing.T) {
	t.Parallel()

	base := time.Now()
	got := mergeReply(&grvCacheEntry{}, true /* generation current */, true /* same epoch */, false, base, 300,
		time.Time{}, false, false)

	if got.version != 300 {
		t.Fatalf("version = %d, want 300: an un-invalidated reply must still populate the "+
			"cache, or the generation check has simply disabled caching", got.version)
	}
	if !got.anchor.Equal(base) || !got.freshness.Equal(base) {
		t.Fatalf("anchor %v / freshness %v, want %v", got.anchor, got.freshness, base)
	}
}

// TestInvalidate_StaleReplyLosingItsCASCannotResurrectItsVersion drives the
// real publish loop through the exact interleaving: a throttled stale reply is
// in flight while the cache is invalidated. It is the behavioural companion to
// the decision test — it can only ever CATCH the resurrection, never invent
// it, so it runs many interleavings and asserts an exact property.
func TestInvalidate_StaleReplyLosingItsCASCannotResurrectItsVersion(t *testing.T) {
	t.Parallel()

	const (
		cachedVersion = 5000
		staleVersion  = 300 // strictly older: must never be served
		attempts      = 400
	)

	for i := 0; i < attempts; i++ {
		base := time.Now()
		c := &grvCache{now: func() time.Time { return base }}
		c.publish(c.token(), base, cachedVersion)

		// The reply is validated under the CURRENT generation, as a real one
		// dispatched before the invalidation would be.
		gen := c.token()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// A throttled stale reply: throttled, because an un-throttled
			// stale reply has nothing to merge and never reaches the CAS.
			c.publishReplyAt(gen, false, base, staleVersion, base, true, false)
		}()
		go func() {
			defer wg.Done()
			c.invalidate()
		}()
		wg.Wait()

		// After both, the cache must not hold the stale version — nor may a
		// reader be served it.
		if v := c.cachedVersion(); v == staleVersion {
			t.Fatalf("attempt %d/%d: the cache holds version %d after an invalidation that "+
				"ran concurrently with that reply.\n"+
				"The reply was stale against the cached version %d and was rejected; "+
				"losing its CAS to the invalidation must not turn that rejection into an "+
				"install. InvalidateGRVCache promises no pre-invalidation version is "+
				"served afterwards",
				i+1, attempts, v, cachedVersion)
		}
		if v, _, ok := c.tryCache(grvPriorityDefault); ok && v == staleVersion {
			t.Fatalf("attempt %d/%d: tryCache served the pre-invalidation version %d",
				i+1, attempts, v)
		}
	}
}

// TestInvalidate_InFlightReplyCannotRepopulate covers the WIDER window of the
// same defect: a reply whose request was dispatched before the invalidation
// but which is processed after it. That window is a whole RPC round trip, not
// a few instructions, so it is the one a real workload actually hits.
func TestInvalidate_InFlightReplyCannotRepopulate(t *testing.T) {
	t.Parallel()

	base := time.Now()
	db := &database{}
	db.grvCache.now = func() time.Time { return base }
	b := &grvBatcher{priority: grvPriorityDefault}

	db.grvCache.publish(db.grvCache.token(), base, 5000)

	// A GRV is dispatched: the generation is captured here, as flush does.
	gen := db.grvCache.token()

	// While it is in flight, the caller invalidates after an external write.
	db.grvCache.invalidate()

	// The reply lands.
	b.applyGRVReply(db, gen, base, 7000, false, false, nil)

	if v, _, ok := db.grvCache.tryCache(grvPriorityDefault); ok {
		t.Fatalf("a GRV reply dispatched BEFORE InvalidateGRVCache repopulated the cache "+
			"with version %d, and it is being served.\n"+
			"The caller invalidated because an external write happened; this version was "+
			"obtained before that call and cannot reflect it. Capturing the generation "+
			"at reply-processing time instead of at dispatch leaves the entire RPC round "+
			"trip as a window in which the invalidation is defeated by a reply it was "+
			"meant to retire", v)
	}
}

// TestInvalidate_ReplyDispatchedAfterInvalidationPopulates is the control: the
// generation check must block only replies that predate the invalidation.
func TestInvalidate_ReplyDispatchedAfterInvalidationPopulates(t *testing.T) {
	t.Parallel()

	base := time.Now()
	db := &database{}
	db.grvCache.now = func() time.Time { return base }
	b := &grvBatcher{priority: grvPriorityDefault}

	db.grvCache.publish(db.grvCache.token(), base, 5000)
	db.grvCache.invalidate()

	// Dispatched AFTER the invalidation: this one is entitled to populate.
	gen := db.grvCache.token()
	b.applyGRVReply(db, gen, base, 7000, false, false, nil)

	v, _, ok := db.grvCache.tryCache(grvPriorityDefault)
	if !ok || v != 7000 {
		t.Fatalf("tryCache = (%d, %v), want (7000, true): a reply dispatched after the "+
			"invalidation must repopulate the cache, or InvalidateGRVCache has "+
			"permanently disabled it rather than resetting it", v, ok)
	}
}
