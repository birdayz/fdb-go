package client

// The COMMIT-warming path publishes into the GRV cache too, and it is the
// path most exposed to InvalidateGRVCache: a commit's round trip is long, and
// the caller who invalidates is very often the same application that just
// committed elsewhere.
//
// A commit dispatched before InvalidateGRVCache but landing after it must not
// install its version. If it does, a USE_GRV_CACHE transaction reads at a
// version that predates the invalidation and misses the very write the caller
// invalidated in order to observe — the cache actively defeats the API meant
// to bypass it.
//
// The generation is therefore captured beside commitStart, BEFORE the commit
// is dispatched, exactly as the GRV batcher and the background refresher
// capture theirs. Every publication entry point on grvCache REQUIRES a
// generation parameter for this reason: there is no overload that loads one
// internally, because "load it at publication time" is the bug itself, and a
// future publisher should be unable to compile without deciding when it
// captured.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestMergeReply_CommitAgainstSentinelWithOldGeneration is the deterministic
// pin for the commit path's specific shape: a commit reply CASing against the
// version-0 sentinel an invalidation left behind. Against 0 every version
// looks like an advance, so the generation is the only thing that can reject
// it — and unlike the GRV path a commit carries no cooldown, so this is the
// case where the reply has nothing legitimate left to contribute.
func TestMergeReply_CommitAgainstSentinelWithOldGeneration(t *testing.T) {
	t.Parallel()

	base := time.Now()
	sentinel := &grvCacheEntry{} // what invalidate() leaves behind

	got := mergeReply(sentinel, false /* generation moved */, true /* same epoch */, true /* durable */, base, 7000,
		time.Time{}, false, false)

	if got.version != 0 {
		t.Fatalf("a commit whose generation was invalidated installed version %d against "+
			"the post-invalidation sentinel.\n"+
			"The commit was dispatched before InvalidateGRVCache and cannot reflect the "+
			"external write the caller invalidated to observe; installing it lets an "+
			"opted-in transaction read a version that predates the very write it was "+
			"invalidated for", got.version)
	}
	if !got.freshness.IsZero() {
		t.Fatalf("the blocked commit renewed freshness to %v: freshness marks the entry as "+
			"confirmed with the cluster, so renewing it on a version that was refused "+
			"would keep the sentinel alive as though it were a real cached version",
			got.freshness)
	}
}

// TestCommitWarming_DispatchedBeforeInvalidateCannotRepopulate drives the real
// publication path end to end in the order a live client produces:
// commit dispatched → InvalidateGRVCache → commit lands.
func TestCommitWarming_DispatchedBeforeInvalidateCannotRepopulate(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}
	c.publish(c.token(), base, 5000)

	// The commit is dispatched: the generation is captured here, beside
	// commitStart, before the RPC goes out.
	commitGen := c.token()

	// While it is in flight, the application invalidates after an external
	// write it needs subsequent reads to observe.
	c.invalidate()

	// The commit lands and warms the cache.
	c.update(commitGen, base, 7000)

	if v, _, ok := c.tryCache(grvPriorityDefault); ok {
		t.Fatalf("the cache is serving version %d, warmed by a commit DISPATCHED BEFORE "+
			"InvalidateGRVCache.\n"+
			"An opted-in transaction now reads at a version that predates the "+
			"invalidation and misses the write the caller invalidated to observe. The "+
			"commit path must capture its generation beside commitStart — before "+
			"dispatch — like every other publisher", v)
	}
}

// TestCommitWarming_DispatchedAfterInvalidatePopulates is the control: a
// commit dispatched after the invalidation is entitled to warm the cache, so
// the test above cannot be passing because commit warming stopped working.
func TestCommitWarming_DispatchedAfterInvalidatePopulates(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}
	c.publish(c.token(), base, 5000)
	c.invalidate()

	commitGen := c.token() // dispatched after the invalidation
	c.update(commitGen, base, 7000)

	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 7000 {
		t.Fatalf("tryCache = (%d, %v), want (7000, true): a commit dispatched after the "+
			"invalidation must still warm the cache, or the generation check has "+
			"disabled commit warming rather than scoping it", v, ok)
	}
}

// TestGRVCachePublishersAllRequireAGeneration is the gate. It is a compile-time
// property expressed as a test: every publication entry point on grvCache takes
// a generation as its first parameter, so a new publisher cannot be added
// without deciding when it captured one.
//
// The bug this closes was not a missing check — the check existed and the GRV
// path used it correctly. It was a publication path that reached a convenience
// wrapper which loaded the generation itself, at exactly the moment loading it
// is wrong. Deleting that wrapper is what makes the mistake unrepresentable;
// this test states the invariant so that re-adding one is a deliberate act
// rather than an innocent-looking convenience.
func TestGRVCachePublishersAllRequireAGeneration(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}
	gen := c.token()

	// Each of these is a publication entry point, and each takes the
	// generation explicitly. If one of them ever regains an internally-loading
	// overload, this file stops compiling against it — which is the point.
	c.publish(gen, base, 100)
	c.update(gen, base, 200)
	c.updateFromGRV(gen, base, 300)
	c.markThrottled(gen, base, true, false)
	c.publishReplyAt(gen, false, base, 400, base, false, true)

	if v := c.cachedVersion(); v != 400 {
		t.Fatalf("cachedVersion = %d, want 400 after the publication sequence: the entry "+
			"points do not all reach the same publication path", v)
	}
}

// TestCommitWarming_BlockedCommitLeavesNoStaleEntryServable is the dual of
// the generation gate, and the case the gate on its own gets wrong.
//
// A commit dispatched under generation N is overtaken by an invalidation; a
// generation-N+1 GRV then caches an OLDER version; the commit lands. Refusing
// the install is right — the commit's anchor belongs to a pre-invalidation
// dispatch. Leaving the older version servable is not: the caller's Commit has
// RETURNED, so that write is durable, and the next USE_GRV_CACHE transaction
// would read at a version that provably predates it. Read-your-committed-writes
// is violated by the very mechanism that protects InvalidateGRVCache.
//
// The floor needs no generation, and that asymmetry is the whole design: an
// install is a claim about WHEN, which a stale dispatch cannot make; the floor
// is a claim about ORDER, which a version comparison settles on its own.
//
// The commit does not EVICT the stale entry — an eviction is point-in-time and
// a publisher already in flight would simply re-install. It raises the floor,
// which makes the entry unservable wherever it came from and whenever it
// arrives.
func TestCommitWarming_BlockedCommitLeavesNoStaleEntryServable(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	// The commit is dispatched under the current generation.
	commitGen := c.token()
	// An invalidation overtakes it.
	c.invalidate()
	// A GRV dispatched AFTER the invalidation caches an older version.
	c.publish(c.token(), base, 6000)
	if v, _, ok := c.tryCache(grvPriorityDefault); !ok || v != 6000 {
		t.Fatalf("precondition: expected 6000 cached and servable, got (%d, %v)", v, ok)
	}

	// The commit lands with a HIGHER version.
	c.update(commitGen, base, 7000)

	if v, _, ok := c.tryCache(grvPriorityDefault); ok {
		t.Fatalf("the cache is still serving version %d after a commit at 7000 RETURNED.\n"+
			"7000 is durable, so 6000 provably predates it and every read served from "+
			"it misses the caller.s own just-committed write. The blocked commit may "+
			"not INSTALL its version — its anchor belongs to a pre-invalidation "+
			"dispatch — but it must still raise the FLOOR, which needs no generation "+
			"because it is a version comparison", v)
	}
}

// TestCommitWarming_BlockedCommitLeavesANewerEntryAlone is the control that
// keeps the retire from degenerating into "always clear". A cached version at
// or above the commit's already reflects it, so it must survive untouched —
// otherwise the fix would simply disable the cache after any invalidation.
func TestCommitWarming_BlockedCommitLeavesANewerEntryAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		cached int64
	}{
		{"cached newer than the commit", 8000},
		{"cached equal to the commit", 7000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := time.Now()
			c := &grvCache{now: func() time.Time { return base }}

			commitGen := c.token()
			c.invalidate()
			c.publish(c.token(), base, tc.cached)

			c.update(commitGen, base, 7000)

			v, _, ok := c.tryCache(grvPriorityDefault)
			if !ok || v != tc.cached {
				t.Fatalf("tryCache = (%d, %v), want (%d, true): a cached version at or above "+
					"the commit's already reflects it and must be left alone. Retiring it "+
					"too would turn the read-your-writes repair into a cache that empties "+
					"itself after every invalidation", v, ok, tc.cached)
			}
		})
	}
}

// TestCommit_RealPathWarmsAndSurvivesConcurrentInvalidation exercises the
// CHANGED CALL SITE — Transaction.Commit's own warming — rather than the cache
// helpers it delegates to. The unit tests above pin the decision; this pins
// that the real commit path reaches it with a dispatch-captured generation.
//
// Two properties, both through a live cluster:
//
//   - a plain commit warms the cache with its committed version, and the
//     anchor it publishes is the PRE-DISPATCH instant (bracketed against the
//     window the commit actually occupied, so a now()-stamped anchor fails);
//   - racing InvalidateGRVCache against a commit never leaves the cache
//     serving a version BELOW the one that commit returned — the
//     read-your-committed-writes invariant, checked across many interleavings.
func TestCommit_RealPathWarmsAndSurvivesConcurrentInvalidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("commit-gen-" + t.Name())

	t.Run("warms with a pre-dispatch anchor", func(t *testing.T) {
		db.db.grvCache.invalidate()

		tx := db.CreateTransaction()
		defer tx.Cancel()
		tx.Set(key, []byte("v1"))

		before := time.Now()
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		after := time.Now()

		cv, err := tx.GetCommittedVersion()
		if err != nil {
			t.Fatalf("committed version: %v", err)
		}
		if got := db.db.grvCache.cachedVersion(); got != cv {
			t.Fatalf("cache holds version %d after a commit at %d: Commit's warming did "+
				"not reach the cache through the real path", got, cv)
		}
		anchor := db.db.grvCache.anchorInstant()
		if anchor.Before(before) || anchor.After(after) {
			t.Fatalf("commit anchor %v outside the commit's own window [%v, %v]",
				anchor, before, after)
		}
	})

	t.Run("a concurrent invalidation never leaves an older version servable", func(t *testing.T) {
		const attempts = 40
		for i := 0; i < attempts; i++ {
			tx := db.CreateTransaction()
			tx.Set(key, []byte("v2"))

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				db.InvalidateGRVCache()
			}()
			commitErr := tx.Commit(ctx)
			wg.Wait()
			if commitErr != nil {
				tx.Cancel()
				t.Fatalf("attempt %d: commit: %v", i+1, commitErr)
			}
			cv, err := tx.GetCommittedVersion()
			tx.Cancel()
			if err != nil {
				t.Fatalf("attempt %d: committed version: %v", i+1, err)
			}

			if v, _, ok := db.db.grvCache.tryCache(grvPriorityDefault); ok && v < cv {
				t.Fatalf("attempt %d/%d: the cache is serving version %d after a commit at "+
					"%d returned.\n"+
					"Whatever the interleaving with InvalidateGRVCache, a version below "+
					"the one this commit durably wrote must never remain servable — a "+
					"USE_GRV_CACHE transaction reading it misses the caller's own write",
					i+1, attempts, v, cv)
			}
		}
	})
}

// TestCommitFloor_InFlightLowGRVCannotRepopulateAfterACommit is the route
// around the retire: clearing the entry does not stop a publisher that is
// ALREADY IN FLIGHT and generation-current from installing a version below the
// one a commit just made durable.
//
// The commit returned at 7000. A GRV dispatched after the invalidation — so
// nothing about the generation refuses it — lands with 6000 and repopulates.
// The next USE_GRV_CACHE transaction reads 6000 and misses the caller's own
// committed write. Same stale read as before, reached by a different door,
// which is why the answer has to be an ORDER invariant rather than another
// eviction: evictions are point-in-time and publishers arrive arbitrarily
// late.
func TestCommitFloor_InFlightLowGRVCannotRepopulateAfterACommit(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	commitGen := c.token() // commit dispatched
	c.invalidate()         // overtaken by an invalidation
	grvGen := c.token()    // a GRV dispatched after it, now in flight

	c.update(commitGen, base, 7000) // the commit lands: durable at 7000

	// The in-flight GRV lands LAST, carrying a lower version.
	c.publish(grvGen, base, 6000)

	if v, _, ok := c.tryCache(grvPriorityDefault); ok && v < 7000 {
		t.Fatalf("the cache is serving version %d after a commit at 7000 returned.\n"+
			"That GRV was dispatched after the invalidation, so the generation cannot "+
			"refuse it, and the retire had already happened before it landed. Only a "+
			"monotonic committed-version FLOOR closes this: a version below the last "+
			"durable commit is stale by construction, whenever it arrives", v)
	}
}

// TestCommitFloor_FreshGRVAtOrAboveTheFloorStillInstalls is the control that
// keeps the floor from being a cache-disabling device. A GRV at or above the
// last committed version is exactly what the cache is for.
func TestCommitFloor_FreshGRVAtOrAboveTheFloorStillInstalls(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		grv  int64
	}{
		{"above the floor", 7001},
		{"exactly at the floor", 7000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := time.Now()
			c := &grvCache{now: func() time.Time { return base }}
			gen := c.token()

			c.update(gen, base, 7000) // floor := 7000
			c.publish(gen, base, tc.grv)

			v, _, ok := c.tryCache(grvPriorityDefault)
			if !ok || v != tc.grv {
				t.Fatalf("tryCache = (%d, %v), want (%d, true): the floor must refuse only "+
					"versions BELOW the last commit. Refusing at-or-above turns it into a "+
					"switch that disables caching after the first commit", v, ok, tc.grv)
			}
		})
	}
}

// TestCommitFloor_SurvivesInvalidation pins the rule that the floor and the
// invalidation answer different questions: invalidation is about freshness
// (what is recent enough), the floor is about order (what is old enough to be
// wrong). Resetting the floor on invalidate would let the next low publication
// become servable and re-open the stale read the commit had already ruled out.
func TestCommitFloor_SurvivesInvalidation(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	c.update(c.token(), base, 7000) // floor := 7000
	c.invalidate()

	// A GRV dispatched after the invalidation — generation-current, so only
	// the floor can refuse it.
	c.publish(c.token(), base, 6000)

	if v, _, ok := c.tryCache(grvPriorityDefault); ok {
		t.Fatalf("the cache is serving version %d after an invalidation, below the "+
			"committed floor of 7000.\n"+
			"invalidate() must PRESERVE the floor: it clears what is stale by TIME, "+
			"while the floor rules out what is stale by ORDER, and a committed version "+
			"stays committed across an invalidation", v)
	}
}

// TestCommitFloor_DoesNotWedgeTheCacheForever states the consequence that can
// look like a regression in a hit-rate test: a client whose GRVs lag its last
// commit pays real GRVs until the cluster's versions catch up, and then caches
// normally again. That is correct, and it is bounded.
func TestCommitFloor_DoesNotWedgeTheCacheForever(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}
	gen := c.token()

	c.update(gen, base, 7000)

	// A lagging GRV is refused, and the entry it failed to overwrite is the
	// COMMIT'S OWN version — which sits exactly at the floor and is perfectly
	// servable. The assertion is therefore about WHICH version is served, not
	// about a miss: refusing the low version must not also discard the good
	// one that was already there.
	c.publish(gen, base, 6500)
	if v, _, ok := c.tryCache(grvPriorityDefault); !ok || v != 7000 {
		t.Fatalf("tryCache = (%d, %v), want (7000, true): the lagging GRV must be refused "+
			"WITHOUT disturbing the commit's own version, which is at the floor and "+
			"legitimately servable", v, ok)
	}

	// The cluster catches up.
	c.publish(gen, base, 7100)
	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 7100 {
		t.Fatalf("tryCache = (%d, %v), want (7100, true): once the cluster's versions pass "+
			"the floor the cache must serve again. A floor that permanently wedged the "+
			"cache would be a worse bug than the stale read it prevents", v, ok)
	}
}

// TestCommitFloor_RestsOnAMonotonicVersionSpace pins the ASSUMPTION the floor
// depends on, against a live cluster, because the floor is unbounded
// client-side state and its safety is entirely inherited from FDB's version
// semantics rather than from anything this client enforces.
//
// The assumption: within one cluster, a GRV issued after a commit returns a
// version at or above that commit's. Every read version comes from the same
// monotonic stream, so a client that has observed a commit at V is never
// handed a read version below V afterwards. That is what makes the floor
// self-clearing in normal operation — it blocks nothing, because nothing
// legitimate ever arrives below it.
//
// It is worth a test rather than a sentence because the floor's failure mode
// when the assumption breaks is silent and total: every publication refused,
// the cache never servable, and the background refresher pinned at its 1ms
// clamp. The assumption holds for a handle bound to one continuous version
// space; a version space that REWINDS beneath a client (a restore from backup,
// a force recovery) breaks it, and the remedy there is to reopen the handle —
// the same remedy those operations require of clients generally. Nothing in
// this client detects such a rewind, and this test is where that limitation is
// written down.
func TestCommitFloor_RestsOnAMonotonicVersionSpace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("floor-monotonic-" + t.Name())

	for i := 0; i < 5; i++ {
		tx := db.CreateTransaction()
		tx.Set(key, []byte{byte(i)})
		if err := tx.Commit(ctx); err != nil {
			tx.Cancel()
			t.Fatalf("commit %d: %v", i, err)
		}
		cv, err := tx.GetCommittedVersion()
		tx.Cancel()
		if err != nil {
			t.Fatalf("committed version %d: %v", i, err)
		}

		// A GRV taken after that commit must be at or above it — the property
		// the floor rests on.
		rtx := db.CreateTransaction()
		rv, err := rtx.GetReadVersion(ctx)
		rtx.Cancel()
		if err != nil {
			t.Fatalf("read version %d: %v", i, err)
		}
		if rv < cv {
			t.Fatalf("a GRV taken after a commit at %d returned %d, BELOW it.\n"+
				"The committed-version floor assumes this cannot happen within one "+
				"cluster: if read versions can fall below an observed commit, the floor "+
				"refuses every publication and the GRV cache becomes permanently "+
				"unservable while the background refresher pins at its %v clamp",
				cv, rv, grvRefreshMin)
		}

		// And the cache the commit warmed is servable, not wedged by its own floor.
		if v, _, ok := db.db.grvCache.tryCache(grvPriorityDefault); ok && v < db.db.grvCache.entryFloor() {
			t.Fatalf("tryCache served %d below the floor %d", v, db.db.grvCache.entryFloor())
		}
	}
}

// TestClusterSwitch_ResetsVersionScopedCacheState covers the one way a Go
// client can end up bound to a DIFFERENT version space: it adopts a coordinator
// set it did not previously have, either by following a coordinator forward or
// by re-reading a cluster file another process rewrote. Neither path validates
// cluster identity — the same is true of the C++ client it is ported from — so
// a rewritten cluster file can point a live handle at an unrelated cluster.
//
// Every version-scoped fact in the cache belongs to the cluster that produced
// it: the cached version, its anchor, its freshness, the ratekeeper cooldowns,
// and above all the committed-version FLOOR. Carrying a floor from cluster A
// into cluster B is the worst of them, because B's versions may be entirely
// below A's and the floor then refuses every reply forever — the cache never
// serves again and the background refresher sits at its 1ms clamp, with no
// operator escape, since InvalidateGRVCache preserves the floor by design.
//
// C++ resets analogous state on its explicit cluster-switch path
// (switchConnectionRecord, NativeAPI.actor.cpp:2191-2213: "Reset state from
// former cluster" — proxies, location cache, minAcceptableReadVersion,
// ssVersionVectorCache). Go has no such API, so the reset belongs at the only
// boundary Go has: coordinator-set adoption.
func TestClusterSwitch_ResetsVersionScopedCacheState(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	// Cluster A: a commit raises the floor high.
	c.update(c.token(), base, 9_000_000)
	if c.entryFloor() != 9_000_000 {
		t.Fatalf("precondition: floor = %d, want 9000000", c.entryFloor())
	}

	// The handle adopts a different coordinator set.
	c.resetForNewCoordinators()

	if f := c.entryFloor(); f != 0 {
		t.Fatalf("the committed-version floor survived a coordinator-set switch (floor %d).\n"+
			"That floor belongs to the previous cluster's version space. Against a "+
			"cluster whose versions are lower, it refuses every reply permanently: the "+
			"cache never serves, the refresher pins at its %v clamp, and "+
			"InvalidateGRVCache cannot clear it because it preserves the floor by "+
			"design", f, grvRefreshMin)
	}

	// Cluster B's versions are far lower, and must cache normally.
	bGen := c.token()
	c.publish(bGen, base, 500)
	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 500 {
		t.Fatalf("tryCache = (%d, %v), want (500, true): after switching clusters the new "+
			"cluster's versions must cache normally, however low they are relative to "+
			"the cluster the handle used to serve", v, ok)
	}
}

// TestClusterSwitch_LateReplyFromTheOldClusterIsRefused is the dispatch-capture
// pattern one level up: a GRV or commit dispatched to cluster A that lands
// after the handle has moved to cluster B must not install A's version into B's
// cache. It carries A's generation, and the switch bumped it.
func TestClusterSwitch_LateReplyFromTheOldClusterIsRefused(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	aGen := c.token()           // dispatched against cluster A
	c.resetForNewCoordinators() // the handle moves to cluster B
	c.publish(c.token(), base, 500)

	// A's reply lands late, carrying a version from an unrelated version space.
	c.publish(aGen, base, 9_000_000)

	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 500 {
		t.Fatalf("tryCache = (%d, %v), want (500, true): a reply dispatched to the PREVIOUS "+
			"cluster landed after the switch and installed its version. Read versions "+
			"from an unrelated cluster are meaningless here — worse than stale — and "+
			"the generation captured at dispatch is what has to refuse them", v, ok)
	}
}

// TestClusterSwitch_SameClusterInvalidationStillPreservesTheFloor is the
// control that keeps the switch reset from collapsing into the invalidation.
// They answer different questions: an invalidation says "this cluster moved on"
// (freshness — the floor survives, since a committed version stays committed);
// a coordinator-set switch says "this may not be the same cluster at all"
// (identity — nothing version-scoped survives).
func TestClusterSwitch_SameClusterInvalidationStillPreservesTheFloor(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	c.update(c.token(), base, 7000)
	c.invalidate()

	if f := c.entryFloor(); f != 7000 {
		t.Fatalf("floor = %d after invalidate(), want 7000 preserved: invalidation is about "+
			"freshness within one cluster, and a version this client committed stays "+
			"committed across it. Only a cluster-identity change may drop the floor", f)
	}
}

// TestClusterSwitch_LateCommitFromOldClusterCannotRaiseTheFloor is the durable
// arm of the cross-epoch fence, and the arm the first late-reply test missed by
// exercising publish() instead of update().
//
// The order-claim/time-claim split — a durable version may raise the floor
// without the generation, because it is a version comparison — is correct
// WITHIN one cluster. Across a cluster switch it is meaningless: A's versions
// and B's are unrelated numbers, so "9,000,000 > 500" proves nothing about
// order and everything about which cluster minted them. A late A commit that
// raises B's floor to 9M refuses every B publication forever, which is the
// exact wedge the epoch exists to prevent — reached through the one arm the
// epoch did not gate.
func TestClusterSwitch_LateCommitFromOldClusterCannotRaiseTheFloor(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	aGen := c.token()           // a commit dispatched against cluster A
	c.resetForNewCoordinators() // the handle moves to cluster B
	bGen := c.token()
	c.publish(bGen, base, 500) // B caches normally

	// A's commit lands late, carrying a version from A's unrelated space.
	c.update(aGen, base, 9_000_000)

	if f := c.entryFloor(); f != 0 {
		t.Fatalf("a commit from the PREVIOUS cluster raised this cluster's floor to %d.\n"+
			"Across an epoch boundary a version comparison proves nothing: A's 9000000 "+
			"and B's 500 are unrelated numbers. That floor now refuses every B "+
			"publication permanently — the wedge the epoch was built to prevent, "+
			"reached through the durable arm it did not gate", f)
	}
	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 500 {
		t.Fatalf("tryCache = (%d, %v), want (500, true): the late commit must leave B's "+
			"cache entirely alone — no install, no floor, no cooldown", v, ok)
	}
}

// TestClusterSwitch_EpochChangesAtTheProxyHandoff pins the window between
// adopting a new coordinator set and actually receiving that cluster's proxies.
//
// Adoption only changes which coordinators we will ASK; db.dbInfo still points
// at the old cluster's proxies until a later successful refresh installs the
// new ones. Bumping the epoch at adoption therefore opens a window in which a
// GRV captures the NEW generation, is dispatched to the OLD proxies, and its
// reply is accepted as current — the old cluster's version installed into what
// is nominally the new epoch, with no second reset when the real proxies land.
//
// C++ closes this by clearing commitProxies/grvProxies in the same block as the
// cache reset (switchConnectionRecord, NativeAPI.actor.cpp:2196-2207), so no
// dispatch to the old cluster can happen under the new epoch at all. Go reaches
// the same observable state by moving the epoch change to the PROXY HANDOFF:
// anything dispatched while the old proxies are still installed carries the old
// generation and is refused the moment the handoff bumps it.
func TestClusterSwitch_EpochChangesAtTheProxyHandoff(t *testing.T) {
	t.Parallel()

	base := time.Now()
	db := &database{}
	db.grvCache.now = func() time.Time { return base }

	db.grvCache.publish(db.grvCache.token(), base, 9_000_000) // cluster A cached

	// Adoption: the cluster file now names B's coordinators, but dbInfo still
	// holds A's proxies.
	db.onCoordinatorSetAdopted()

	// A GRV dispatched in this window goes to A's proxies. Whatever generation
	// it captures must not survive the handoff.
	inWindowGen := db.grvCache.token()

	// The handoff: B's proxies arrive.
	db.onProxySetInstalled()

	// The in-window reply lands after the handoff, carrying A's version.
	db.grvCache.publish(inWindowGen, base, 9_000_001)

	if v, _, ok := db.grvCache.tryCache(grvPriorityDefault); ok {
		t.Fatalf("a GRV dispatched between coordinator adoption and proxy handoff "+
			"installed version %d after the handoff.\n"+
			"That request went to the PREVIOUS cluster's proxies — adoption only "+
			"changes which coordinators we ask — so its reply belongs to the old "+
			"cluster and must not survive into the new epoch. The epoch has to change "+
			"where the proxies do, not where the cluster file does", v)
	}
}

// TestClusterSwitch_PreAdoptionCommitLandingPostHandoffIsRefusedOnBothArms is
// the interaction of the two fences. A commit dispatched before the handle even
// began switching, landing after the new cluster's proxies are installed, must
// be refused on BOTH arms: it may not install its version (generation) and it
// may not raise the floor or touch the cooldowns (epoch). Either arm alone
// leaves a hole — an install would serve another cluster's version, a floor
// would wedge this one permanently.
func TestClusterSwitch_PreAdoptionCommitLandingPostHandoffIsRefusedOnBothArms(t *testing.T) {
	t.Parallel()

	base := time.Now()
	db := &database{}
	db.grvCache.now = func() time.Time { return base }

	// A commit dispatched against cluster A, long before any switch.
	commitTok := db.grvCache.token()

	// The handle adopts B's coordinators, then receives B's proxies.
	db.onCoordinatorSetAdopted()
	db.onProxySetInstalled()

	// B caches normally.
	db.grvCache.publish(db.grvCache.token(), base, 500)

	// A's commit finally lands.
	db.grvCache.update(commitTok, base, 9_000_000)

	if f := db.grvCache.entryFloor(); f != 0 {
		t.Fatalf("the pre-adoption commit raised the new cluster's floor to %d — it would "+
			"refuse every version this cluster mints", f)
	}
	v, _, ok := db.grvCache.tryCache(grvPriorityDefault)
	if !ok || v != 500 {
		t.Fatalf("tryCache = (%d, %v), want (500, true): a commit that predates the whole "+
			"switch must touch nothing in the new cluster's cache", v, ok)
	}
}
