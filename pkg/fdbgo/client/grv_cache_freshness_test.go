package client

// Freshness and the MVCC anchor are two different clocks with OPPOSITE rules,
// and collapsing them into one field breaks whichever it is not shaped for.
//
//   - The ANCHOR answers "when did this version's MVCC window open?". For a
//     given version that instant is fixed; a later observation of the same
//     version must NOT move it, because the window did not reopen. Taking the
//     later time overclaims the remaining budget, and RFC-198 admits a page
//     the cluster then refuses with 1007.
//
//   - FRESHNESS answers "how long since we last confirmed this cached entry
//     with the cluster?". Every successful GRV confirms it, INCLUDING one that
//     returns the version we already had — a quiet cluster returns the same
//     version repeatedly, and that is a healthy, freshly-confirmed cache, not
//     a stale one. C++ makes this instant monotonic for exactly that reason
//     (NativeAPI.actor.cpp:378-380, `if (t > lastGrvTime) lastGrvTime = t;`).
//
// ONE field carrying the anchor's rule is correct for the anchor and starves
// freshness: an equal-version refresh is discarded outright, so a read-only
// workload on a quiet cluster stops getting cache hits after 100ms no matter
// how many GRVs succeed, and the background refresher — whose pacing reads the
// same instant — pegs at its 1ms floor hammering the proxy forever.
//
// Every test below fails against that single-field design and passes with the
// two rules separated.

import (
	"testing"
	"time"
)

// TestGRVCache_EqualVersionRefreshKeepsTheCacheServing is the read-only
// workload on a quiet cluster: the same version comes back again and again,
// and each success must renew the entry's freshness.
func TestGRVCache_EqualVersionRefreshKeepsTheCacheServing(t *testing.T) {
	t.Parallel()

	base := time.Now()
	at := base
	c := &grvCache{now: func() time.Time { return at }}

	const sameVersion = 5000
	c.updateFromGRV(c.generation.Load(), base, sameVersion)

	// Refresh every 50ms — comfortably inside the 100ms window each time —
	// and always with the SAME version, as a quiet cluster returns.
	for step := 1; step <= 6; step++ {
		at = base.Add(time.Duration(step) * 50 * time.Millisecond)
		c.updateFromGRV(c.generation.Load(), at, sameVersion)

		v, _, ok := c.tryCache(grvPriorityDefault)
		if !ok {
			t.Fatalf("cache MISS %v after the run began, immediately after a successful "+
				"GRV that returned the cached version %d.\n"+
				"A repeated version is a quiet cluster, not a stale cache: the entry was "+
				"confirmed with the cluster this instant. Discarding equal-version "+
				"refreshes means a read-only workload gets ZERO cache hits forever once "+
				"the first observation ages past %v, defeating USE_GRV_CACHE entirely — "+
				"and it is why C++ keeps lastGrvTime monotonic "+
				"(NativeAPI.actor.cpp:378-380)",
				at.Sub(base), sameVersion, maxVersionCacheLag)
		}
		if v != sameVersion {
			t.Fatalf("cached version = %d, want %d", v, sameVersion)
		}
	}
}

// TestGRVCache_EqualVersionRefreshDoesNotMoveTheAnchor is the other half, and
// the reason the two cannot share a field: while freshness advances above, the
// ANCHOR for that version must stay exactly where it was.
func TestGRVCache_EqualVersionRefreshDoesNotMoveTheAnchor(t *testing.T) {
	t.Parallel()

	base := time.Now()
	at := base
	c := &grvCache{now: func() time.Time { return at }}

	const sameVersion = 6000
	c.updateFromGRV(c.generation.Load(), base, sameVersion)

	_, anchor0, ok := c.tryCache(grvPriorityDefault)
	if !ok {
		t.Fatal("no cache hit immediately after the first publish")
	}
	if !anchor0.Equal(base) {
		t.Fatalf("first anchor %v != the instant the version was obtained %v", anchor0, base)
	}

	// A later refresh returning the SAME version. Freshness may advance; the
	// anchor may not — version 6000's MVCC window opened once.
	at = base.Add(50 * time.Millisecond)
	c.updateFromGRV(c.generation.Load(), at, sameVersion)

	_, anchor1, ok := c.tryCache(grvPriorityDefault)
	if !ok {
		t.Fatal("no cache hit after an equal-version refresh")
	}
	if !anchor1.Equal(base) {
		t.Fatalf("anchor moved from %v to %v on an equal-version refresh (by %v): version "+
			"%d's window opened ONCE. Advancing the anchor claims the transaction is "+
			"younger than it is, so the RFC-198 budget over-grants and the page it "+
			"admits dies on 1007. Freshness advances here; the anchor does not",
			base, anchor1, anchor1.Sub(base), sameVersion)
	}
}

// TestGRVCache_RefresherDoesNotPegAtTheFloor pins the second consequence of
// starving freshness: the background refresher's cache budget goes unboundedly
// negative and clamps to grvRefreshMin, so a same-version refresh loop spins at
// 1ms against the proxy forever despite every GRV succeeding.
func TestGRVCache_RefresherDoesNotPegAtTheFloor(t *testing.T) {
	t.Parallel()

	base := time.Now()
	at := base
	c := &grvCache{now: func() time.Time { return at }}

	const sameVersion = 7000
	c.updateFromGRV(c.generation.Load(), base, sameVersion)
	c.lastProxyContact.Store(base.UnixNano())

	// The refresher's own loop: wait, GRV (same version), repeat.
	const grvDelay = 2 * time.Millisecond
	var pegged int
	const iterations = 8
	for i := 0; i < iterations; i++ {
		wait := nextGRVRefreshDelay(at, time.Unix(0, c.lastProxyContact.Load()),
			c.freshnessInstant(), grvDelay)
		if wait <= grvRefreshMin {
			pegged++
		}
		// Time advances by the wait, then the GRV succeeds with the same
		// version and refreshes both the proxy contact and freshness.
		at = at.Add(wait)
		c.updateFromGRV(c.generation.Load(), at, sameVersion)
		c.lastProxyContact.Store(at.UnixNano())
	}

	if pegged > 0 {
		t.Fatalf("%d of %d refresh iterations waited only the %v floor. A same-version "+
			"refresh loop is the STEADY STATE of a quiet cluster, and every one of "+
			"those GRVs succeeded — pacing that collapses to the floor there means the "+
			"client hammers the proxy indefinitely because its freshness instant never "+
			"advanced. The refresher must pace off freshness, which equal-version "+
			"replies renew, never off the version anchor, which they must not move",
			pegged, iterations, grvRefreshMin)
	}
}
