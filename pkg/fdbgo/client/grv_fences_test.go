package client

// The two fences — the invalidation generation and the cluster epoch — are ONE
// word, and this file is why.
//
// As two atomics, a coordinator handoff bumps them in sequence, and a publisher
// that tokened before the handoff can load the pair BETWEEN the two bumps. It
// then observes mayInstall=true (the generation has not moved yet) beside
// sameEpoch=false (the epoch already has) — a combination describing no state
// the cache was ever in. Under it mergeReply installs a previous-cluster version
// over a valid current-epoch entry, and then correctly declines to stamp a
// stale-epoch publication, leaving the cache holding an entry that is unservable
// at epoch 0. The current cluster's cached version is gone and the replacement
// can never be served.
//
// Packed into one word, mayInstall is `obs == tok` and therefore IMPLIES
// sameEpoch. The skew is not merely unlikely; it cannot be expressed.

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestFences_MayInstallImpliesSameEpoch is the structural pin, stated over the
// whole space of (observed, token) pairs rather than over the handful a race can
// be coaxed into producing. It is the property that makes the skew
// unrepresentable, so it is asserted directly.
func TestFences_MayInstallImpliesSameEpoch(t *testing.T) {
	t.Parallel()

	for _, obsE := range []int64{0, 1, 7} {
		for _, obsG := range []int64{0, 1, 9} {
			for _, tokE := range []int64{0, 1, 7} {
				for _, tokG := range []int64{0, 1, 9} {
					obs := packFences(obsE, obsG)
					tok := cacheToken{f: packFences(tokE, tokG)}
					mayInstall := obs == tok.f
					sameEpoch := fenceEpoch(obs) == tok.epoch()
					if mayInstall && !sameEpoch {
						t.Fatalf("obs=(epoch %d, gen %d) token=(epoch %d, gen %d) yields "+
							"mayInstall=true with sameEpoch=false.\n"+
							"That is the fence skew the packed word exists to make "+
							"unrepresentable: under it a previous-cluster reply installs its "+
							"version over a valid current-epoch entry and then declines to "+
							"stamp it, leaving an unservable epoch-0 entry where the current "+
							"cluster's cached version used to be",
							obsE, obsG, tokE, tokG)
					}
				}
			}
		}
	}
}

// TestMergeReply_FenceTable is the 2x2 over (mayInstall x sameEpoch), asserting
// the STAMPED EPOCH of the result and not only its version — the stamp is what
// decides whether the merged content counts at all, and a table that checks only
// the version passes with the stamp wrong.
//
// Three cells, not four: the fourth (mayInstall without sameEpoch) has no
// (obs, tok) pair that produces it, which is the one-fence-word invariant above.
func TestMergeReply_FenceTable(t *testing.T) {
	t.Parallel()

	base := time.Now()
	// The entry already cached, published under epoch 3.
	cur := &grvCacheEntry{epoch: 3, version: 5000, anchor: base, freshness: base, floor: 4000}

	for _, tc := range []struct {
		name       string
		obs        uint64
		tok        cacheToken
		wantVer    int64
		wantEpoch  int64
		wantFloor  int64
		mayInstall bool
		sameEpoch  bool
	}{
		{
			// Neither fence moved: a plain successful publication.
			name: "may install, same epoch", obs: packFences(3, 2), tok: cacheToken{f: packFences(3, 2)},
			wantVer: 7000, wantEpoch: 3, wantFloor: 7000, mayInstall: true, sameEpoch: true,
		},
		{
			// An invalidation moved the generation: the version is void, but the
			// commit's ORDER claim still stands within the cluster, so the floor
			// rises and the entry keeps the epoch it already had.
			name: "blocked, same epoch", obs: packFences(3, 3), tok: cacheToken{f: packFences(3, 2)},
			wantVer: 5000, wantEpoch: 3, wantFloor: 7000, mayInstall: false, sameEpoch: true,
		},
		{
			// A handoff moved both halves: the publication belongs to another
			// cluster and may touch NOTHING — not the version, not the floor, and
			// above all not the stamp, which would promote the previous cluster's
			// content into the current epoch and make it servable.
			name: "blocked, previous epoch", obs: packFences(4, 3), tok: cacheToken{f: packFences(3, 2)},
			wantVer: 5000, wantEpoch: 3, wantFloor: 4000, mayInstall: false, sameEpoch: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.obs == tc.tok.f; got != tc.mayInstall {
				t.Fatalf("table setup: mayInstall = %v, want %v", got, tc.mayInstall)
			}
			if got := fenceEpoch(tc.obs) == tc.tok.epoch(); got != tc.sameEpoch {
				t.Fatalf("table setup: sameEpoch = %v, want %v", got, tc.sameEpoch)
			}

			got := mergeReply(cur, tc.obs, tc.tok, true /* durable */, base, 7000,
				time.Time{}, false, false)

			if got.version != tc.wantVer {
				t.Errorf("version = %d, want %d", got.version, tc.wantVer)
			}
			if got.floor != tc.wantFloor {
				t.Errorf("floor = %d, want %d", got.floor, tc.wantFloor)
			}
			if got.epoch != tc.wantEpoch {
				t.Errorf("STAMPED epoch = %d, want %d.\n"+
					"The stamp decides whether this entry's content counts. Stamping a "+
					"stale-epoch publication promotes a previous cluster's entry into the "+
					"current epoch and makes it servable; failing to stamp a current-epoch "+
					"one makes a perfectly good version unservable at epoch 0",
					got.epoch, tc.wantEpoch)
			}
		})
	}
}

// TestFences_StaleEpochWithCurrentGenerationCannotClobber is the DETERMINISTIC
// reproducer for the skew, and it needs no race at all — because the skewed
// combination is reachable on the live path through a composed token, not only
// through the instant between two bumps.
//
// A GRV attempt selects its proxies, then the handoff bumps the fences, then the
// reply lands. The token it presents carries the CURRENT generation (nothing
// invalidated) beside the epoch of the proxy set it actually used, which is the
// previous cluster's. A generation-only install predicate says yes to that: the
// previous cluster's version overwrites the new cluster's entry and, correctly
// declining to stamp a stale-epoch publication, leaves behind an entry the
// reader will never serve. The install predicate has to be the WHOLE fence word.
func TestFences_StaleEpochWithCurrentGenerationCannotClobber(t *testing.T) {
	t.Parallel()

	base := time.Now()
	c := &grvCache{now: func() time.Time { return base }}

	c.resetForNewCoordinators()     // the handle moves to cluster B
	c.publish(c.token(), base, 500) // B caches normally
	if v, _, ok := c.tryCache(grvPriorityDefault); !ok || v != 500 {
		t.Fatalf("precondition: tryCache = (%d, %v), want (500, true)", v, ok)
	}

	// The straggler: current generation, previous cluster's epoch.
	straggler := c.token().withEpoch(0)
	if straggler.gen() != c.token().gen() {
		t.Fatalf("setup: the straggler must carry the CURRENT generation (got %d, cache at %d) "+
			"— that is what makes a generation-only predicate accept it",
			straggler.gen(), c.token().gen())
	}
	c.publish(straggler, base, 9_000_000)

	v, _, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 500 {
		t.Fatalf("tryCache = (%d, %v), want (500, true).\n"+
			"A reply from the PREVIOUS cluster — current generation, previous epoch — "+
			"displaced the current cluster's cached version. Whether it installed its "+
			"own version or left an unstamped entry behind, the current cluster's cache "+
			"is gone. mayInstall must be the whole fence word (obs == tok), not its "+
			"generation half", v, ok)
	}
	if e := c.entry.Load(); e != nil && e.epoch != c.epochNow() {
		t.Fatalf("the entry is stamped at epoch %d while the cache serves epoch %d: the "+
			"straggler clobbered a valid entry with one that can never be served",
			e.epoch, c.epochNow())
	}
}

// TestFences_ResetIsAtomicAcrossBothHalves is the behavioural companion to the
// deterministic pin above: it drives the real reset against real concurrent
// publishers. The window a two-atomic reset opens is a few instructions wide, so
// this can only ever CATCH the skew, never invent it — the pin above is what
// makes the property assertable.
func TestFences_ResetIsAtomicAcrossBothHalves(t *testing.T) {
	t.Parallel()

	const attempts = 400
	for i := 0; i < attempts; i++ {
		base := time.Now()
		c := &grvCache{now: func() time.Time { return base }}

		// Cluster A has a good cached version, and a publisher is in flight
		// against it.
		c.publish(c.token(), base, 9_000_000)
		inFlight := c.token()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.resetForNewCoordinators()
		}()
		go func() {
			defer wg.Done()
			c.publish(inFlight, base, 9_000_001)
		}()
		wg.Wait()

		// The handoff has happened, so nothing from cluster A may be servable —
		// whether the in-flight publisher landed before it (an A entry at A's
		// epoch, refused by the reader) or after it (refused by the fence).
		if v, _, ok := c.tryCache(grvPriorityDefault); ok {
			t.Fatalf("attempt %d/%d: after a coordinator handoff the cache serves version "+
				"%d, published by a request dispatched to the PREVIOUS cluster",
				i+1, attempts, v)
		}

		// And the new cluster caches normally: a reset that let a skewed
		// publisher through would leave an entry stamped at an epoch the cache
		// never serves, and cluster B's low version could not displace it.
		c.publish(c.token(), base, 500)
		if v, _, ok := c.tryCache(grvPriorityDefault); !ok || v != 500 {
			t.Fatalf("attempt %d/%d: tryCache = (%d, %v), want (500, true).\n"+
				"A publisher that tokened before the handoff observed the state BETWEEN "+
				"the epoch bump and the generation bump, installed cluster A's version "+
				"without stamping it, and left the cache holding an entry it will never "+
				"serve. One fence word makes that observation impossible",
				i+1, attempts, v, ok)
		}
	}
}

// TestFences_InvalidateLeavesTheEpochAlone is the other half of the packing
// contract: an invalidation is a within-cluster event, so it must move the
// generation and ONLY the generation. Moving the epoch too would make every
// cached entry — and every in-flight commit's durable floor — cross-cluster
// garbage after a routine InvalidateGRVCache.
func TestFences_InvalidateLeavesTheEpochAlone(t *testing.T) {
	t.Parallel()

	for _, adoptions := range []int{0, 1, 5} {
		t.Run(fmt.Sprintf("after_%d_adoptions", adoptions), func(t *testing.T) {
			t.Parallel()
			c := &grvCache{}
			for i := 0; i < adoptions; i++ {
				c.resetForNewCoordinators()
			}
			before := c.token()
			c.invalidate()
			after := c.token()

			if after.epoch() != before.epoch() {
				t.Fatalf("invalidate() moved the epoch %d -> %d. An invalidation is a "+
					"within-cluster event; moving the epoch discards the committed floor "+
					"and the ratekeeper cooldowns it is explicitly required to preserve",
					before.epoch(), after.epoch())
			}
			if after.gen() != before.gen()+1 {
				t.Fatalf("invalidate() moved the generation %d -> %d, want +1",
					before.gen(), after.gen())
			}
		})
	}
}

// TestFences_ResetMovesBothHalves is the dual control: a coordinator handoff
// must move BOTH, because the epoch alone would let a pre-handoff reply install
// (its generation still matches) and the generation alone would let a previous
// cluster's ENTRY stay servable.
func TestFences_ResetMovesBothHalves(t *testing.T) {
	t.Parallel()

	c := &grvCache{}
	before := c.token()
	epoch := c.resetForNewCoordinators()
	after := c.token()

	if after.epoch() != before.epoch()+1 || after.gen() != before.gen()+1 {
		t.Fatalf("reset moved fences (epoch %d, gen %d) -> (epoch %d, gen %d), want both +1",
			before.epoch(), before.gen(), after.epoch(), after.gen())
	}
	if epoch != after.epoch() {
		t.Fatalf("resetForNewCoordinators returned epoch %d but installed %d: the DBInfo "+
			"published next carries the returned value, so a mismatch desynchronises the "+
			"fence word from the proxy set it is supposed to describe", epoch, after.epoch())
	}
}

// TestPackFences_GenerationWrapCannotReachTheEpoch pins the reason both bumps
// are CAS loops rather than atomic Adds. An Add on a packed word carries, so the
// generation's wrap at 2^32 would silently increment the cluster epoch — after
// which every cached entry is unservable and every in-flight reply is refused,
// on a handle that never changed clusters.
func TestPackFences_GenerationWrapCannotReachTheEpoch(t *testing.T) {
	t.Parallel()

	const maxGen = int64(1)<<32 - 1
	f := packFences(3, maxGen)
	if fenceEpoch(f) != 3 || fenceGen(f) != maxGen {
		t.Fatalf("packFences(3, maxGen) = (epoch %d, gen %d), want (3, %d)",
			fenceEpoch(f), fenceGen(f), maxGen)
	}
	// The bump the CAS loop performs at the wrap point.
	wrapped := packFences(fenceEpoch(f), fenceGen(f)+1)
	if fenceEpoch(wrapped) != 3 {
		t.Fatalf("the generation wrapped into the epoch: %d -> %d.\n"+
			"A carry between the halves turns a routine invalidation into a phantom "+
			"cluster switch, which is why both bumps recompute the halves under a CAS "+
			"instead of adding to the packed word", fenceEpoch(f), fenceEpoch(wrapped))
	}
	if fenceGen(wrapped) != 0 {
		t.Fatalf("generation = %d after wrap, want 0", fenceGen(wrapped))
	}
}
