package recordlayer

import (
	"math"
	"math/rand"
	"sort"
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
	many := []spfreshCandidate{{1, 1}, {2, 1.01}, {3, 1.02}, {4, 1.03}}
	if got = spfreshClosure(many, 2, 2.0); len(got) != 2 {
		t.Fatalf("r=2 must cap: got %+v", got)
	}

	if got = spfreshClosure(nil, 2, 1.2); got != nil {
		t.Fatal("empty candidates must return nil")
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
			if got[i] != ref[i] {
				t.Fatalf("k=%d: candidate %d = %+v, want %+v", k, i, got[i], ref[i])
			}
		}
	}
}
