package recordlayer

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

func TestSPFreshKMeansDeterministic(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float64, 200)
	for i := range vecs {
		vecs[i] = []float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
	}
	c1, a1 := spfreshKMeans(vecs, 8, 7, 25)
	c2, a2 := spfreshKMeans(vecs, 8, 7, 25)
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("assignment %d differs across identical runs", i)
		}
	}
	for i := range c1 {
		for d := range c1[i] {
			if c1[i][d] != c2[i][d] {
				t.Fatalf("centroid %d differs across identical runs", i)
			}
		}
	}
	// A different seed is allowed to differ (sanity that the seed is used).
	c3, _ := spfreshKMeans(vecs, 8, 8, 25)
	same := true
	for i := range c1 {
		for d := range c1[i] {
			if c1[i][d] != c3[i][d] {
				same = false
			}
		}
	}
	if same {
		t.Fatal("different seeds produced identical centroids — seed unused?")
	}
}

func TestSPFreshKMeansSeparatesObviousClusters(t *testing.T) {
	t.Parallel()
	// Two tight, far-apart blobs: 2-means must split them exactly.
	rng := rand.New(rand.NewSource(1))
	var vecs [][]float64
	for i := 0; i < 50; i++ {
		vecs = append(vecs, []float64{rng.NormFloat64() * 0.1, 0})
	}
	for i := 0; i < 50; i++ {
		vecs = append(vecs, []float64{100 + rng.NormFloat64()*0.1, 0})
	}
	_, assign := spfreshKMeans(vecs, 2, 99, 25)
	for i := 1; i < 50; i++ {
		if assign[i] != assign[0] {
			t.Fatalf("blob A split: vec %d in cluster %d, vec 0 in %d", i, assign[i], assign[0])
		}
	}
	for i := 51; i < 100; i++ {
		if assign[i] != assign[50] {
			t.Fatalf("blob B split: vec %d in cluster %d, vec 50 in %d", i, assign[i], assign[50])
		}
	}
	if assign[0] == assign[50] {
		t.Fatal("both blobs in one cluster")
	}
}

func TestSPFreshKMeansEdgeCases(t *testing.T) {
	t.Parallel()
	// k > n clamps; identical points don't divide by zero or loop forever.
	vecs := [][]float64{{1, 1}, {1, 1}, {1, 1}}
	cents, assign := spfreshKMeans(vecs, 10, 5, 25)
	if len(cents) != 3 || len(assign) != 3 {
		t.Fatalf("clamp: got %d centroids, %d assigns", len(cents), len(assign))
	}
	// k=1 mean is the centroid.
	cents, _ = spfreshKMeans([][]float64{{0, 0}, {2, 0}, {4, 0}}, 1, 5, 25)
	if math.Abs(cents[0][0]-2) > 1e-12 || cents[0][1] != 0 {
		t.Fatalf("k=1 centroid = %v, want [2 0]", cents[0])
	}
	// Empty input.
	if c, a := spfreshKMeans(nil, 3, 5, 25); c != nil || a != nil {
		t.Fatal("empty input must return nil, nil")
	}
}

func TestSPFreshClosureRule(t *testing.T) {
	t.Parallel()
	// Candidates at squared distances 1, 1.21, 2.0 (true distances 1, 1.1, ~1.41).
	cands := []spfreshCandidate{{id: 1, d2: 1}, {id: 2, d2: 1.21}, {id: 3, d2: 2.0}}

	// alpha=1.2, r=3: threshold = 1.44 ⇒ admits c2 (1.21 ≤ 1.44), rejects c3.
	got := spfreshClosure(cands, 3, 1.2)
	if len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
		t.Fatalf("alpha=1.2 r=3: got %+v, want [1 2]", got)
	}

	// THE rev-3 RFC bug pinned: alpha=1.0 admits only the nearest even with
	// r=2 and a near-tie — exactly why config validation rejects alpha<=1.0
	// when replication > 1.
	got = spfreshClosure(cands, 2, 1.0)
	if len(got) != 1 || got[0].id != 1 {
		t.Fatalf("alpha=1.0 must degenerate to r=1: got %+v", got)
	}

	// Exact tie IS admitted at alpha=1.0 (<= rule).
	tie := []spfreshCandidate{{id: 1, d2: 1}, {id: 2, d2: 1}}
	if got = spfreshClosure(tie, 2, 1.0); len(got) != 2 {
		t.Fatalf("exact tie at alpha=1.0 must be admitted: got %+v", got)
	}

	// r caps admission regardless of alpha.
	many := []spfreshCandidate{{id: 1, d2: 1}, {id: 2, d2: 1.01}, {id: 3, d2: 1.02}, {id: 4, d2: 1.03}}
	if got = spfreshClosure(many, 2, 2.0); len(got) != 2 {
		t.Fatalf("r=2 must cap: got %+v", got)
	}

	if got = spfreshClosure(nil, 2, 1.2); got != nil {
		t.Fatal("empty candidates must return nil")
	}
}

func TestSPFreshClosureRNGRule(t *testing.T) {
	t.Parallel()
	// SPANN §3.2.2 Figure 5: x at the origin; blue is nearest; yellow sits
	// just past blue in the SAME direction (its posting list near-duplicates
	// blue's); grey is farther from x than yellow but in the OPPOSITE
	// direction. The RNG rule spends the replica on grey, not yellow.
	x := []float64{0, 0}
	mk := func(id int64, vec ...float64) spfreshCandidate {
		return spfreshCandidate{id: id, d2: spfreshSquaredDistance(x, vec), vec: vec}
	}
	blue := mk(1, 1, 0)     // d2 = 1
	yellow := mk(2, 1.3, 0) // d2 = 1.69; d2(blue, yellow) = 0.09 <= 1.69 → RNG-skipped
	grey := mk(3, -1.4, 0)  // d2 = 1.96; d2(blue, grey) = 5.76 > 1.96 → kept
	cands := []spfreshCandidate{blue, yellow, grey}

	got := spfreshClosure(cands, 3, 1.5) // ratio threshold 2.25 admits all three
	if len(got) != 2 || got[0].id != 1 || got[1].id != 3 {
		t.Fatalf("figure-5 closure: got %+v, want ids [1 3]", got)
	}

	// Lookahead past index r: with r=2 the old cands[1:r] scan would stop at
	// yellow and return blue alone; the RNG scan must reach grey.
	got = spfreshClosure(cands, 2, 1.5)
	if len(got) != 2 || got[1].id != 3 {
		t.Fatalf("RNG lookahead past r: got %+v, want ids [1 3]", got)
	}

	// r still caps when candidates are all diverse.
	diverse := []spfreshCandidate{mk(1, 1, 0), mk(2, 0, 1.05), mk(3, -1.1, 0)}
	if got = spfreshClosure(diverse, 2, 2.0); len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
		t.Fatalf("r cap over diverse candidates: got %+v, want ids [1 2]", got)
	}

	// Equality rejects (SPTAG's <= rule): the candidate is exactly as close
	// to the kept centroid as to the vector.
	eq := []spfreshCandidate{mk(1, 1, 0), mk(2, 0.5, 1)} // d2(x,c2) = d2(c1,c2) = 1.25
	if got = spfreshClosure(eq, 2, 2.0); len(got) != 1 || got[0].id != 1 {
		t.Fatalf("RNG equality must reject: got %+v, want ids [1]", got)
	}

	// The ratio bound still ends the scan before RNG is consulted.
	farDiverse := []spfreshCandidate{mk(1, 1, 0), mk(2, -3, 0)} // d2 = 9 > 1.44
	if got = spfreshClosure(farDiverse, 2, 1.2); len(got) != 1 {
		t.Fatalf("ratio bound must end scan: got %+v", got)
	}

	// A candidate without a centroid vector falls open to the ratio rule.
	mixed := []spfreshCandidate{mk(1, 1, 0), {id: 2, d2: 1.21}}
	if got = spfreshClosure(mixed, 2, 1.2); len(got) != 2 {
		t.Fatalf("nil-vec candidate must fall open: got %+v", got)
	}
}

// FuzzSPFreshClosure pins the closure invariants under arbitrary candidate
// geometry: the nearest candidate is always kept, the result is capped at r,
// every kept replica passes the ratio bound, kept replicas are pairwise
// RNG-diverse, and the selection is deterministic.
func FuzzSPFreshClosure(f *testing.F) {
	f.Add([]byte{16, 0, 240, 0, 0, 16, 200, 30}, uint8(2), uint8(2))
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, uint8(4), uint8(15))
	f.Fuzz(func(t *testing.T, raw []byte, rRaw, alphaRaw uint8) {
		r := int(rRaw)%8 + 1
		alpha := 1.0 + float64(alphaRaw)/64 // [1.0, ~5.0), finite
		var cands []spfreshCandidate
		for i := 0; i+1 < len(raw) && len(cands) < 32; i += 2 {
			vec := []float64{float64(int8(raw[i])) / 16, float64(int8(raw[i+1])) / 16}
			cands = append(cands, spfreshCandidate{
				id:  int64(len(cands)),
				d2:  spfreshSquaredDistance([]float64{0, 0}, vec),
				vec: vec,
			})
		}
		spfreshSortCandidates(cands)
		got := spfreshClosure(cands, r, alpha)
		if len(cands) == 0 {
			if got != nil {
				t.Fatalf("empty in, non-nil out: %+v", got)
			}
			return
		}
		if len(got) == 0 || got[0].id != cands[0].id {
			t.Fatalf("nearest candidate must always be kept: got %+v, cands %+v", got, cands)
		}
		if len(got) > r {
			t.Fatalf("r=%d exceeded: %d kept", r, len(got))
		}
		threshold := alpha * alpha * cands[0].d2
		for j, c := range got {
			if j == 0 {
				continue
			}
			if c.d2 > threshold {
				t.Fatalf("kept candidate %d violates ratio bound: d2=%g threshold=%g", c.id, c.d2, threshold)
			}
			if c.d2 < got[j-1].d2 {
				t.Fatalf("kept candidates must stay ascending: %+v", got)
			}
			for _, s := range got[:j] {
				if spfreshSquaredDistance(s.vec, c.vec) <= c.d2 {
					t.Fatalf("kept candidates %d and %d violate RNG diversity", s.id, c.id)
				}
			}
		}
		again := spfreshClosure(cands, r, alpha)
		if len(again) != len(got) {
			t.Fatalf("nondeterministic closure: %d vs %d kept", len(got), len(again))
		}
		for j := range got {
			if again[j].id != got[j].id {
				t.Fatalf("nondeterministic closure at %d: %v vs %v", j, again[j].id, got[j].id)
			}
		}
	})
}

// newTwoLevelTestRouter builds a two-level build router (RFC-099) from a flat
// (ids, cells, vecs) list. Each cell's coarse centroid is the mean of its
// fines, and w_b defaults to spfreshBuildAssignCells (48) — far more than the
// handful of cells these closure tests use, so EVERY fine is gathered and the
// closure sees the identical candidate set the old flat router gave it. These
// tests exercise the closure/RNG/widening logic, not the routing.
func newTwoLevelTestRouter(ids, cells []int64, vecs [][]float64) *spfreshBuildRouter {
	cellFineIDs := map[int64][]int64{}
	cellFineVecs := map[int64][][]float64{}
	sum := map[int64][]float64{}
	cnt := map[int64]int{}
	for i := range ids {
		c := cells[i]
		cellFineIDs[c] = append(cellFineIDs[c], ids[i])
		cellFineVecs[c] = append(cellFineVecs[c], vecs[i])
		if sum[c] == nil {
			sum[c] = make([]float64, len(vecs[i]))
		}
		for d, x := range vecs[i] {
			sum[c][d] += x
		}
		cnt[c]++
	}
	var coarseIDs []int64
	var coarseVecs [][]float64
	for c, s := range sum {
		mean := make([]float64, len(s))
		for d := range s {
			mean[d] = s[d] / float64(cnt[c])
		}
		coarseIDs = append(coarseIDs, c)
		coarseVecs = append(coarseVecs, mean)
	}
	r := &spfreshBuildRouter{
		coarseIDs:    coarseIDs,
		coarseVecs:   coarseVecs,
		cellFineIDs:  cellFineIDs,
		cellFineVecs: cellFineVecs,
		w:            spfreshDefaultBuildAssignCells,
	}
	r.precomputePrune()
	return r
}

func TestSPFreshBuildRouterAssignRNGPool(t *testing.T) {
	t.Parallel()
	// Three same-direction fines stacked beyond the nearest plus one diverse
	// fine. With rep=2 the copy-set must be {nearest, diverse}: a candidate
	// pool of exactly rep would only ever see the same-direction duplicate
	// and RNG-skip it, silently shrinking the copy-set to 1.
	r := newTwoLevelTestRouter(
		[]int64{1, 2, 3, 4},
		[]int64{10, 10, 10, 20},
		[][]float64{{1, 0}, {1.2, 0}, {1.3, 0}, {-1.5, 0}},
	)
	ids, fvecs := r.assign([]float64{0, 0}, 2, 2.0)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 4 {
		t.Fatalf("assign copy-set: got %v, want [1 4]", ids)
	}
	if len(fvecs) != 2 || fvecs[1][0] != -1.5 {
		t.Fatalf("assign fine vectors must track the kept candidates: got %v", fvecs)
	}
}

func TestSPFreshBuildRouterAssignWidensPastFixedPool(t *testing.T) {
	t.Parallel()
	// 17 same-direction near-duplicates fill the entire initial pool
	// (spfreshClosurePool(2) = 16) before the single diverse fine, which is
	// still within the alpha ratio. A fixed pool truncates ahead of it and
	// under-replicates to {nearest}; the widening loop must reach it.
	var ids0 []int64
	var cells0 []int64
	var vecs0 [][]float64
	for i := 0; i < 17; i++ {
		ids0 = append(ids0, int64(i+1))
		cells0 = append(cells0, 10)
		vecs0 = append(vecs0, []float64{1 + float64(i)*0.001, 0})
	}
	const diverse = int64(99)
	ids0 = append(ids0, diverse)
	cells0 = append(cells0, 20)
	vecs0 = append(vecs0, []float64{-1.05, 0}) // d2 1.1025 <= 1.2²·1 = 1.44
	r := newTwoLevelTestRouter(ids0, cells0, vecs0)

	ids, _ := r.assign([]float64{0, 0}, 2, 1.2)
	if len(ids) != 2 || ids[1] != diverse {
		t.Fatalf("widening must reach the diverse in-ratio candidate past the fixed pool: got %v", ids)
	}
}

func TestSPFreshNearestK(t *testing.T) {
	t.Parallel()
	ids := []int64{10, 20, 30}
	vecs := [][]float64{{3, 0}, {1, 0}, {2, 0}}
	got := spfreshNearestK([]float64{0, 0}, ids, vecs, 2)
	if len(got) != 2 || got[0].id != 20 || got[1].id != 30 {
		t.Fatalf("nearestK: got %+v", got)
	}
	// Deterministic tie-break by id.
	tie := spfreshNearestK([]float64{0, 0}, []int64{7, 3}, [][]float64{{1, 0}, {-1, 0}}, 2)
	if tie[0].id != 3 || tie[1].id != 7 {
		t.Fatalf("tie-break by id: got %+v", tie)
	}
}

// The parallel path (n spanning many chunks): determinism must hold through
// the chunk-merged float reductions — fixed chunk size, merge in chunk order,
// independent of worker count and scheduling.
func TestSPFreshKMeansDeterministicParallel(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	vecs := make([][]float64, 3*spfreshKMeansChunk+123) // >3 chunks, ragged tail
	for i := range vecs {
		vecs[i] = []float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
	}
	c1, a1 := spfreshKMeans(vecs, 32, 11, 25)
	c2, a2 := spfreshKMeans(vecs, 32, 11, 25)
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("assignment %d differs across identical parallel runs", i)
		}
	}
	for i := range c1 {
		for d := range c1[i] {
			if c1[i][d] != c2[i][d] {
				t.Fatalf("centroid %d dim %d differs across identical parallel runs", i, d)
			}
		}
	}
	// Every cluster non-degenerate and every point assigned in range.
	for i, a := range a1 {
		if a < 0 || a >= len(c1) {
			t.Fatalf("point %d assigned out of range: %d", i, a)
		}
	}
}

// spfreshNearestK is a top-k SELECTION (the full sort made the 1M build
// unbounded); its results must stay bit-identical to sort-then-truncate,
// ties and all.
func TestSPFreshNearestKMatchesFullSort(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(99))
	n := 5000
	ids := make([]int64, n)
	vecs := make([][]float64, n)
	for i := range ids {
		ids[i] = int64(i * 7)
		// Coarse grid => many exact distance TIES exercise the id tie-break.
		vecs[i] = []float64{float64(rng.Intn(8)), float64(rng.Intn(8))}
	}
	query := []float64{3.3, 4.1}
	for _, k := range []int{1, 2, 7, 64, 200, n, n + 10} {
		got := spfreshNearestK(query, ids, vecs, k)
		// Reference: full sort, truncate.
		ref := make([]spfreshCandidate, n)
		for i := range ids {
			ref[i] = spfreshCandidate{id: ids[i], d2: spfreshSquaredDistance(query, vecs[i])}
		}
		sort.Slice(ref, func(i, j int) bool {
			if ref[i].d2 != ref[j].d2 {
				return ref[i].d2 < ref[j].d2
			}
			return ref[i].id < ref[j].id
		})
		if k < len(ref) {
			ref = ref[:k]
		}
		if len(got) != len(ref) {
			t.Fatalf("k=%d: got %d candidates, want %d", k, len(got), len(ref))
		}
		for i := range ref {
			if got[i].id != ref[i].id || got[i].d2 != ref[i].d2 {
				t.Fatalf("k=%d: candidate %d = %+v, want %+v", k, i, got[i], ref[i])
			}
		}
	}
}

// errOnly adapts (T, error) returns for single-value Expect assertions.
func errOnly[T any](_ T, err error) error { return err }

// TestSPFreshKMeansWorkerCountInvariance pins the actual cross-machine
// determinism guarantee: at the FIXED chunk size, worker count must not change
// a single bit of the result (chunk boundaries fix the float-reduction order;
// workers only decide who computes each chunk). A same-process double-run
// can't catch a worker-count dependence — this comparison can.
func TestSPFreshKMeansWorkerCountInvariance(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(11))
	vecs := make([][]float64, 3*spfreshKMeansChunk/2+57) // multiple chunks, uneven tail
	for i := range vecs {
		vecs[i] = []float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
	}
	cSeq, aSeq := spfreshKMeansWorkers(vecs, 16, 5, 25, 1)
	cPar, aPar := spfreshKMeansWorkers(vecs, 16, 5, 25, 0)
	for i := range aSeq {
		if aSeq[i] != aPar[i] {
			t.Fatalf("assignment %d differs between workers=1 and parallel", i)
		}
	}
	for i := range cSeq {
		for d := range cSeq[i] {
			if cSeq[i][d] != cPar[i][d] {
				t.Fatalf("centroid %d dim %d differs between workers=1 and parallel: %v vs %v",
					i, d, cSeq[i][d], cPar[i][d])
			}
		}
	}
}

// TestSPFreshKMeansBuildConvergeFraction pins RFC-102: the build k-means
// convergence-fraction early-stop is (1) deterministic run-to-run, (2)
// GOMAXPROCS-invariant, (3) exact (fraction 0) for the split/csplit path — i.e.
// spfreshKMeans is unchanged — while (4) a non-zero fraction actually trims a
// long-tail (oscillating) high-k run, proving the parameter has effect.
func TestSPFreshKMeansBuildConvergeFraction(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(101))
	vecs := make([][]float64, 3*spfreshKMeansChunk/2+57)
	for i := range vecs {
		vecs[i] = []float64{rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
	}

	// (1) deterministic run-to-run.
	c1, a1 := spfreshKMeansBuild(vecs, 16, 5, 25)
	c2, a2 := spfreshKMeansBuild(vecs, 16, 5, 25)
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("build k-means assignment %d differs across identical runs", i)
		}
	}
	for i := range c1 {
		for d := range c1[i] {
			if c1[i][d] != c2[i][d] {
				t.Fatalf("build k-means centroid %d dim %d differs across identical runs", i, d)
			}
		}
	}

	// (2) GOMAXPROCS-invariant (workers=1 vs parallel) with the fraction.
	cSeq, aSeq := spfreshKMeansCore(vecs, 16, 5, 25, 1, spfreshKMeansBuildConvergeFraction)
	cPar, aPar := spfreshKMeansCore(vecs, 16, 5, 25, 0, spfreshKMeansBuildConvergeFraction)
	for i := range aSeq {
		if aSeq[i] != aPar[i] {
			t.Fatalf("build k-means assignment %d differs between workers=1 and parallel", i)
		}
	}
	for i := range cSeq {
		for d := range cSeq[i] {
			if cSeq[i][d] != cPar[i][d] {
				t.Fatalf("build k-means centroid %d dim %d differs between workers=1 and parallel", i, d)
			}
		}
	}

	// (3) fraction 0 == the exact split/csplit path: spfreshKMeans (which splits
	// call) must be byte-identical to spfreshKMeansCore(...,0). This is what keeps
	// the foreground rebalance clustering bit-identical (no recall A/B there).
	cEx, aEx := spfreshKMeans(vecs, 16, 5, 25)
	cZero, aZero := spfreshKMeansCore(vecs, 16, 5, 25, 0, 0)
	for i := range aEx {
		if aEx[i] != aZero[i] {
			t.Fatalf("spfreshKMeans (split path) diverged from fraction-0 core at %d", i)
		}
	}
	for i := range cEx {
		for d := range cEx[i] {
			if cEx[i][d] != cZero[i][d] {
				t.Fatalf("spfreshKMeans (split path) centroid %d dim %d diverged from fraction-0 core", i, d)
			}
		}
	}

	// (4) the fraction has effect: on this multi-chunk run a large fraction must
	// stop earlier and yield a different (still valid) clustering than exact.
	// (k-means with random data oscillates a small tail forever, so exact runs
	// the full 25 iters while a 10% fraction stops early.)
	_, aLoose := spfreshKMeansCore(vecs, 16, 5, 25, 0, 0.10)
	diff := false
	for i := range aEx {
		if aEx[i] != aLoose[i] {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatalf("convergeFraction=0.10 produced the SAME assignment as exact — the early-stop never engaged (regression is vacuous)")
	}

	// (5) the final-assignment pass is skipped on early-stop, so
	// assign MUST already be current: every point at its nearest centroid. This
	// pins that skipping the pass left no stale assignment.
	cB, aB := spfreshKMeansBuild(vecs, 16, 5, 25)
	for i := range aB {
		best, bestD := 0, math.Inf(1)
		for c := range cB {
			if d := spfreshSquaredDistance(vecs[i], cB[c]); d < bestD {
				best, bestD = c, d
			}
		}
		if aB[i] != best {
			t.Fatalf("point %d assigned to %d but nearest centroid is %d — final-pass skip left a stale assignment", i, aB[i], best)
		}
	}
}

// TestSPFreshBuilderFineIDPool pins the wave-A ID pool's doling: concurrent
// claims hand out disjoint consecutive ranges from the pre-claimed block
// without touching the allocator key, and an over-block request fails loudly
// instead of looping on refills.
func TestSPFreshBuilderFineIDPool(t *testing.T) {
	t.Parallel()
	b := &spfreshBuilder{idNext: 1000, idEnd: 1000 + spfreshIDBlockSize}
	const claimers, perClaim = 16, 5
	starts := make([]int64, claimers)
	var wg sync.WaitGroup
	for g := 0; g < claimers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			s, err := b.claimFineIDs(context.Background(), perClaim)
			if err != nil {
				t.Errorf("claim %d: %v", g, err)
				return
			}
			starts[g] = s
		}(g)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, s := range starts {
		for i := int64(0); i < perClaim; i++ {
			if seen[s+i] {
				t.Fatalf("ID %d doled twice", s+i)
			}
			seen[s+i] = true
		}
	}
	if len(seen) != claimers*perClaim {
		t.Fatalf("doled %d unique IDs, want %d", len(seen), claimers*perClaim)
	}

	if _, err := b.claimFineIDs(context.Background(), spfreshIDBlockSize+1); err == nil {
		t.Fatal("an over-block claim must error, not refill forever")
	}
}

func BenchmarkSPFreshSquaredDistance(b *testing.B) {
	rng := rand.New(rand.NewSource(3))
	x := make([]float64, 128)
	y := make([]float64, 128)
	for i := range x {
		x[i], y[i] = rng.NormFloat64(), rng.NormFloat64()
	}
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += spfreshSquaredDistance(x, y)
	}
	_ = sink
}

func TestSPFreshBuildRouterAssignWideningIsBounded(t *testing.T) {
	t.Parallel()
	// 70 same-direction near-duplicates inside the ratio bound, with the only
	// diverse candidate hidden beyond the 4×base widening cap (rep=2 → base
	// 16 → cap 64). The scan must STOP at the cap and accept the
	// under-replicated copy-set: unbounded "widen until the ratio break" was
	// quadratic at 1M density (the RNG rejects whole neighborhoods and the
	// pool doubled to the entire fine table per vector). The miss is NPA's
	// to repair, not the build's to hunt.
	var ids0 []int64
	var cells0 []int64
	var vecs0 [][]float64
	for i := 0; i < 70; i++ {
		ids0 = append(ids0, int64(i+1))
		cells0 = append(cells0, 10)
		vecs0 = append(vecs0, []float64{1 + float64(i)*0.0001, 0})
	}
	ids0 = append(ids0, 999)
	cells0 = append(cells0, 20)
	vecs0 = append(vecs0, []float64{-1.05, 0}) // diverse, in-ratio, but past the cap
	r := newTwoLevelTestRouter(ids0, cells0, vecs0)
	ids, _ := r.assign([]float64{0, 0}, 2, 1.2)
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("bounded widening must stop at 4x base and accept under-replication: got %v", ids)
	}
}

func TestSPFreshBuildRouterAssignWideningBoundary(t *testing.T) {
	t.Parallel()
	// The 4×base cap must actually be SCANNED, not just respected as an
	// upper bound: with the diverse in-ratio candidate at sorted index 40 —
	// inside (2·base, 4·base] for base 16 — only the third pool (64) reaches
	// it. A silent regression of the cap to 2×base would miss it and this
	// test, together with the >4×base negative, pins the boundary from both
	// sides.
	var ids0 []int64
	var cells0 []int64
	var vecs0 [][]float64
	for i := 0; i < 40; i++ {
		ids0 = append(ids0, int64(i+1))
		cells0 = append(cells0, 10)
		vecs0 = append(vecs0, []float64{1 + float64(i)*0.0001, 0})
	}
	const diverse = int64(999)
	ids0 = append(ids0, diverse)
	cells0 = append(cells0, 20)
	vecs0 = append(vecs0, []float64{-1.05, 0}) // sorted index 40, in-ratio at α=1.2
	r := newTwoLevelTestRouter(ids0, cells0, vecs0)

	ids, _ := r.assign([]float64{0, 0}, 2, 1.2)
	if len(ids) != 2 || ids[1] != diverse {
		t.Fatalf("the widening must scan through 4x base (pool 64) and find index-40 diverse candidate: got %v", ids)
	}
}

// TestSPFreshPruneLowerBoundIsConservative is the near-cancellation regression: the
// prune bound is dvc-dcf where dvc=d(v,c), dcf=d(c,f) are SEPARATELY-rounded sqrts
// of dims-term summed squared distances. When dvc≈dcf (a fine almost on the query
// but far from its cell centroid) the subtraction CANCELS catastrophically and
// the raw (dvc-dcf)² can exceed the true d(v,f)² by an amount no FIXED relative
// slack can bound (the relative overshoot → ∞ as d(v,f) → 0). spfreshPruneLowerBound
// subtracts a magnitude-scaled absolute error term, so for the prune to be EXACT
// its result must NEVER exceed the actual computed d(v,f). This asserts
// lb*lb <= d²(q,f) (whenever lb>0) across an adversarial sweep that DELIBERATELY
// drives the near-cancellation regime (fines fractionally off the query, large
// magnitude, up to 160-D), plus the exact originally-reported triple. A naive bound
// (dvc-dcf without the magnitude-scaled error term) fails this.
func TestSPFreshPruneLowerBoundIsConservative(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7777))

	check := func(q, c, f []float64, label string) {
		dims := len(q)
		d2qc := spfreshSquaredDistance(q, c)
		d2cf := spfreshSquaredDistance(c, f)
		d2qf := spfreshSquaredDistance(q, f)
		lb := spfreshPruneLowerBound(math.Sqrt(d2qc), math.Sqrt(d2cf), dims)
		if lb > 0 && lb*lb > d2qf {
			t.Fatalf("%s: prune lower bound NOT conservative: lb²=%.17g > d²(q,f)=%.17g (would wrong-skip)", label, lb*lb, d2qf)
		}
	}

	// The exact originally-reported case (1-D): q=3000, c=13000, f=3000.0001.
	check([]float64{3000}, []float64{13000}, []float64{3000.0001}, "codex-1d")

	naiveOvershoots := 0 // how often the NAIVE bound (no error term) would wrong-skip
	for trial := 0; trial < 400000; trial++ {
		// Sweep dims up to >128 (SIFT is 128-D) and magnitude up to ~1e9
		// (arbitrary float64 the index accepts; real SIFT is uint8 0..255, exact).
		dims := 1 + rng.Intn(160)
		dir := make([]float64, dims)
		var nn float64
		for d := range dir {
			dir[d] = rng.NormFloat64()
			nn += dir[d] * dir[d]
		}
		if nn == 0 {
			continue
		}
		nn = math.Sqrt(nn)
		mag := math.Pow(10, 3+rng.Float64()*6) // 1e3 .. 1e9
		q := make([]float64, dims)
		for d := range q {
			q[d] = rng.NormFloat64() * mag
		}
		scale := (rng.Float64()*99 + 1) * mag * 0.1
		// tf near 0 puts the fine fractionally off the query while the centroid
		// stays ~scale away — the near-cancellation regime.
		var tf float64
		if rng.Intn(2) == 0 {
			tf = math.Pow(10, -(rng.Float64() * 12)) // 1e-12 .. 1, biased tiny
		} else {
			tf = rng.Float64()
		}
		c := make([]float64, dims)
		f := make([]float64, dims)
		for d := range dir {
			u := dir[d] / nn
			c[d] = q[d] + u*scale
			f[d] = q[d] + u*scale*tf
		}
		check(q, c, f, "sweep")

		// Track how often the NAIVE bound would have wrong-skipped, to prove the
		// adversarial sweep actually exercises the hazard (non-vacuous regression).
		d2qc := spfreshSquaredDistance(q, c)
		d2cf := spfreshSquaredDistance(c, f)
		d2qf := spfreshSquaredDistance(q, f)
		if naive := math.Sqrt(d2qc) - math.Sqrt(d2cf); naive > 0 && naive*naive > d2qf {
			naiveOvershoots++
		}
	}
	t.Logf("naive (no error-term) bound would have overshot d²(q,f) in %d trials; spfreshPruneLowerBound never did", naiveOvershoots)
	if naiveOvershoots == 0 {
		t.Fatalf("sweep never drove the cancellation hazard — regression is vacuous; widen the search")
	}
}

// TestSPFreshGatherTopKExactSubnormal is the behavioral regression for the
// subnormal ulp hazard: when squared distances are subnormal (coords ~1e-160), the
// relative error term in spfreshPruneLowerBound underflows and the squaring
// lb*lb can round up by a subnormal ulp at an exact tie. gatherTopK gates the
// prune to the normal range (spfreshMinPrunableWorst), so subnormal inputs are
// scored exactly and stay byte-identical to the flat scan. Includes the exact
// originally-reported 1-D triple (which wrong-skips id 1 WITHOUT the gate) plus a
// subnormal-scale sweep.
func TestSPFreshGatherTopKExactSubnormal(t *testing.T) {
	t.Parallel()
	assertExact := func(t *testing.T, r *spfreshBuildRouter, q []float64, pool int, label string) {
		t.Helper()
		cellsK := spfreshNearestK(q, r.coarseIDs, r.coarseVecs, len(r.coarseIDs))
		got := r.gatherTopK(q, cellsK, pool)
		var fIDs []int64
		var fVecs [][]float64
		for _, c := range cellsK {
			fIDs = append(fIDs, r.cellFineIDs[c.id]...)
			fVecs = append(fVecs, r.cellFineVecs[c.id]...)
		}
		want := spfreshNearestK(q, fIDs, fVecs, pool)
		if len(got) != len(want) {
			t.Fatalf("%s: len got=%d want=%d", label, len(got), len(want))
		}
		for i := range got {
			if got[i].id != want[i].id || got[i].d2 != want[i].d2 {
				t.Fatalf("%s pos %d: got (id=%d d2=%g) want (id=%d d2=%g)", label, i, got[i].id, got[i].d2, want[i].id, want[i].d2)
			}
		}
	}

	// The exact originally-reported 1-D triple: centroid c far, query q, two fines that tie in
	// distance. id 2 is offered FIRST (becomes the pool worst), then id 1 — whose
	// ungated bound rounds up by a subnormal ulp past worst and is wrong-skipped,
	// even though id 1 should win the tie-break. The gate scores it exactly.
	cdx := &spfreshBuildRouter{
		coarseIDs:    []int64{1},
		coarseVecs:   [][]float64{{0x1.97f6271e113d5p-487}},
		cellFineIDs:  map[int64][]int64{1: {2, 1}},
		cellFineVecs: map[int64][][]float64{1: {{0x1.97f6270ddaa0dp-487}, {0x1.97f6271c84e43p-487}}},
		w:            1,
	}
	cdx.precomputePrune()
	for pool := 1; pool <= 2; pool++ {
		assertExact(t, cdx, []float64{0x1.97f627152fc28p-487}, pool, "codex-subnormal-1d")
	}

	// Subnormal-scale sweep: coords ~1e-160 so squared distances are subnormal.
	rng := rand.New(rand.NewSource(0x5AB))
	for trial := 0; trial < 4000; trial++ {
		dims := 1 + rng.Intn(6)
		nFines := 2 + rng.Intn(10)
		ids := make([]int64, nFines)
		cells := make([]int64, nFines)
		vecs := make([][]float64, nFines)
		base := make([]float64, dims)
		for d := range base {
			base[d] = rng.NormFloat64() * 0x1p-540 // sqrt → subnormal d²
		}
		for i := 0; i < nFines; i++ {
			ids[i] = int64(i + 1)
			cells[i] = 1
			v := make([]float64, dims)
			for d := range v {
				v[d] = base[d] + rng.NormFloat64()*0x1p-548
			}
			vecs[i] = v
		}
		r := newTwoLevelTestRouter(ids, cells, vecs)
		for sub := 0; sub < 3; sub++ {
			q := make([]float64, dims)
			for d := range q {
				q[d] = rng.NormFloat64() * 0x1p-540
			}
			assertExact(t, r, q, 1+rng.Intn(nFines), "subnormal-sweep")
		}
	}
}

// TestSPFreshGatherTopKExactLargeCoords is the behavioral large-magnitude guard:
// gatherTopK must stay byte-identical to the flat scan even when coordinates are
// large (~1e6) and fines cluster tightly near the pool boundary — the regime
// where the sqrt-rounded bound binds at the ulp scale (see
// TestSPFreshPruneLowerBoundIsConservative). Exercises gatherTopK end-to-end.
func TestSPFreshGatherTopKExactLargeCoords(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(909))
	for trial := 0; trial < 3000; trial++ {
		dims := 2 + rng.Intn(8)
		// One cell, many fines tightly clustered around a far, large center, so
		// their query-distances differ by ~the spread — comparable to the sqrt
		// roundoff at this magnitude, which is exactly where the bound can flip.
		base := (rng.Float64()*9 + 1) * 1e6 // 1e6..1e7
		spread := rng.Float64()*1e-2 + 1e-3
		nFines := 4 + rng.Intn(12)
		ids := make([]int64, nFines)
		cells := make([]int64, nFines)
		vecs := make([][]float64, nFines)
		center := make([]float64, dims)
		for d := range center {
			center[d] = base + rng.NormFloat64()*base*0.01
		}
		for i := 0; i < nFines; i++ {
			ids[i] = int64(i + 1)
			cells[i] = 1
			v := make([]float64, dims)
			for d := range v {
				v[d] = center[d] + rng.NormFloat64()*spread
			}
			vecs[i] = v
		}
		r := newTwoLevelTestRouter(ids, cells, vecs)
		for sub := 0; sub < 6; sub++ {
			q := make([]float64, dims)
			for d := range q {
				// Query far from the cluster, also large, so d(q,centroid) is
				// large and the sqrt roundoff is maximal.
				q[d] = rng.NormFloat64() * base
			}
			pool := 1 + rng.Intn(nFines)
			cellsK := spfreshNearestK(q, r.coarseIDs, r.coarseVecs, len(r.coarseIDs))
			got := r.gatherTopK(q, cellsK, pool)
			var fIDs []int64
			var fVecs [][]float64
			for _, c := range cellsK {
				fIDs = append(fIDs, r.cellFineIDs[c.id]...)
				fVecs = append(fVecs, r.cellFineVecs[c.id]...)
			}
			want := spfreshNearestK(q, fIDs, fVecs, pool)
			if len(got) != len(want) {
				t.Fatalf("trial %d/%d: len got=%d want=%d (pool=%d base=%g)", trial, sub, len(got), len(want), pool, base)
			}
			for i := range got {
				if got[i].id != want[i].id || got[i].d2 != want[i].d2 {
					t.Fatalf("trial %d/%d pos %d: got (id=%d d2=%.6f) want (id=%d d2=%.6f) pool=%d base=%g — ROUNDOFF SKIP",
						trial, sub, i, got[i].id, got[i].d2, want[i].id, want[i].d2, pool, base)
				}
			}
		}
	}
}

// randomTopology builds a random (ids, cell-assignment, vecs) topology for the
// RFC-101 exactness fuzz: nFines fines spread over up to nCells cells.
func randomTopology(rng *rand.Rand, dims, nFines, nCells int) (ids, cells []int64, vecs [][]float64) {
	ids = make([]int64, nFines)
	cells = make([]int64, nFines)
	vecs = make([][]float64, nFines)
	for i := 0; i < nFines; i++ {
		ids[i] = int64(i + 1)
		cells[i] = int64(rng.Intn(nCells) + 1)
		v := make([]float64, dims)
		for d := range v {
			v[d] = rng.NormFloat64() * 3
		}
		vecs[i] = v
	}
	return ids, cells, vecs
}

// TestSPFreshGatherTopKExactVsFlat is the load-bearing RFC-101 test: the
// bound-pruned two-level top-k must return a BYTE-IDENTICAL pool (same ids, same
// squared distances, same order) as a flat spfreshNearestK over the gathered
// cells' fines, for every topology / probe width / pool size. Pruning that ever
// changed the pool would change the closure copy-set and thus recall — this
// fails closed on any such divergence.
func TestSPFreshGatherTopKExactVsFlat(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(101))
	for trial := 0; trial < 400; trial++ {
		dims := 2 + rng.Intn(48)
		nFines := 1 + rng.Intn(400)
		nCells := 1 + rng.Intn(nFines)
		ids, cells, vecs := randomTopology(rng, dims, nFines, nCells)
		r := newTwoLevelTestRouter(ids, cells, vecs)
		for sub := 0; sub < 4; sub++ {
			q := make([]float64, dims)
			for d := range q {
				q[d] = rng.NormFloat64() * 3
			}
			w := 1 + rng.Intn(len(r.coarseIDs))
			pool := 1 + rng.Intn(2*nFines+1) // includes pool > total fines
			cellsK := spfreshNearestK(q, r.coarseIDs, r.coarseVecs, w)

			got := r.gatherTopK(q, cellsK, pool)

			// Flat reference: gather the same cells' fines, plain top-k.
			var fIDs []int64
			var fVecs [][]float64
			for _, c := range cellsK {
				fIDs = append(fIDs, r.cellFineIDs[c.id]...)
				fVecs = append(fVecs, r.cellFineVecs[c.id]...)
			}
			want := spfreshNearestK(q, fIDs, fVecs, pool)

			if len(got) != len(want) {
				t.Fatalf("trial %d/%d: len got=%d want=%d (w=%d pool=%d cells=%d fines=%d)",
					trial, sub, len(got), len(want), w, pool, len(cellsK), len(fIDs))
			}
			for i := range got {
				if got[i].id != want[i].id || got[i].d2 != want[i].d2 {
					t.Fatalf("trial %d/%d pos %d: got (id=%d d2=%g) want (id=%d d2=%g) w=%d pool=%d",
						trial, sub, i, got[i].id, got[i].d2, want[i].id, want[i].d2, w, pool)
				}
			}
		}
	}
}

// TestSPFreshAssignExactVsFlat exercises the full assign() (including the
// bounded-widening pool loop) against a flat reference that gathers all the
// w_b cells' fines and runs the identical closure/widening — proving the prune
// does not change the chosen copy-set or the widening termination.
func TestSPFreshAssignExactVsFlat(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(202))
	// flatAssign is the pre-RFC-101 assign: gather then spfreshNearestK.
	flatAssign := func(r *spfreshBuildRouter, vec []float64, rep int, alpha float64) ([]int64, [][]float64) {
		cells := spfreshNearestK(vec, r.coarseIDs, r.coarseVecs, r.w)
		var gIDs []int64
		var gVecs [][]float64
		for _, c := range cells {
			gIDs = append(gIDs, r.cellFineIDs[c.id]...)
			gVecs = append(gVecs, r.cellFineVecs[c.id]...)
		}
		base := spfreshClosurePool(rep)
		for pool := base; ; pool *= 2 {
			cands := spfreshNearestK(vec, gIDs, gVecs, pool)
			kept := spfreshClosure(cands, rep, alpha)
			if len(kept) >= rep || len(cands) < pool || pool >= 4*base ||
				(len(cands) > 0 && cands[len(cands)-1].d2 > alpha*alpha*cands[0].d2) {
				var ids []int64
				var fv [][]float64
				for _, c := range kept {
					ids = append(ids, c.id)
					fv = append(fv, c.vec)
				}
				return ids, fv
			}
		}
	}
	for trial := 0; trial < 300; trial++ {
		dims := 2 + rng.Intn(32)
		nFines := 1 + rng.Intn(300)
		nCells := 1 + rng.Intn(nFines)
		ids, cells, vecs := randomTopology(rng, dims, nFines, nCells)
		r := newTwoLevelTestRouter(ids, cells, vecs)
		r.w = 1 + rng.Intn(len(r.coarseIDs))
		for sub := 0; sub < 3; sub++ {
			q := make([]float64, dims)
			for d := range q {
				q[d] = rng.NormFloat64() * 3
			}
			rep := 1 + rng.Intn(4)
			alpha := 1.0 + rng.Float64()
			gotIDs, gotVecs := r.assign(q, rep, alpha)
			wantIDs, wantVecs := flatAssign(r, q, rep, alpha)
			if len(gotIDs) != len(wantIDs) {
				t.Fatalf("trial %d/%d: len ids got=%d want=%d (w=%d rep=%d alpha=%g)",
					trial, sub, len(gotIDs), len(wantIDs), r.w, rep, alpha)
			}
			for i := range gotIDs {
				if gotIDs[i] != wantIDs[i] {
					t.Fatalf("trial %d/%d pos %d: id got=%d want=%d", trial, sub, i, gotIDs[i], wantIDs[i])
				}
			}
			if len(gotVecs) != len(wantVecs) {
				t.Fatalf("trial %d/%d: len vecs got=%d want=%d", trial, sub, len(gotVecs), len(wantVecs))
			}
		}
	}
}
