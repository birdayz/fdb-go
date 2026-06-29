package recordlayer

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

	It("widens past the rerank budget when k > c (large-k under pruning)", func() {
		// codex full-PR P2: the starvation widening gated on s.c (the rerank
		// budget), but the re-rank keeps cTop = max(s.c, k). With k > s.c a
		// pruned query stopped widening once the probe set filled s.c and
		// returned FEWER than k rows despite enough indexed records. Here the
		// probe set (3-vector origin cluster) exceeds a small c but is < k, and
		// the answer lives in the pruned far cluster — without the fix the query
		// returns 3, not k.
		config := DefaultSPFreshConfig(2)
		config.Lmax = 4
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-prune").Sub("largek"), 1)

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
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-largek")
		Expect(builder.build(ctx, inputs, 7)).To(Succeed())

		query := []float64{0, 0}
		k := 8
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
			searcher.epsilon = 0.0001 // prune the far cluster's lists
			searcher.c = 1            // rerank budget BELOW k — the codex k>c case
			results, serr := searcher.search(tx, query, k)
			if serr != nil {
				return nil, serr
			}
			Expect(results).To(HaveLen(k),
				"k > c must still widen into the pruned tail and return k rows")
			for _, r := range results {
				Expect(want[r.PrimaryKey[0].(int64)]).To(BeTrue(),
					"large-k widened search must return the true nearest neighbors")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// RFC-156 Phase A: the one-shot search() is refactored into a resumable
// frontier (searchInit + searchNext). These specs pin the spfresh-reviewer ACK
// conditions: (1) ε-pruning starvation widening ≡ one-shot batch prune,
// (2) re-rank only finalized prefixes — emitted order is exact distance order,
// never RaBitQ-estimate order, and (4) the wrapper is byte-for-byte the
// pre-refactor result (proven against a brute-force golden across k).
var _ = Describe("SPFresh resumable search (RFC-156 Phase A)", func() {
	ctx := context.Background()

	// buildIdx builds a deterministic SPFresh index over inputs and returns the
	// storage handle (gen 1) plus its config. dims-D, Euclidean, sidecar on.
	buildIdx := func(name string, lmax int, inputs []spfreshBuildInput, dims int) (*spfreshStorage, SPFreshConfig) {
		config := DefaultSPFreshConfig(dims)
		config.Lmax = lmax
		storage := newSPFreshStorage(specSubspace().Sub(name), 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-"+name)
		Expect(builder.build(ctx, inputs, 7)).To(Succeed())
		return storage, config
	}

	// runSearch reloads the routing cache and runs one search() with the given
	// per-query tuning, returning the results and the instrumentation timer.
	runSearch := func(storage *spfreshStorage, config SPFreshConfig, query []float64, k int, tune func(*spfreshSearcher)) ([]spfreshSearchResult, *StoreTimer) {
		timer := NewStoreTimer()
		var out []spfreshSearchResult
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cache := newSPFreshRoutingCache(0)
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			searcher.timer = timer
			if tune != nil {
				tune(searcher)
			}
			res, serr := searcher.search(tx, query, k)
			if serr != nil {
				return nil, serr
			}
			out = res
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out, timer
	}

	pkIDs := func(res []spfreshSearchResult) []int64 {
		ids := make([]int64, len(res))
		for i, r := range res {
			ids[i] = r.PrimaryKey[0].(int64)
		}
		return ids
	}

	// bruteForce returns the true k nearest ids over the fp16-roundtripped
	// vectors (the sidecar stores fp16, and re-rank reads it — the golden must
	// use the same precision). Tie-break by id ascending.
	bruteForce := func(ids []int64, vecs [][]float64, query []float64, metric VectorMetric, k int) []int64 {
		type cand struct {
			id int64
			d  float64
		}
		cs := make([]cand, len(vecs))
		for i, v := range vecs {
			rtv, rerr := vectorcodecRoundtrip(v)
			Expect(rerr).NotTo(HaveOccurred())
			cs[i] = cand{ids[i], vectorDistance(query, rtv, metric)}
		}
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].d != cs[j].d {
				return cs[i].d < cs[j].d
			}
			return cs[i].id < cs[j].id
		})
		out := make([]int64, 0, k)
		for i := 0; i < k && i < len(cs); i++ {
			out = append(out, cs[i].id)
		}
		return out
	}

	It("ε-pruning starvation widening equals one-shot batch prune (invariant 1)", func() {
		// Four well-separated 2-D clusters of 15 points each. A query at the
		// origin is near cluster 0; with a near-zero ε only cluster 0's lists
		// survive pruning, so the search MUST widen the ε-pruned tail (the other
		// three clusters) to fill the re-rank budget. That starvation widening
		// (the whole ε-pruned tail admitted in ONE parallel burst, d2 order) must
		// yield the IDENTICAL result to running with ε disabled, which probes the
		// whole routed set in one batch — the monotonic-equivalence proof obligation.
		centers := [][]float64{{0, 0}, {40, 40}, {-40, 35}, {38, -42}}
		var inputs []spfreshBuildInput
		id := int64(1)
		for _, c := range centers {
			for i := 0; i < 15; i++ {
				v := []float64{c[0] + float64(i)*0.05, c[1] + float64(i%5)*0.07}
				inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
				id++
			}
		}
		storage, config := buildIdx("widen-eq", 16, inputs, 2)

		query := []float64{0, 0}
		k := 12

		// Starvation-widen path: near-zero ε prunes the far clusters; the starved
		// probe set widens into them in one parallel burst (whole tail at once).
		tuneWiden := func(s *spfreshSearcher) {
			s.w = 8
			s.kc = 64
			s.epsilon = 1e-6
		}
		widenRes, widenTimer := runSearch(storage, config, query, k, tuneWiden)

		// Batch path: ε disabled probes the whole routed set up front (no tail,
		// no widening) — the one-shot reference.
		tuneBatch := func(s *spfreshSearcher) {
			s.w = 8
			s.kc = 64
			s.epsilon = 0
		}
		batchRes, batchTimer := runSearch(storage, config, query, k, tuneBatch)

		// The widening path must actually have pruned AND widened (non-vacuous).
		Expect(widenTimer.GetCount(CountSPFreshPostingsPruned)).To(BeNumerically(">", 0),
			"near-zero ε must prune the far clusters")
		Expect(widenTimer.GetCount(CountSPFreshStarvationWiden)).To(Equal(int64(1)),
			"the starved probe set must widen into the pruned tail exactly once")
		Expect(batchTimer.GetCount(CountSPFreshPostingsPruned)).To(Equal(int64(0)),
			"ε disabled must prune nothing")

		// Identical pk order AND distances: starvation widen ≡ batch prune.
		Expect(widenRes).To(HaveLen(len(batchRes)))
		Expect(pkIDs(widenRes)).To(Equal(pkIDs(batchRes)),
			"ε-pruning starvation widening must equal one-shot batch prune")
		for i := range widenRes {
			Expect(widenRes[i].Distance).To(Equal(batchRes[i].Distance))
		}
	})

	It("the resumable wrapper matches a brute-force golden across k (invariant 4)", func() {
		// Small deterministic index at clearly distinct distances; wide probe +
		// large re-rank budget make the ANN search exhaustive, so search() must
		// equal the exact k-nearest. Proves the refactored wrapper is the same
		// answer the one-shot path gave, for every k. Also drives the resumable
		// iterator one row at a time (searchNext demand=1) and asserts the
		// streamed sequence equals the wrapper.
		var inputs []spfreshBuildInput
		var ids []int64
		var vecs [][]float64
		for i := 0; i < 30; i++ {
			// Distinct radii along a ray + small lateral jitter → distinct
			// distances, deterministic order.
			r := 1.0 + float64(i)*0.9
			v := []float64{r, float64(i%3) * 0.13}
			id := int64(100 + i)
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
			ids = append(ids, id)
			vecs = append(vecs, v)
		}
		storage, config := buildIdx("golden", 16, inputs, 2)
		query := []float64{0, 0}

		tune := func(s *spfreshSearcher) {
			s.w = 64   // reach every coarse cell
			s.kc = 256 // fetch every fine posting
			s.c = 200  // re-rank budget ≫ corpus → exhaustive
		}

		for _, k := range []int{1, 3, 5, 10, 30} {
			res, _ := runSearch(storage, config, query, k, tune)
			want := bruteForce(ids, vecs, query, config.Metric, k)
			Expect(pkIDs(res)).To(Equal(want),
				"search(k=%d) must return the exact k nearest in distance order", k)
		}

		// Resumable parity: searchNext one row at a time == wrapper search(k).
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cache := newSPFreshRoutingCache(0)
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			tune(searcher)
			f, ferr := searcher.searchInit(tx, query)
			Expect(ferr).NotTo(HaveOccurred())
			var streamed []spfreshSearchResult
			for {
				batch, exhausted, nerr := searcher.searchNext(f, 1)
				Expect(nerr).NotTo(HaveOccurred())
				Expect(len(batch)).To(BeNumerically("<=", 1))
				streamed = append(streamed, batch...)
				if exhausted {
					break
				}
			}
			want := bruteForce(ids, vecs, query, config.Metric, 30)
			Expect(pkIDs(streamed)).To(Equal(want),
				"one-row-at-a-time searchNext must stream the full exact order")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("emits in exact distance order when RaBitQ estimate order disagrees (invariant 2)", func() {
		// A dense shell of points at near-equal distances in 6-D. With 1-bit
		// RaBitQ the residual ESTIMATE order disagrees with the exact order for
		// some pairs; the search must emit in EXACT (re-ranked) order, never the
		// pre-re-rank estimate order. We prove both halves: (a) the re-ranked
		// result equals the exact golden, and (b) the no-re-rank (estimate-only)
		// result actually differs from it — so the re-rank really did reorder.
		dims := 6
		var inputs []spfreshBuildInput
		var ids []int64
		var vecs [][]float64
		for i := 0; i < 24; i++ {
			v := make([]float64, dims)
			for d := 0; d < dims; d++ {
				// Base shell at radius ~3 with small per-point, per-dim wobble.
				v[d] = 3.0 + float64((i*7+d*13)%11)*0.02 + float64(d)*0.01
			}
			id := int64(200 + i)
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
			ids = append(ids, id)
			vecs = append(vecs, v)
		}
		storage, config := buildIdx("rerank-order", 16, inputs, dims)
		query := make([]float64, dims) // origin

		tune := func(s *spfreshSearcher) {
			s.w = 64
			s.kc = 256
			s.c = 200
		}
		k := 12

		// Re-ranked (exact) order.
		exactRes, _ := runSearch(storage, config, query, k, tune)
		want := bruteForce(ids, vecs, query, config.Metric, k)
		Expect(pkIDs(exactRes)).To(Equal(want),
			"re-ranked emission must be in exact distance order")
		// The reported distances must be non-decreasing (true distance order).
		for i := 1; i < len(exactRes); i++ {
			Expect(exactRes[i].Distance).To(BeNumerically(">=", exactRes[i-1].Distance))
		}

		// Estimate-only order (c=-1 disables the sidecar re-rank wave).
		estRes, _ := runSearch(storage, config, query, k, func(s *spfreshSearcher) {
			tune(s)
			s.noRerank = true
		})
		Expect(pkIDs(estRes)).NotTo(Equal(pkIDs(exactRes)),
			"test must be non-vacuous: RaBitQ estimate order should disagree with exact order")
	})
})
