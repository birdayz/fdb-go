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

	got := mergeReply(sentinel, false /* generation moved */, true /* durable */, base, 7000,
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
	c.publish(c.generation.Load(), base, 5000)

	// The commit is dispatched: the generation is captured here, beside
	// commitStart, before the RPC goes out.
	commitGen := c.generation.Load()

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
	c.publish(c.generation.Load(), base, 5000)
	c.invalidate()

	commitGen := c.generation.Load() // dispatched after the invalidation
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
	gen := c.generation.Load()

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

// TestCommitWarming_BlockedCommitStillRetiresAStaleEntry is the dual of the
// generation gate, and the case the gate on its own gets wrong.
//
// A commit dispatched under generation N is overtaken by an invalidation; a
// generation-N+1 GRV then caches an OLDER version; the commit lands. Refusing
// the install is right — the commit's anchor belongs to a pre-invalidation
// dispatch. Leaving the older version servable is not: the caller's Commit has
// RETURNED, so that write is durable, and the next USE_GRV_CACHE transaction
// would read at a version that provably predates it. Read-your-committed-writes
// is violated by the very mechanism that protects InvalidateGRVCache.
//
// Retiring needs no generation, and that asymmetry is the whole design: an
// install is a claim about WHEN, which a stale dispatch cannot make; a retire
// is a claim about ORDER, which a version comparison settles on its own.
func TestCommitWarming_BlockedCommitStillRetiresAStaleEntry(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	// The commit is dispatched under the current generation.
	commitGen := c.generation.Load()
	// An invalidation overtakes it.
	c.invalidate()
	// A GRV dispatched AFTER the invalidation caches an older version.
	c.publish(c.generation.Load(), base, 6000)
	if v, _, ok := c.tryCache(grvPriorityDefault); !ok || v != 6000 {
		t.Fatalf("precondition: expected 6000 cached and servable, got (%d, %v)", v, ok)
	}

	// The commit lands with a HIGHER version.
	c.update(commitGen, base, 7000)

	if v, _, ok := c.tryCache(grvPriorityDefault); ok {
		t.Fatalf("the cache is still serving version %d after a commit at 7000 RETURNED.\n"+
			"7000 is durable, so 6000 provably predates it and every read served from "+
			"it misses the caller's own just-committed write. The blocked commit may "+
			"not INSTALL its version — its anchor belongs to a pre-invalidation "+
			"dispatch — but it must still RETIRE one it proves stale, which needs no "+
			"generation because it is a version comparison", v)
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

			commitGen := c.generation.Load()
			c.invalidate()
			c.publish(c.generation.Load(), base, tc.cached)

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
