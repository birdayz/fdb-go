package vectorindex

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"

	"fdb.dev/pkg/rabitq"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

func scalarKMeansL2(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		d := a[i] - b[i]
		s += float64(d * d)
	}
	return s
}

func sameKMeansFloat(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b) || math.IsNaN(a) && math.IsNaN(b)
}

func TestKMeansSequentialKernelsPreserveFloatOrder(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewSource(819))
	for _, n := range []int{0, 1, 3, 4, 7, 8, 9, 15, 16, 17, 31, 32, 33, 97, 257, 2048} {
		a, b := make([]float64, n), make([]float64, n)
		for i := range a {
			a[i] = math.Ldexp(random.Float64()-0.5, random.Intn(1000)-500)
			b[i] = math.Ldexp(random.Float64()-0.5, random.Intn(1000)-500)
		}
		for pass := 0; pass < 6; pass++ {
			want := scalarKMeansL2(a, b)
			if got := l2SquaredSequential(a, b); !sameKMeansFloat(got, want) {
				t.Fatalf("n=%d pass=%d: squared distance bits %x, want %x", n, pass, math.Float64bits(got), math.Float64bits(want))
			}
			wantSum, gotSum := slices.Clone(a), slices.Clone(a)
			for i := range b {
				wantSum[i] += b[i]
			}
			addInto(gotSum, b)
			for i := range wantSum {
				if !sameKMeansFloat(gotSum[i], wantSum[i]) {
					t.Fatalf("n=%d pass=%d: sum[%d]=%x, want %x", n, pass, i, math.Float64bits(gotSum[i]), math.Float64bits(wantSum[i]))
				}
			}
			if n > 0 && pass < 5 {
				b[n-1] = []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1), math.SmallestNonzeroFloat64}[pass]
			}
		}
	}
	// Reassociation could preserve four unit terms instead of rounding each away.
	a := []float64{1 << 27, 1, 1, 1, 1, 0, 0, 0}
	if got := l2SquaredSequential(a, make([]float64, len(a))); math.Float64bits(got) != math.Float64bits(float64(1<<54)) {
		t.Fatalf("reassociated the sequential squared-distance sum: %.17g", got)
	}
}

func TestKMeansSequentialKernelsRoundProductsBeforeAddition(t *testing.T) {
	t.Parallel()
	small := math.Ldexp(1, -27)
	a := []float64{small, small, 1 + small, 0, 0}
	zero := make([]float64, len(a))
	// An FMA retains the product's discarded 2^-54 and rounds the final sum up.
	want := math.Float64bits(1 + math.Ldexp(1, -26))
	for name, got := range map[string]float64{
		"l2":      l2SquaredSequential(a, zero),
		"bounded": l2SquaredUntil(a, zero, math.Inf(1)),
		"dot":     dotSequential(a, a),
	} {
		if math.Float64bits(got) != want {
			t.Errorf("%s fused multiplication and addition: bits=%x, want %x", name, math.Float64bits(got), want)
		}
	}
}

func TestKMeansBoundedDistanceOnlyTruncatesLosers(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 3, 4, 7, 8, 9, 16, 17, 31, 32, 33} {
		a, b := make([]float64, n), make([]float64, n)
		for i := range b {
			b[i] = float64(i%5) * 0.1
		}
		full := scalarKMeansL2(a, b)
		for _, bound := range []float64{0, full / 2, full, math.Nextafter(full, math.Inf(1)), math.Inf(1), math.NaN()} {
			got := l2SquaredUntil(a, b, bound)
			if (got < bound) != (full < bound) || got < bound && !sameKMeansFloat(got, full) {
				t.Fatalf("n=%d bound=%v: bounded=%v full=%v", n, bound, got, full)
			}
		}
	}
	// The NaN tail demonstrates that the losing candidate really stops early.
	a, b := make([]float64, 17), make([]float64, 17)
	b[0], b[16] = 2, math.NaN()
	if got := l2SquaredUntil(a, b, 1); got != 4 {
		t.Fatalf("losing candidate was not cut off: %v", got)
	}
	if got := l2SquaredUntil(a, b, math.NaN()); !math.IsNaN(got) {
		t.Fatalf("NaN incumbent must not enable cutoff: %v", got)
	}
	b[0] = math.Inf(1)
	if got := l2SquaredUntil(a, b, math.Inf(1)); !math.IsInf(got, 1) {
		t.Fatalf("infinite loser should cut off without becoming a winner: %v", got)
	}
}

// The oracle deliberately evaluates every centroid, then recomputes the winner
// as Java KMeans.fit does. Its raw L2 loop is independent of the optimized kernel.
func unboundedKMeansAssignment(a kMeansAdapter, vectors []gVector, centroids [][]float64, order, assignment, projected []int, distances []float64) (int, error) {
	objective := func(v gVector, c []float64) (float64, error) {
		if a.codec.config.metric != VectorMetricCosine && !(a.codec.quantizer != nil && v.typ == rabitq.TypeByte) {
			return scalarKMeansL2(v.data, c), nil
		}
		return a.baseObjective(v, c)
	}
	changed := 0
	for _, i := range order {
		bestC := 0
		best, err := objective(vectors[i], centroids[0])
		if err != nil {
			return 0, err
		}
		for c := 1; c < len(centroids); c++ {
			score, err := objective(vectors[i], centroids[c])
			if err != nil {
				return 0, err
			}
			if score < best {
				best, bestC = score, c
			}
		}
		if assignment[i] != bestC {
			assignment[i] = bestC
			changed++
		}
		projected[bestC]++
		var err2 error
		distances[i], err2 = objective(vectors[i], centroids[bestC])
		if err2 != nil {
			return 0, err2
		}
	}
	return changed, nil
}

func checkKMeansAssignment(t *testing.T, a kMeansAdapter, vectors []gVector, centroids [][]float64) {
	t.Helper()
	order := make([]int, len(vectors))
	wantAssignment := make([]int, len(vectors))
	for i := range order {
		order[i] = len(order) - i - 1
		wantAssignment[i] = i % len(centroids)
	}
	gotAssignment := slices.Clone(wantAssignment)
	wantSizes, gotSizes := make([]int, len(centroids)), make([]int, len(centroids))
	wantDistances, gotDistances := make([]float64, len(vectors)), make([]float64, len(vectors))
	wantChanged, wantErr := unboundedKMeansAssignment(a, vectors, centroids, order, wantAssignment, wantSizes, wantDistances)
	gotChanged, gotErr := assignmentStep(a, vectors, centroids, order, gotAssignment, gotSizes, gotDistances)
	if fmt.Sprintf("%T:%v", gotErr, gotErr) != fmt.Sprintf("%T:%v", wantErr, wantErr) {
		t.Fatalf("error=%T:%v, want %T:%v", gotErr, gotErr, wantErr, wantErr)
	}
	if wantErr != nil {
		return
	}
	if gotChanged != wantChanged || !slices.Equal(gotAssignment, wantAssignment) || !slices.Equal(gotSizes, wantSizes) {
		t.Fatalf("changed=%d assignment=%v sizes=%v, want %d %v %v", gotChanged, gotAssignment, gotSizes, wantChanged, wantAssignment, wantSizes)
	}
	for i, want := range wantDistances {
		if !sameKMeansFloat(gotDistances[i], want) {
			t.Fatalf("winning distance[%d]=%x, want exact %x", i, math.Float64bits(gotDistances[i]), math.Float64bits(want))
		}
	}
}

func TestKMeansAssignmentMatchesUnboundedSequentialOracle(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewSource(451))
	for _, n := range []int{0, 1, 3, 4, 7, 8, 9, 15, 16, 17, 31, 32, 33, 97, 257} {
		vectors := make([]gVector, 17)
		for i := range vectors {
			vectors[i] = gVector{data: make([]float64, n), typ: vectorcodec.TypeDouble}
			for d := range vectors[i].data {
				vectors[i].data[d] = random.NormFloat64() + float64(i%4)*10
			}
		}
		centroids := make([][]float64, 4)
		for i := range centroids {
			centroids[i] = slices.Clone(vectors[i].data)
		}
		for _, metric := range []VectorMetric{VectorMetricEuclidean, VectorMetricEuclideanSquare, VectorMetricCosine} {
			checkKMeansAssignment(t, kMeansAdapter{codec: &guardiannVectorCodec{config: guardiannConfig{metric: metric}}}, vectors, centroids)
		}
	}
}

func TestKMeansAssignmentPruningNonFiniteAndTies(t *testing.T) {
	t.Parallel()
	a := kMeansAdapter{codec: &guardiannVectorCodec{config: guardiannConfig{metric: VectorMetricEuclidean}}}
	for _, first := range []float64{0, 1, math.Inf(1), math.NaN()} {
		for _, tail := range []float64{0, math.Inf(1), math.NaN()} {
			centroids := make([][]float64, 4)
			for i := range centroids {
				centroids[i] = make([]float64, 17)
			}
			centroids[0][0] = first
			centroids[1][0], centroids[1][16] = 2, tail
			centroids[2][16] = 0.5
			centroids[3][16] = -0.5 // Tie must retain the earlier winner.
			checkKMeansAssignment(t, a, []gVector{{data: make([]float64, 17), typ: vectorcodec.TypeDouble}}, centroids)
		}
	}
}

func TestKMeansAssignmentPruningPreservesQuantizedEstimator(t *testing.T) {
	t.Parallel()
	cfg := defaultGuardiannConfig(17)
	cfg.useRaBitQ = true
	codec := newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: make([]float64, 17)})
	centroids := [][]float64{make([]float64, 17), make([]float64, 17)}
	centroids[1][0], centroids[1][16] = 10, 20
	encoded, err := codec.decode(codec.encode(gVector{data: slices.Clone(centroids[1]), typ: vectorcodec.TypeDouble}))
	if err != nil {
		t.Fatal(err)
	}
	checkKMeansAssignment(t, kMeansAdapter{codec: codec}, []gVector{encoded}, centroids)
	encoded.encoded = []byte{rabitq.TypeByte}
	checkKMeansAssignment(t, kMeansAdapter{codec: codec}, []gVector{encoded}, centroids)
}
