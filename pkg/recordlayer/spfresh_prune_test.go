package recordlayer

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// SPANN Eq. (3) query-aware dynamic pruning (RFC-094 §4; paper §3.2.3): probe
// list ij ⟺ Dist(q,c_ij) ≤ (1+ε)·Dist(q,c_i1). Dist is SPTAG's SQUARED L2
// and the ratio applies to it DIRECTLY (the published ε₂=7.0 = an 8× d²
// bound) — squaring the ratio was the calibration bug that made ε=7.0 inert
// on the 1M sweep (paper-review catch). Pruned tail kept for starvation
// widening.
var _ = Describe("SPFresh ε-pruning", func() {
	ctx := context.Background()

	rt := func(fine int64, d2 float64) spfreshRouted {
		return spfreshRouted{fineID: fine, d2: d2, state: spfreshStateActive}
	}

	It("splits a routed list at the (1+ε) squared-distance threshold", func() {
		routed := []spfreshRouted{rt(1, 1.0), rt(2, 1.9), rt(3, 2.0), rt(4, 2.1), rt(5, 100)}
		// ε=1 → threshold (1+1)·d2(c1) = 2.0 ON d², boundary INCLUSIVE.
		probe, pruned := spfreshPruneRouted(routed, 1.0)
		Expect(probe).To(HaveLen(3))
		Expect(pruned).To(HaveLen(2))
		Expect(pruned[0].fineID).To(Equal(int64(4)))

		// The published SPANN §4.2 point: ε=7 ⇒ 8× in d² (NOT 64× — the
		// ratio is not squared again).
		eight := []spfreshRouted{rt(1, 1.0), rt(2, 8.0), rt(3, 8.1), rt(4, 63.9)}
		probe, pruned = spfreshPruneRouted(eight, 7.0)
		Expect(probe).To(HaveLen(2), "8·d² is in, anything past it is out")
		Expect(pruned).To(HaveLen(2))
		Expect(pruned[0].fineID).To(Equal(int64(3)))

		// ε ≤ 0 disables: everything probed.
		probe, pruned = spfreshPruneRouted(routed, 0)
		Expect(probe).To(HaveLen(5))
		Expect(pruned).To(BeEmpty())

		// The nearest list always survives, even at d1 = 0 (exact-centroid
		// queries prune everything farther).
		zero := []spfreshRouted{rt(1, 0), rt(2, 0), rt(3, 0.5)}
		probe, pruned = spfreshPruneRouted(zero, 7.0)
		Expect(probe).To(HaveLen(2), "equal-distance lists share the threshold")
		Expect(pruned).To(HaveLen(1))

		// Single candidate: untouched.
		probe, pruned = spfreshPruneRouted(routed[:1], 7.0)
		Expect(probe).To(HaveLen(1))
		Expect(pruned).To(BeEmpty())
	})

	It("starved probes widen into the pruned tail (recall survives aggressive ε)", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 4
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-prune").Sub("starve"), 1)

		// Two far-apart clusters: 3 vectors at the origin, 10 at (50,50).
		var inputs []spfreshBuildInput
		var all [][]float64
		id := int64(1)
		for i := 0; i < 3; i++ {
			v := []float64{float64(i) * 0.1, float64(i) * 0.1}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
			all = append(all, v)
			id++
		}
		for i := 0; i < 10; i++ {
			v := []float64{50 + float64(i%4)*0.1, 50 + float64(i/4)*0.1}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
			all = append(all, v)
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-prune")
		Expect(builder.build(ctx, inputs, 7)).To(Succeed())

		query := []float64{0, 0}
		k := 8
		// Brute-force truth over the fp16-pinned vectors.
		type cand struct {
			id int64
			d2 float64
		}
		var truth []cand
		for i, v := range all {
			rtv, rerr := vectorcodecRoundtrip(v)
			Expect(rerr).NotTo(HaveOccurred())
			truth = append(truth, cand{id: int64(i + 1), d2: spfreshSquaredDistance(query, rtv)})
		}
		sort.Slice(truth, func(i, j int) bool {
			if truth[i].d2 != truth[j].d2 {
				return truth[i].d2 < truth[j].d2
			}
			return truth[i].id < truth[j].id
		})
		want := map[int64]bool{}
		for _, c := range truth[:k] {
			want[c.id] = true
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cache := newSPFreshRoutingCache(0)
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			searcher.kc = 32
			// Near-zero ε: only the origin cluster's nearest list survives
			// pruning, which holds 3 < k entries — the starvation widening
			// MUST pull the pruned tail or the query returns short/wrong.
			searcher.epsilon = 0.0001
			results, serr := searcher.search(tx, query, k)
			if serr != nil {
				return nil, serr
			}
			Expect(results).To(HaveLen(k), "starved probe set must widen into the pruned tail")
			for _, r := range results {
				Expect(want[r.PrimaryKey[0].(int64)]).To(BeTrue(),
					"widened search must return the true nearest neighbors")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
