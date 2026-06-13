package recordlayer

import (
	"math/rand"
	"testing"
)

// BenchmarkSPFreshBuildAssign measures the wave-B per-vector assignment cost —
// the bulk-build bottleneck (RFC-099). It synthesizes a 1M-scale topology
// (~245 coarse cells × ~25 fine centroids ≈ 6,100 fines) and times assign()
// over a stream of query-like vectors. The flat-scan router scores ALL ~6,100
// fines per vector; the two-level router scores only the fines in the w
// nearest cells (~w×25). go test -bench, no FDB.
func benchAssignTopology(rng *rand.Rand, dims, nCells, finesPerCell int) (map[int64][]int64, map[int64][][]float64, [][]float64) {
	fineIDs := make(map[int64][]int64, nCells)
	fineVecs := make(map[int64][][]float64, nCells)
	cellVecs := make([][]float64, nCells)
	var fid int64 = 1
	for c := 0; c < nCells; c++ {
		// Each cell is a Gaussian blob around a random center.
		center := make([]float64, dims)
		for d := range center {
			center[d] = rng.Float64() * 100
		}
		cellVecs[c] = center
		for f := 0; f < finesPerCell; f++ {
			v := make([]float64, dims)
			for d := range v {
				v[d] = center[d] + rng.NormFloat64()*3
			}
			fineIDs[int64(c+1)] = append(fineIDs[int64(c+1)], fid)
			fineVecs[int64(c+1)] = append(fineVecs[int64(c+1)], v)
			fid++
		}
	}
	return fineIDs, fineVecs, cellVecs
}

func BenchmarkSPFreshBuildAssign(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	const dims, nCells, finesPerCell = 128, 245, 25
	fineIDs, fineVecs, cellVecs := benchAssignTopology(rng, dims, nCells, finesPerCell)
	config := DefaultSPFreshConfig(dims)
	coarseIDs := make([]int64, nCells)
	for c := 0; c < nCells; c++ {
		coarseIDs[c] = int64(c + 1)
	}
	router := &spfreshBuildRouter{
		coarseIDs:    coarseIDs,
		coarseVecs:   cellVecs,
		cellFineIDs:  fineIDs,
		cellFineVecs: fineVecs,
		w:            DefaultSPFreshConfig(dims).BuildAssignCells,
	}

	// A stream of query-like vectors near random cell centers.
	queries := make([][]float64, 256)
	for i := range queries {
		v := make([]float64, dims)
		for d := range v {
			v[d] = rng.Float64()*100 + rng.NormFloat64()*3
		}
		queries[i] = v
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		router.assign(queries[i%len(queries)], config.Replication, config.Alpha)
	}
}
