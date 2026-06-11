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

func TestSPFreshBuildRouterAssignRNGPool(t *testing.T) {
	t.Parallel()
	// Three same-direction fines stacked beyond the nearest plus one diverse
	// fine. With rep=2 the copy-set must be {nearest, diverse}: a candidate
	// pool of exactly rep would only ever see the same-direction duplicate
	// and RNG-skip it, silently shrinking the copy-set to 1.
	r := &spfreshBuildRouter{
		ids:   []int64{1, 2, 3, 4},
		cells: []int64{10, 10, 10, 20},
		vecs:  [][]float64{{1, 0}, {1.2, 0}, {1.3, 0}, {-1.5, 0}},
	}
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
	// under-replicates to {nearest}; the widening loop must reach it
	// (codex 094.4 r2).
	r := &spfreshBuildRouter{}
	for i := 0; i < 17; i++ {
		r.ids = append(r.ids, int64(i+1))
		r.cells = append(r.cells, 10)
		r.vecs = append(r.vecs, []float64{1 + float64(i)*0.001, 0})
	}
	const diverse = int64(99)
	r.ids = append(r.ids, diverse)
	r.cells = append(r.cells, 20)
	r.vecs = append(r.vecs, []float64{-1.05, 0}) // d2 1.1025 <= 1.2²·1 = 1.44

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
	r := &spfreshBuildRouter{}
	for i := 0; i < 70; i++ {
		r.ids = append(r.ids, int64(i+1))
		r.cells = append(r.cells, 10)
		r.vecs = append(r.vecs, []float64{1 + float64(i)*0.0001, 0})
	}
	r.ids = append(r.ids, 999)
	r.cells = append(r.cells, 20)
	r.vecs = append(r.vecs, []float64{-1.05, 0}) // diverse, in-ratio, but past the cap
	ids, _ := r.assign([]float64{0, 0}, 2, 1.2)
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("bounded widening must stop at 4x base and accept under-replication: got %v", ids)
	}
}
