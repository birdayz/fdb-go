package recordlayer

import (
	"context"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RFC-156 Phase C — the demand-driven, budget-bounded ordered-stream cursor.
//
// These specs drive the SPFresh streaming cursor (spfreshStreamCursor) directly,
// simulating the Filter→Limit operators above it, and pin the four guarantees the
// shift requires: (1) demand-driven BATCHED widening reaches matches beyond the
// fixed Phase-B horizon (the rare-predicate under-return is fixed); (2) honest
// truncation — a budget cap surfaces ScanLimitReached + telemetry, "only N match"
// surfaces SourceExhausted, two DISTINCT outcomes, never a silent < k;
// (3) bounded memory — the retained-candidate set stays O(budget), not O(corpus);
// (4) positional pagination is deterministic across transactions.
var _ = Describe("SPFresh ordered-stream cursor (RFC-156 Phase C)", func() {
	ctx := context.Background()

	// buildStreamIdx bulk-builds a SPFresh index over inputs (gen 1, generation
	// pointer set by the builder) and returns the base subspace + config + a stub
	// Index handle. dims-D, Euclidean, sidecar on — the same path the prune/Phase-A
	// specs use, no store/metadata needed.
	buildStreamIdx := func(name string, lmax, dims int, inputs []spfreshBuildInput, metric ...VectorMetric) (subspace.Subspace, SPFreshConfig, *Index) {
		cfg := DefaultSPFreshConfig(dims)
		cfg.Lmax = lmax
		if len(metric) > 0 {
			cfg.Metric = metric[0]
		}
		base := specSubspace().Sub(name)
		storage := newSPFreshStorage(base, 1)
		builder := newSPFreshBuilder(sharedDB, storage, cfg, "builder-"+name)
		Expect(builder.build(ctx, inputs, 7)).To(Succeed())
		idx := NewIndex(name, Concat(Field("a"), Field("b")))
		idx.Type = IndexTypeVectorSPFresh
		return base, cfg, idx
	}

	newMaintainer := func(base subspace.Subspace, cfg SPFreshConfig, idx *Index, rtx *FDBRecordContext, timer *StoreTimer) *spfreshIndexMaintainer {
		return &spfreshIndexMaintainer{
			standardIndexMaintainer: standardIndexMaintainer{index: idx, indexSubspace: base, tx: rtx.Transaction()},
			config:                  cfg,
			timer:                   timer,
		}
	}

	streamRange := func(query []float64) TupleRange {
		return TupleRange{Low: tuple.Tuple{SerializeVector(query)}}
	}

	type streamOut struct {
		ids       []int64
		reason    NoNextReason
		satisfied bool // consumer hit its limit (reason is then not meaningful)
		peak      int
		timer     *StoreTimer
	}

	// drive runs the streaming cursor in one transaction, applying an optional
	// residual filter and an optional limit (simulating Filter→Limit above the
	// scan): collect matching ids in emission order; stop at the limit (satisfied)
	// or at the cursor's terminal (reason recorded).
	drive := func(base subspace.Subspace, cfg SPFreshConfig, idx *Index, query []float64,
		budget spfreshStreamBudget, filter func(int64) bool, limit int,
		tweak ...func(*spfreshStreamCursor),
	) streamOut {
		out := streamOut{timer: NewStoreTimer()}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			m := newMaintainer(base, cfg, idx, rtx, out.timer)
			cur := m.newOrderedStreamCursor(streamRange(query), budget, nil, ScanProperties{})
			defer func() { _ = cur.Close() }()
			if sc, ok := cur.(*spfreshStreamCursor); ok {
				for _, tw := range tweak {
					tw(sc) // e.g. the near-side ablation sets sc.f.disableNearSide
				}
			}
			for {
				res, cerr := cur.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					out.reason = res.GetNoNextReason()
					break
				}
				id := res.GetValue().PrimaryKey()[0].(int64)
				if filter == nil || filter(id) {
					out.ids = append(out.ids, id)
					if limit > 0 && len(out.ids) >= limit {
						out.satisfied = true
						break
					}
				}
			}
			if sc, ok := cur.(*spfreshStreamCursor); ok && sc.f != nil {
				out.peak = sc.f.peakBest
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	// driveTopK drains the LEGACY materialized top-k path (ScanByDistance) and
	// returns the filtered ids — the Phase-B HEAD behaviour that under-returns when
	// the matches lie beyond the fixed horizon.
	driveTopK := func(base subspace.Subspace, cfg SPFreshConfig, idx *Index, query []float64, k int, filter func(int64) bool) []int64 {
		var ids []int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			m := newMaintainer(base, cfg, idx, rtx, nil)
			cur := m.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector(query)},
				High: tuple.Tuple{int64(k), int64(0), int64(0), int64(k)},
			}, nil, ScanProperties{})
			defer func() { _ = cur.Close() }()
			for {
				res, cerr := cur.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
				id := res.GetValue().PrimaryKey()[0].(int64)
				if filter == nil || filter(id) {
					ids = append(ids, id)
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return ids
	}

	// oneShotTopK runs the EXHAUSTIVE one-shot search (ScanByDistance with maxed
	// w/kc/c and ε disabled) — the same RaBitQ-estimate + fp16 sidecar re-rank path
	// the streaming cursor uses, materialized and sorted. This is the true oracle
	// the streaming output must equal (Graefe BLOCKER 3: compare to the one-shot,
	// not just brute-force, which the grid conflates).
	oneShotTopK := func(base subspace.Subspace, cfg SPFreshConfig, idx *Index, query []float64, k int) []int64 {
		var ids []int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			m := newMaintainer(base, cfg, idx, rtx, nil)
			cur := m.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector(query)},
				High: tuple.Tuple{int64(k), int64(4096), int64(4096), int64(4096), 0.0}, // ε=0 disables pruning
			}, nil, ScanProperties{})
			defer func() { _ = cur.Close() }()
			for {
				res, cerr := cur.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
				ids = append(ids, res.GetValue().PrimaryKey()[0].(int64))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return ids
	}

	// rareCorpus: 250 decoys tightly clustered at the origin (ids 1..250) + 12
	// matches in a FAR cluster (ids 1001..1012) at strictly increasing distance.
	// The far matches are pruned out of the initial probe — only demand-driven
	// widening reaches them.
	rareCorpus := func() []spfreshBuildInput {
		var inputs []spfreshBuildInput
		for i := 0; i < 250; i++ {
			v := []float64{float64(i%16) * 0.01, float64(i/16) * 0.01}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{int64(i + 1)}, vec: v})
		}
		for i := 1; i <= 12; i++ {
			v := []float64{50 + float64(i)*2, 0}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{int64(1000 + i)}, vec: v})
		}
		return inputs
	}
	isMatch := func(id int64) bool { return id >= 1000 }

	// bruteForce sorts ids by fp16-roundtripped exact distance (the same precision
	// the sidecar re-rank reads), tie-break by id — the one-shot/exact oracle.
	// Returns the sorted ids and the id→distance lookup.
	bruteForce := func(ids []int64, vecs [][]float64, query []float64, metric VectorMetric) ([]int64, map[int64]float64) {
		dist := make(map[int64]float64, len(ids))
		for i, v := range vecs {
			rtv, rerr := vectorcodecRoundtrip(v)
			Expect(rerr).NotTo(HaveOccurred())
			dist[ids[i]] = vectorDistance(query, rtv, metric)
		}
		sorted := append([]int64(nil), ids...)
		sort.Slice(sorted, func(i, j int) bool {
			if dist[sorted[i]] != dist[sorted[j]] {
				return dist[sorted[i]] < dist[sorted[j]]
			}
			return sorted[i] < sorted[j]
		})
		return sorted, dist
	}

	It("emits in exact one-shot order across widen batches — no recall drift (oracle parity)", func() {
		// Scattered 8×8 grid: kmeans cells tile the grid and OVERLAP in
		// distance-to-query, so cells admitted in centroid-d2 order interleave in
		// EXACT distance across widen batches. The relaxed-monotonicity emission
		// barrier must hold each candidate until no un-admitted cell can beat it, so
		// the streamed order is IDENTICAL to the one-shot's whole-horizon finalize
		// (= brute-force sort over the scanned set). Without the barrier a later
		// batch's closer survivor would emit AFTER an already-emitted farther one →
		// the wrong k-set under Limit(k) (the Graefe/spfresh blocker).
		var inputs []spfreshBuildInput
		var ids []int64
		var vecs [][]float64
		id := int64(1)
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				v := []float64{float64(i), float64(j)}
				inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: v})
				ids = append(ids, id)
				vecs = append(vecs, v)
				id++
			}
		}
		base, cfg, idx := buildStreamIdx("oracle", 8, 2, inputs)
		// Irrational-ish corner offset → all 64 distances are DISTINCT (no ties to
		// confound the pk-order comparison).
		query := []float64{-0.41, -0.73}

		oracle, dist := bruteForce(ids, vecs, query, cfg.Metric)
		// Guard: the oracle order is unambiguous (strictly increasing distances).
		for i := 1; i < len(oracle); i++ {
			Expect(dist[oracle[i]]).To(BeNumerically(">", dist[oracle[i-1]]),
				"test precondition: distances must be distinct (no ties)")
		}

		// Streaming, exhaustive (generous budget, no filter): collect every emitted
		// id in emission order.
		out := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), nil, 0)
		Expect(out.reason).To(Equal(SourceExhausted), "the grid exhausts within budget")
		Expect(out.timer.GetCount(CountSPFreshStreamWiden)).To(BeNumerically(">", 0),
			"the grid spans multiple widen batches — the cross-batch emission path is exercised")

		// IDENTICAL to the oracle: same pk order …
		Expect(out.ids).To(Equal(oracle),
			"streamed order == one-shot/brute-force exact order over the scanned set (no recall drift)")
		// … and emission is distance-monotone (the barrier property, directly).
		for i := 1; i < len(out.ids); i++ {
			Expect(dist[out.ids[i]]).To(BeNumerically(">=", dist[out.ids[i-1]]),
				"emission must be distance-monotone — the barrier never lets a later widen "+
					"surface a closer-than-already-emitted survivor")
		}
		// … and every Limit(k) prefix is the true k nearest.
		for _, k := range []int{1, 3, 8, 20} {
			Expect(out.ids[:k]).To(Equal(oracle[:k]),
				"the first %d streamed rows are the true %d nearest", k, k)
		}
	})

	It("near-side correction (−maxResidual) is load-bearing — pure-centroid barrier breaks (ablation)", func() {
		// 8×8 grid: kmeans cells inherently hold NEAR-SIDE members (closer to the
		// query than their own centroid → a far-centroid cell with a near-query,
		// high-residual member; closure replication scores it early). The full
		// barrier B = nextCentroidDist − maxResidual corrects for them; the
		// PURE-CENTROID barrier (maxResidual≡0) does not — it emits a farther member
		// before a closer near-side one. The comparison oracle is the EXHAUSTIVE
		// ONE-SHOT (same RaBitQ-estimate + fp16 sidecar re-rank path), not just
		// brute-force (Graefe BLOCKER 3 — the grid conflates the two).
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i), float64(j)}})
				id++
			}
		}
		base, cfg, idx := buildStreamIdx("ablation", 8, 2, inputs)
		query := []float64{-0.41, -0.73}
		oneShot := oneShotTopK(base, cfg, idx, query, 64)
		Expect(oneShot).To(HaveLen(64))

		// GREEN: the full barrier streams EXACTLY the exhaustive one-shot order.
		full := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), nil, 0)
		Expect(full.ids).To(Equal(oneShot),
			"full barrier: streaming emission == exhaustive one-shot (no recall drift)")

		// RED→ablation: maxResidual forced to 0 (pure-centroid barrier) — the
		// emission ORDER demonstrably breaks vs the one-shot, proving the
		// −maxResidual near-side term is load-bearing (not cosmetic).
		ablated := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), nil, 0,
			func(sc *spfreshStreamCursor) { sc.f.disableNearSide = true })
		Expect(ablated.ids).NotTo(Equal(oneShot),
			"pure-centroid barrier (maxResidual≡0) emits a near-side member out of order — "+
				"the −maxResidual correction is load-bearing")
		// Same multiset — only the ORDER drifts, no row is lost.
		Expect(ablated.ids).To(ConsistOf(oneShot))
	})

	It("non-Euclidean metric streams correctly via materialize-then-emit (metric guard)", func() {
		// BLOCKER 4: for cosine the residual-margin (reverse-triangle) barrier is
		// unsound, so the cursor DISABLES it and falls back to materialize-then-emit
		// (hold every candidate, flush the full sorted scanned set at the terminal)
		// — exact for any metric. Assert the cosine streaming top-k == the exhaustive
		// one-shot. (Same construction would otherwise risk the unsound subtraction.)
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i) + 1, float64(j)*0.5 + 1}})
				id++
			}
		}
		base, cfg, idx := buildStreamIdx("noneuclid", 8, 2, inputs, VectorMetricCosine)
		Expect(cfg.Metric).To(Equal(VectorMetricCosine))
		query := []float64{0.3, 1.0}

		oneShot := oneShotTopK(base, cfg, idx, query, 64)
		out := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), nil, 0)
		Expect(out.reason).To(Equal(SourceExhausted),
			"the cosine index exhausts within budget (materialize-then-emit)")
		Expect(out.ids).To(Equal(oneShot),
			"cosine streaming (barrier disabled, materialize-then-emit) == exhaustive one-shot — exact for a non-L2 metric")
	})

	It("non-Euclidean inner-product (dot) metric streams correctly via materialize-then-emit (metric guard)", func() {
		// Companion to the cosine spec above, on the OTHER non-metric: unnormalized
		// inner-product (−dot). Like cosine it fails the triangle inequality AND is
		// not preserved under translation, so the residual-margin barrier is unsound
		// — streamInit DISABLES it (barrierEnabled is true ONLY for EUCLIDEAN) and
		// the cursor falls back to materialize-then-emit. For inner product the
		// d2-admission order can even DISAGREE with the metric order (magnitude
		// matters), so this is the metric whose early-streaming barrier would be most
		// wrong — proving the guard, the streamed top-k must still equal the
		// exhaustive one-shot. Magnitudes VARY across the grid (j scales the second
		// coordinate) so −dot is not a monotone function of either axis alone; the
		// query has a non-trivial best-dot ordering.
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i) + 1, float64(j)*0.5 + 1}})
				id++
			}
		}
		base, cfg, idx := buildStreamIdx("noneuclid-ip", 8, 2, inputs, VectorMetricInnerProduct)
		Expect(cfg.Metric).To(Equal(VectorMetricInnerProduct))
		query := []float64{0.3, 1.0}

		oneShot := oneShotTopK(base, cfg, idx, query, 64)
		Expect(oneShot).To(HaveLen(64))
		out := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), nil, 0)
		Expect(out.reason).To(Equal(SourceExhausted),
			"the inner-product index exhausts within budget (materialize-then-emit)")
		Expect(out.timer.GetCount(CountSPFreshStreamWiden)).To(BeNumerically(">", 0),
			"the grid spans multiple widen batches — the cross-batch materialize-then-emit path is exercised")
		Expect(out.ids).To(Equal(oneShot),
			"inner-product streaming (barrier disabled, materialize-then-emit) == exhaustive one-shot — exact for the −dot non-metric")
		// Direct emission-order check against the fp16-roundtripped −dot oracle: the
		// flushed order is metric-monotone (it equals a sort over the scanned set).
		var ids []int64
		var vecs [][]float64
		for _, in := range inputs {
			ids = append(ids, in.pk[0].(int64))
			vecs = append(vecs, in.vec)
		}
		ipOracle, ipDist := bruteForce(ids, vecs, query, cfg.Metric)
		Expect(out.ids).To(Equal(ipOracle),
			"inner-product streamed order == fp16 −dot brute-force order over the full scanned set")
		for i := 1; i < len(out.ids); i++ {
			Expect(ipDist[out.ids[i]]).To(BeNumerically(">=", ipDist[out.ids[i-1]]),
				"materialize-then-emit flushes in metric (−dot) order — no out-of-order emission")
		}
	})

	It("widens beyond the fixed horizon to return the true k nearest MATCHING rows (rare predicate)", func() {
		base, cfg, idx := buildStreamIdx("rare-widen", 16, 2, rareCorpus())
		query := []float64{0, 0}

		// Phase-B HEAD: the materialized top-200 path returns ONLY the near decoys
		// (the 200 globally-nearest), so the residual culls them ALL → under-return.
		oldMatches := driveTopK(base, cfg, idx, query, 200, isMatch)
		Expect(oldMatches).To(BeEmpty(),
			"Phase-B HEAD: matches lie beyond the fixed-200 horizon → the residual under-returns to ∅")

		// Phase C: the streaming cursor widens (admits the pruned far cells) and the
		// simulated Filter→Limit(3) collects the true 3 nearest MATCHING rows.
		out := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), isMatch, 3)
		Expect(out.satisfied).To(BeTrue(), "the 3 nearest matches must be reachable within budget")
		Expect(out.ids).To(Equal([]int64{1001, 1002, 1003}),
			"Phase C returns the true 3 nearest CATEGORY-match rows in distance order")
		Expect(out.timer.GetCount(CountSPFreshStreamWiden)).To(BeNumerically(">", 0),
			"reaching the matches REQUIRED batched demand-driven widening")
	})

	It("distinguishes budget truncation (ScanLimitReached) from exhaustion (SourceExhausted), never a silent < k", func() {
		base, cfg, idx := buildStreamIdx("honest-trunc", 16, 2, rareCorpus())
		query := []float64{0, 0}

		// (a) Budget cap hit before k: a tight candidate budget bounds the scan to
		// the near decoys; the far matches are never reached → ScanLimitReached +
		// the CountSPFreshFilteredTruncated telemetry, NOT a silent < k.
		tiny := spfreshStreamBudget{maxCells: 512, maxCandidates: 50, widenBatch: 8}
		a := drive(base, cfg, idx, query, tiny, isMatch, 3)
		Expect(a.satisfied).To(BeFalse())
		Expect(a.ids).To(BeEmpty(), "no match is reachable within the tight budget")
		Expect(a.reason).To(Equal(ScanLimitReached),
			"budget hit before k → ScanLimitReached (resumable), NOT SourceExhausted")
		Expect(a.timer.GetCount(CountSPFreshFilteredTruncated)).To(Equal(int64(1)),
			"the truncation telemetry fires IN ADDITION to the reason")

		// (b) Only N < k rows match within an exhausted index: a generous budget
		// reaches both matching rows, the index exhausts → SourceExhausted + exactly
		// those N, in distance order. A DISTINCT outcome from (a).
		onlyTwo := func(id int64) bool { return id == 1001 || id == 1003 }
		b := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), onlyTwo, 5)
		Expect(b.satisfied).To(BeFalse(), "k=5 is never reached: only 2 rows match")
		Expect(b.reason).To(Equal(SourceExhausted),
			"only N match within the exhausted index → SourceExhausted (terminal)")
		Expect(b.ids).To(Equal([]int64{1001, 1003}),
			"exactly the N matching rows, in distance order")
		Expect(b.timer.GetCount(CountSPFreshFilteredTruncated)).To(Equal(int64(0)),
			"exhaustion is not a truncation — the metric must NOT fire")
	})

	It("emits matching probe rows even when the probe alone exceeds the budget (no premature truncation)", func() {
		// Regression: streamInit can pre-set budgetHit when the dense probe already
		// exceeds the candidate budget. The cursor MUST still emit the probe's
		// finalized prefix before truncating — otherwise a matching probe row is
		// lost to a premature ScanLimitReached. Here the filter matches the 5
		// NEAREST rows and the budget is below the probe size; the 2 nearest matches
		// must come out (satisfied), never a silent < k.
		base, cfg, idx := buildStreamIdx("probe-match", 16, 2, rareCorpus())
		query := []float64{0, 0}
		tiny := spfreshStreamBudget{maxCells: 512, maxCandidates: 3, widenBatch: 8}
		out := drive(base, cfg, idx, query, tiny, func(id int64) bool { return id <= 5 }, 2)
		Expect(out.satisfied).To(BeTrue(),
			"the 2 nearest matching rows live in the probe and MUST be emitted despite the tiny budget")
		Expect(out.ids).To(Equal([]int64{1, 2}),
			"the probe's matching rows are emitted in distance order before any truncation")
	})

	It("retains O(budget) candidates, never O(corpus) (memory bound)", func() {
		// Same SMALL candidate budget over two corpus sizes that differ 4×. A
		// pathological filter matches nothing, so the cursor widens to the budget
		// cap. The peak retained-candidate count (the dedup-map high-water) must
		// stay bounded by the budget and NOT scale with the corpus.
		budget := spfreshStreamBudget{maxCells: 4096, maxCandidates: 200, widenBatch: 8}
		none := func(int64) bool { return false }
		query := []float64{0, 0}

		corpus := func(n int) []spfreshBuildInput {
			in := make([]spfreshBuildInput, 0, n)
			for i := 0; i < n; i++ {
				// Spread across a wide grid so widening keeps admitting new cells.
				v := []float64{float64(i%64) * 0.5, float64(i/64) * 0.5}
				in = append(in, spfreshBuildInput{pk: tuple.Tuple{int64(i + 1)}, vec: v})
			}
			return in
		}

		baseS, cfgS, idxS := buildStreamIdx("mem-small", 16, 2, corpus(300))
		small := drive(baseS, cfgS, idxS, query, budget, none, 1)

		baseL, cfgL, idxL := buildStreamIdx("mem-large", 16, 2, corpus(1200))
		large := drive(baseL, cfgL, idxL, query, budget, none, 1)

		// Both exhaust the budget before satisfying the (never-matching) consumer.
		Expect(small.reason).To(Equal(ScanLimitReached))
		Expect(large.reason).To(Equal(ScanLimitReached))

		// The retained set stays O(budget): the candidate cap (200) plus at most one
		// widen-batch of postings of overshoot (8 cells × ≤4·Lmax+1 entries). It is
		// bounded by a CONSTANT independent of the corpus.
		const bound = 200 + 8*(4*16+1) // = 720
		Expect(small.peak).To(BeNumerically("<=", bound))
		Expect(large.peak).To(BeNumerically("<=", bound))
		// The proof of O(budget) NOT O(corpus): a 4× larger corpus does NOT grow the
		// peak (it stays within the same constant bound, nowhere near 1200).
		Expect(large.peak).To(BeNumerically("<", 1200),
			"peak retained candidates must not grow with the corpus")
		Expect(large.peak-small.peak).To(BeNumerically("<=", 4*16+1),
			"peak is corpus-independent (within one posting's worth of jitter)")
	})

	It("paginates deterministically: single-row pages across transactions == the unpaged stream", func() {
		// Small index so the full ordered stream exhausts within budget. The
		// continuation is POSITIONAL; each page re-runs the search in a NEW
		// transaction and skips to its offset (Java's ListCursor semantics).
		var inputs []spfreshBuildInput
		for i := 0; i < 24; i++ {
			v := []float64{1 + float64(i)*0.7, float64(i%5) * 0.11}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{int64(300 + i)}, vec: v})
		}
		base, cfg, idx := buildStreamIdx("paginate", 16, 2, inputs)
		query := []float64{0, 0}
		budget := defaultSPFreshStreamBudget()

		unpaged := drive(base, cfg, idx, query, budget, nil, 0).ids
		Expect(unpaged).NotTo(BeEmpty())

		pageAll := func() []int64 {
			var ids []int64
			var cont []byte
			for {
				var pageID int64
				var pageCont []byte
				got := false
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					m := newMaintainer(base, cfg, idx, rtx, nil)
					cur := m.newOrderedStreamCursor(streamRange(query), budget, cont, ScanProperties{})
					defer func() { _ = cur.Close() }()
					res, cerr := cur.OnNext(ctx)
					if cerr != nil {
						return nil, cerr
					}
					if !res.HasNext() {
						return nil, nil
					}
					got = true
					pageID = res.GetValue().PrimaryKey()[0].(int64)
					cb, berr := res.GetContinuation().ToBytes()
					if berr != nil {
						return nil, berr
					}
					pageCont = cb
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				if !got {
					break
				}
				ids = append(ids, pageID)
				cont = pageCont
			}
			return ids
		}

		// 10× for planner/cursor stability: every paged run must equal the unpaged
		// result exactly (positional re-run, no drift).
		for rep := 0; rep < 10; rep++ {
			Expect(pageAll()).To(Equal(unpaged),
				"page-by-page (returned-row-limit 1) must equal the unpaged stream (rep %d)", rep)
		}
	})

	It("propagates the budget-truncation reason through Filter and Limit (never swallowed to SourceExhausted)", func() {
		// The honest-truncation contract only holds end-to-end if the inner
		// ScanLimitReached survives the operators above. Wrap the streaming cursor
		// in the SAME generic filterCursor + LimitRowsCursor the executor stacks
		// (executeFilter's applySkipLimit) and assert the drained terminal reason.
		base, cfg, idx := buildStreamIdx("propagate", 16, 2, rareCorpus())
		query := []float64{0, 0}

		drainReason := func(budget spfreshStreamBudget, filter func(int64) bool, k int) (NoNextReason, int) {
			var reason NoNextReason
			matched := 0
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				m := newMaintainer(base, cfg, idx, rtx, NewStoreTimer())
				scan := m.newOrderedStreamCursor(streamRange(query), budget, nil, ScanProperties{})
				filtered := &filterCursor[*IndexEntry]{
					inner:     scan,
					predicate: func(e *IndexEntry) bool { return filter(e.PrimaryKey()[0].(int64)) },
				}
				limited := LimitRowsCursor[*IndexEntry](filtered, k)
				defer func() { _ = limited.Close() }()
				for {
					res, cerr := limited.OnNext(ctx)
					if cerr != nil {
						return nil, cerr
					}
					if !res.HasNext() {
						reason = res.GetNoNextReason()
						break
					}
					matched++
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return reason, matched
		}

		// Budget hit before k matches → the inner ScanLimitReached survives both
		// operators (Limit's remaining>0, so it forwards the inner out-of-band stop).
		tiny := spfreshStreamBudget{maxCells: 512, maxCandidates: 50, widenBatch: 8}
		reason, matched := drainReason(tiny, isMatch, 3)
		Expect(matched).To(Equal(0))
		Expect(reason).To(Equal(ScanLimitReached),
			"ScanLimitReached must propagate through Filter→Limit, not be swallowed")

		// Exhaustion below k → SourceExhausted survives unchanged (the DISTINCT
		// outcome).
		onlyTwo := func(id int64) bool { return id == 1001 || id == 1003 }
		reason, matched = drainReason(defaultSPFreshStreamBudget(), onlyTwo, 5)
		Expect(matched).To(Equal(2))
		Expect(reason).To(Equal(SourceExhausted))
	})

	It("stays within the FDB 5s transaction under a worst-case selective filter (budget calibration)", func() {
		// A large index + a never-matching residual is the worst case: the cursor
		// widens to the budget cap. With the calibrated DEFAULT budget it must
		// return ScanLimitReached well within the 5s tx — never a tx timeout.
		var inputs []spfreshBuildInput
		const n = 4500
		for i := 0; i < n; i++ {
			v := []float64{float64(i%96) * 0.4, float64(i/96) * 0.4}
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{int64(i + 1)}, vec: v})
		}
		base, cfg, idx := buildStreamIdx("stress-5s", 16, 2, inputs)
		query := []float64{0, 0}

		start := time.Now()
		out := drive(base, cfg, idx, query, defaultSPFreshStreamBudget(), func(int64) bool { return false }, 1)
		elapsed := time.Since(start)

		Expect(out.reason).To(Equal(ScanLimitReached),
			"the default budget must BIND on a corpus larger than the candidate cap")
		Expect(out.timer.GetCount(CountSPFreshFilteredTruncated)).To(Equal(int64(1)))
		Expect(elapsed).To(BeNumerically("<", 4500*time.Millisecond),
			"budget-bounded search+materialize must complete within the FDB 5s tx (no timeout)")
	})
})
