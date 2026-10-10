package vectorindex

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// The peel's performance criterion (ws-d-design.md, "Go's performance
// criterion"): an admitted peel that outlasts FDB's 5 s transaction window
// fails with 1007 on every attempt, so the WHOLE peel — the candidate fit,
// every refit, the sorts, the assignment passes and the scoring — must take at
// most 2.5 s at the admission edges (n = 2000 at d = 980, W = B; n = 1001 at
// d = 2175) and at the acceptance fixtures' shapes (n = 2000 at d = 768,
// n = 1001 at d = 2048), HALF precision, default KMeans knobs, under the suite's
// concurrency. It runs in the full lane, beside the other FDB targets, so the
// measured maximum is a loaded one; the load average is logged before and
// after.
//
// Two measurements per shape and seed: the peel over the tight-core-plus-50-
// outliers generator, and its worst case, the candidate fit plus
// floor(log2(n - 1)) full refits on all n vectors, which bounds every input
// the admission lets through. A miss is a Go performance defect in the peel,
// fixed in Go; the bound B is never raised to meet it.

const peelTimeMargin = 2500 * time.Millisecond

func peelShape(n, d int, seed int64) []guardiannVectorRef {
	rnd := rand.New(rand.NewSource(seed))
	refs := make([]guardiannVectorRef, n)
	const outliers = 50
	for i := range refs {
		v := make([]float64, d)
		for j := range v {
			v[j] = rnd.NormFloat64() * 0.01
		}
		if i >= n-outliers {
			// The outliers: far from the core, each in its own direction.
			v[(i*7)%d] += 50
		}
		refs[i] = guardiannVectorRef{
			id:      guardiannVectorID{pk: tuple.Tuple{int64(i)}},
			vector:  gVector{data: halfRound(v), typ: vectorcodec.TypeHalf},
			primary: true,
		}
	}
	return refs
}

// halfRound rounds each component to half precision, as the stored vector is.
func halfRound(v []float64) []float64 {
	out, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(v))
	if err != nil {
		panic(err)
	}
	return out
}

func loadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

func TestGuardiannPeelPerformanceCriterion(t *testing.T) {
	if os.Getenv("FDB_PEEL_CPU_PROFILE") == "1" {
		dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
		if dir == "" {
			t.Fatal("FDB_PEEL_CPU_PROFILE requires Bazel's TEST_UNDECLARED_OUTPUTS_DIR")
		}
		profile, err := os.Create(filepath.Join(dir, "peel.cpu.pprof"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := profile.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := pprof.StartCPUProfile(profile); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pprof.StopCPUProfile)
	}
	shapes := []struct{ n, d int }{
		{2000, 980},  // W = B
		{1001, 2175}, // the first over-max size at its largest admitted d
		{2000, 768},  // the d = 768 acceptance fixture
		{1001, 2048}, // the d = 2048 acceptance fixture
	}
	var worst time.Duration
	for run := 0; run < 2; run++ {
		t.Logf("run %d: load before %s", run, loadAverage())
		for _, s := range shapes {
			cfg := defaultGuardiannConfig(s.d)
			if !peelAdmitted(s.n, s.d, cfg.kMeansMaxIterations, cfg.kMeansMaxRestarts) {
				t.Fatalf("shape n=%d d=%d is not admitted; the criterion times admitted shapes", s.n, s.d)
			}
			g := newGuardiann(subspace.FromBytes([]byte("peel-timing")), cfg, nil, nil)
			for _, seed := range []int64{1, 2} {
				primaries := peelShape(s.n, s.d, seed)
				current := guardiannCluster{centroid: gVector{data: make([]float64, s.d), typ: vectorcodec.TypeHalf}, refs: primaries}

				start := time.Now()
				c12, err := g.kMeansCandidate(&clusterClassification{}, primaries, newSplittableRandomForUUID(tuple.UUID{byte(seed)}), 2)
				if err != nil {
					t.Fatal(err)
				}
				_, exit, err := g.peelSplit(newSplittableRandomForUUID(tuple.UUID{byte(seed), 1}), current, c12)
				if err != nil {
					t.Fatal(err)
				}
				peel := time.Since(start)

				// The worst case the admission allows: every refit runs, each
				// on all n vectors.
				vectors := make([]gVector, len(primaries))
				for i, p := range primaries {
					vectors[i] = p.vector
				}
				refits := int(math.Floor(math.Log2(float64(s.n - 1))))
				start = time.Now()
				random := newSplittableRandomForUUID(tuple.UUID{byte(seed), 2})
				for r := 0; r <= refits; r++ { // the candidate fit, then the refits
					if _, err := kMeansFit(random.split(), g.codec, vectors, 2, cfg.kMeansMaxIterations, cfg.kMeansMaxRestarts); err != nil {
						t.Fatal(err)
					}
				}
				bound := time.Since(start)
				t.Logf("n=%d d=%d seed=%d: peel %v (exit %d), worst case (%d refits) %v", s.n, s.d, seed, peel, exit, refits, bound)
				worst = max(worst, peel, bound)
			}
		}
		t.Logf("run %d: load after %s", run, loadAverage())
	}
	t.Logf("maximum over every run, shape and seed: %v (margin %v)", worst, peelTimeMargin)
	if worst > peelTimeMargin {
		t.Errorf("the peel took %v, above the %v margin: a Go performance defect in the peel (fix it in Go; B is never raised)", worst, peelTimeMargin)
	}
}
