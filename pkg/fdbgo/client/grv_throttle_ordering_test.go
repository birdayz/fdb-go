package client

// A GRV reply that renews the cache AND reports ratekeeper throttling must
// never leave a window in which the cache is servable but the cooldown has not
// landed.
//
// tryCache serves an entry that is FRESH and outside the ratekeeper cooldown.
// While those were two separately-published values, a reply that renewed
// freshness before storing the cooldown left a state a concurrent opted-in
// transaction could catch: served from an entry this very reply had just
// refreshed, bypassing the throttling the same reply was delivering.
//
// C++ writes them as separate statements too, in that same order
// (extractReadVersion: updateCachedReadVersion at NativeAPI.actor.cpp:7409,
// then lastRkBatchThrottleTime/lastRkDefaultThrottleTime at :7411-7416), and
// is safe regardless — no wait() separates those statements, so the actor
// cannot yield and no other actor observes the intermediate state. Go has real
// threads and no such guarantee, so matching C++ here means reproducing which
// states an observer can SEE, not copying the statement order.
//
// Reordering the two stores would close the window; publishing them as one
// value closes it by CONSTRUCTION, and that is what the cache does. The
// cooldowns live inside the same immutable entry as the freshness they must be
// consulted with, so "renew freshness without the cooldown" is not a state the
// type can represent — a property the next edit to applyGRVReply cannot
// silently undo, which an ordering convention cannot promise.
//
// Both the equal-version and the new-version paths are covered. Only the
// equal-version exposure was new (before freshness renewal existed, an expired
// entry stayed unservable through the whole reply); the new-version one is
// older than this workstream.

import (
	"sync"
	"testing"
	"time"
)

// TestGRVReply_ThrottleAndFreshnessArePublishedTogether is the DETERMINISTIC
// pin: no goroutines, no interleaving luck. It asserts the two facts tryCache
// consults arrive in the same entry, which is what makes the window
// unrepresentable rather than merely unlikely.
func TestGRVReply_ThrottleAndFreshnessArePublishedTogether(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		replyVersion int64
	}{
		{"equal version", 9000},
		{"new version", 9001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := time.Now()
			db := &database{}
			db.grvCache.now = func() time.Time { return base }
			b := &grvBatcher{priority: grvPriorityDefault}

			// Expired entry, no cooldown yet: unservable only because it is stale.
			db.grvCache.publish(base.Add(-2*maxVersionCacheLag), 9000)
			before := db.grvCache.entry.Load()
			if !before.rkDefault.IsZero() {
				t.Fatal("precondition: a cooldown is already recorded")
			}

			publishedBefore := db.grvCache.publications.Load()
			b.applyGRVReply(db, db.grvCache.generation.Load(), base, tc.replyVersion, true, false, nil)

			// ONE publication, and this is the assertion that makes the window
			// unrepresentable rather than merely unlikely. Two publications —
			// freshness first, cooldown after, in either order — leave an entry
			// visible in between, and a reader holding it is served past the
			// ratekeeper. Checking only the FINAL state cannot see that: the
			// split path reaches an identical end state, one step later.
			if n := db.grvCache.publications.Load() - publishedBefore; n != 1 {
				t.Fatalf("the reply published the entry %d times, want exactly 1. Everything "+
					"tryCache consults — freshness AND the ratekeeper cooldown — must "+
					"arrive in ONE publication; any intermediate entry is a state where "+
					"the cache looks fresh and un-throttled, and an opted-in transaction "+
					"that loads it bypasses the throttling this reply was delivering", n)
			}

			// ONE load. Both facts must be here — if the cooldown were still a
			// separate value, this entry could carry the renewed freshness
			// alone and a reader holding it would be served.
			after := db.grvCache.entry.Load()
			if after == before {
				t.Fatal("the reply published nothing")
			}
			if !after.freshness.Equal(base) {
				t.Fatalf("freshness = %v, want %v: the reply did not renew the entry, so "+
					"this test cannot say anything about what it was published with",
					after.freshness, base)
			}
			if after.rkDefault.IsZero() {
				t.Fatalf("the entry carrying the renewed freshness has NO ratekeeper " +
					"cooldown. The reply reported throttling, so an entry that is fresh " +
					"and un-throttled is exactly the state that gets an opted-in " +
					"transaction served past the ratekeeper. The cooldown must be " +
					"published WITH the freshness it accompanies, not after it")
			}

			// And the net effect: throttled, so not servable.
			if v, _, ok := db.grvCache.tryCache(grvPriorityDefault); ok {
				t.Fatalf("cache served version %d after a reply that reported ratekeeper "+
					"throttling", v)
			}
		})
	}
}

// TestGRVReply_UnthrottledReplyStillServes keeps the test above honest: it
// would pass just as well against a cache that never serves anything. An
// identical reply WITHOUT throttling must leave the entry servable.
func TestGRVReply_UnthrottledReplyStillServes(t *testing.T) {
	t.Parallel()

	base := time.Now()
	db := &database{}
	db.grvCache.now = func() time.Time { return base }
	b := &grvBatcher{priority: grvPriorityDefault}

	db.grvCache.publish(base.Add(-2*maxVersionCacheLag), 9000)
	b.applyGRVReply(db, db.grvCache.generation.Load(), base, 9000, false, false, nil)

	v, _, ok := db.grvCache.tryCache(grvPriorityDefault)
	if !ok {
		t.Fatal("an un-throttled reply that renewed the entry left it unservable: the " +
			"throttle test above would then pass for the wrong reason, since every " +
			"observation would be a miss no matter what the cooldown said")
	}
	if v != 9000 {
		t.Fatalf("served version %d, want 9000", v)
	}
}

// throttleOrderingProbe is the concurrent net beside the deterministic pin: it
// hammers the real reply path with a reader spinning on tryCache. It can only
// ever CATCH a window, never invent one — every state it may legitimately
// observe is a miss:
//
//	before the reply  — freshness expired            → miss
//	after  the reply  — throttled by this very reply → miss
//
// It is kept because it exercises applyGRVReply end to end under real
// concurrency, and deliberately NOT relied on as the primary pin: a window
// this narrow is caught probabilistically, so a green run is weak evidence
// while a red one is proof.
func throttleOrderingProbe(t *testing.T, replyVersion int64) {
	t.Helper()

	const attempts = 300
	for i := 0; i < attempts; i++ {
		base := time.Now()
		db := &database{}
		db.grvCache.now = func() time.Time { return base }
		b := &grvBatcher{priority: grvPriorityDefault}

		const cachedVersion = 9000
		db.grvCache.publish(base.Add(-2*maxVersionCacheLag), cachedVersion)
		if _, _, ok := db.grvCache.tryCache(grvPriorityDefault); ok {
			t.Fatalf("precondition: the expired entry is servable before the reply — this "+
				"probe can only detect the window if every state EXCEPT the intermediate "+
				"one is a miss (freshness %v old, window %v)",
				2*maxVersionCacheLag, maxVersionCacheLag)
		}
		if !db.grvCache.throttleStamp(grvPriorityDefault).IsZero() {
			t.Fatal("precondition: a cooldown is already recorded")
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		var served bool
		stop := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, ok := db.grvCache.tryCache(grvPriorityDefault); ok {
					mu.Lock()
					served = true
					mu.Unlock()
					return
				}
			}
		}()

		b.applyGRVReply(db, db.grvCache.generation.Load(), base, replyVersion, true, false, nil)

		close(stop)
		wg.Wait()

		mu.Lock()
		caught := served
		mu.Unlock()
		if caught {
			t.Fatalf("a concurrent transaction was SERVED from the cache during a GRV reply "+
				"that reported ratekeeper throttling (attempt %d/%d, reply version %d vs "+
				"cached %d).\n"+
				"Every state this probe can legitimately observe is a miss, so being "+
				"served means the reader caught the entry with its freshness renewed and "+
				"the cooldown not yet visible — an opted-in transaction bypassing the "+
				"ratekeeper throttling the reply was carrying. Publish the cooldown in "+
				"the SAME entry as the freshness",
				i+1, attempts, replyVersion, cachedVersion)
		}
	}
}

func TestGRVReply_EqualVersionThrottleHasNoServableWindow(t *testing.T) {
	t.Parallel()
	throttleOrderingProbe(t, 9000) // equal to the cached version
}

func TestGRVReply_NewVersionThrottleHasNoServableWindow(t *testing.T) {
	t.Parallel()
	throttleOrderingProbe(t, 9001) // higher than the cached version
}
