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

	got := mergeReply(sentinel, false /* generation moved */, base, 7000,
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
	c.publishReplyAt(gen, base, 400, base, false, true)

	if v := c.cachedVersion(); v != 400 {
		t.Fatalf("cachedVersion = %d, want 400 after the publication sequence: the entry "+
			"points do not all reach the same publication path", v)
	}
}
