package recordlayer

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Multi-tenant cache-map eviction: a serving fleet touches arbitrarily many
// indexes, so the process-global spfreshCaches map must shed idle tenants
// (TTL) and bound itself (cap, oldest-first). All assertions here are on the
// test's OWN keys only — the map is shared with concurrently running specs,
// whose caches carry fresh lastUse stamps and are never evicted by these
// sweeps.
var _ = Describe("SPFresh routing-cache eviction across tenants", func() {
	cacheKey := func(sub subspace.Subspace, gen int64) string {
		return fmt.Sprintf("%x/%d", sub.Bytes(), gen)
	}

	It("evicts idle tenants by TTL and keeps live ones", func() {
		idleSub := specSubspace().Sub("spfresh-evict").Sub("idle")
		liveSub := specSubspace().Sub("spfresh-evict").Sub("live")
		idle := spfreshCacheFor(idleSub, 1)
		_ = spfreshCacheFor(liveSub, 1) // fresh stamp: must survive

		now := spfreshNowMs()
		idle.lastUseMs.Store(now - spfreshCacheIdleTTLMs - 1)
		spfreshSweepCaches(now)

		_, idlePresent := spfreshCaches.Load(cacheKey(idleSub, 1))
		Expect(idlePresent).To(BeFalse(), "idle tenant cache must be TTL-evicted")
		_, livePresent := spfreshCaches.Load(cacheKey(liveSub, 1))
		Expect(livePresent).To(BeTrue(), "live tenant cache must survive the sweep")

		// The evicted tenant's next touch transparently recreates the cache.
		again := spfreshCacheFor(idleSub, 1)
		Expect(again).NotTo(BeNil())
		Expect(again).NotTo(BeIdenticalTo(idle), "post-eviction access must mint a fresh cache")
	})

	It("caps the map across tenants, evicting oldest-first", func() {
		base := specSubspace().Sub("spfresh-evict").Sub("cap")
		now := spfreshNowMs()
		// Old-but-live stamps, strictly older than any concurrent spec's
		// fresh caches and strictly ordered among themselves: index i is
		// OLDER than i+1.
		n := int(spfreshCacheMaxEntries) + 50
		subs := make([]subspace.Subspace, n)
		for i := 0; i < n; i++ {
			subs[i] = base.Sub(tuple.Tuple{int64(i)})
			c := spfreshCacheFor(subs[i], 1)
			c.lastUseMs.Store(now - 60_000 - int64(n-i)) // within TTL, ordered
		}
		spfreshSweepCaches(now)

		// Over-cap eviction is oldest-first down to 90% of the cap: the
		// oldest of OUR entries must be gone, the newest must survive.
		_, oldest := spfreshCaches.Load(cacheKey(subs[0], 1))
		Expect(oldest).To(BeFalse(), "oldest tenant must be cap-evicted")
		_, newest := spfreshCaches.Load(cacheKey(subs[n-1], 1))
		Expect(newest).To(BeTrue(), "newest tenants must survive cap eviction")
		Expect(spfreshCacheCount.Load()).To(BeNumerically("<=", spfreshCacheMaxEntries))

		// Clean up our flood so later specs/sweeps aren't dominated by it.
		for i := 0; i < n; i++ {
			if c, ok := spfreshCaches.Load(cacheKey(subs[i], 1)); ok {
				c.(*spfreshRoutingCache).lastUseMs.Store(now - spfreshCacheIdleTTLMs - 1)
			}
		}
		spfreshSweepCaches(spfreshNowMs())
	})
})
