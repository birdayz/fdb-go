package recordlayer

import (
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// SPFresh clustering and closure assignment (RFC-094 §5, §6, §8). Pure CPU,
// deterministic given the seed — splits and builds must be reproducible for
// the pinned lifecycle tests, so all randomness flows through the same
// splittableRandom the rest of the index uses.

// spfreshKMeansChunk is the fixed work-unit size for the parallel loops.
// FIXED (not derived from worker count) so chunk boundaries — and therefore
// the float accumulation order in the chunk-merged reductions — are identical
// on every machine: determinism holds per (vectors, k, seed) regardless of
// GOMAXPROCS.
const spfreshKMeansChunk = 4096

// spfreshParallelChunks runs fn over [0,n) in fixed-size chunks across
// NumCPU workers. fn must only write chunk-local or index-disjoint state.
func spfreshParallelChunks(n int, fn func(chunk, lo, hi int)) {
	chunks := (n + spfreshKMeansChunk - 1) / spfreshKMeansChunk
	if chunks <= 1 {
		if n > 0 {
			fn(0, 0, n)
		}
		return
	}
	workers := runtime.NumCPU()
	if workers > chunks {
		workers = chunks
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c := int(next.Add(1) - 1)
				if c >= chunks {
					return
				}
				lo := c * spfreshKMeansChunk
				hi := lo + spfreshKMeansChunk
				if hi > n {
					hi = n
				}
				fn(c, lo, hi)
			}
		}()
	}
	wg.Wait()
}

// spfreshKMeans runs Lloyd's algorithm with k-means++ seeding on the given
// vectors, returning k centroids and each vector's assignment. Deterministic
// for a given (vectors, k, seed). Empty clusters are re-seeded from the point
// farthest from its centroid (a balance nudge; build-time sub-Lmin folding and
// the merge machinery handle the rest — RFC-094 §8).
//
// vectors must be non-empty and share one dimensionality. k is clamped to
// [1, len(vectors)].
func spfreshKMeans(vectors [][]float64, k int, seed int64, maxIters int) (centroids [][]float64, assign []int) {
	n := len(vectors)
	if n == 0 {
		return nil, nil
	}
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}
	dims := len(vectors[0])
	rng := &splittableRandom{seed: seed, gamma: goldenGamma}

	// k-means++ seeding: first centroid uniform, then proportional to squared
	// distance from the nearest chosen centroid.
	centroids = make([][]float64, 0, k)
	first := int(uint64(rng.nextLong()) % uint64(n))
	centroids = append(centroids, append([]float64(nil), vectors[first]...))

	d2 := make([]float64, n) // squared distance to nearest chosen centroid
	spfreshParallelChunks(n, func(_, lo, hi int) {
		for i := lo; i < hi; i++ {
			d2[i] = spfreshSquaredDistance(vectors[i], centroids[0])
		}
	})
	for len(centroids) < k {
		var sum float64
		for _, d := range d2 {
			sum += d
		}
		var idx int
		if sum <= 0 {
			// All remaining points coincide with chosen centroids; pick any.
			idx = int(uint64(rng.nextLong()) % uint64(n))
		} else {
			target := rng.nextDouble() * sum
			var acc float64
			idx = n - 1
			for i, d := range d2 {
				acc += d
				if acc >= target {
					idx = i
					break
				}
			}
		}
		c := append([]float64(nil), vectors[idx]...)
		centroids = append(centroids, c)
		spfreshParallelChunks(n, func(_, lo, hi int) {
			for i := lo; i < hi; i++ {
				if d := spfreshSquaredDistance(vectors[i], c); d < d2[i] {
					d2[i] = d
				}
			}
		})
	}

	// Lloyd iterations.
	assign = make([]int, n)
	counts := make([]int, k)
	sums := make([][]float64, k)
	for i := range sums {
		sums[i] = make([]float64, dims)
	}
	numChunks := (n + spfreshKMeansChunk - 1) / spfreshKMeansChunk
	for iter := 0; iter < maxIters; iter++ {
		// Assignment: index-disjoint writes, order-independent — parallel.
		var changedFlag atomic.Bool
		spfreshParallelChunks(n, func(_, lo, hi int) {
			chunkChanged := false
			for i := lo; i < hi; i++ {
				v := vectors[i]
				best, bestD := 0, math.Inf(1)
				for c := range centroids {
					if d := spfreshSquaredDistance(v, centroids[c]); d < bestD {
						best, bestD = c, d
					}
				}
				if assign[i] != best {
					chunkChanged = true
					assign[i] = best
				}
			}
			if chunkChanged {
				changedFlag.Store(true)
			}
		})
		if iter > 0 && !changedFlag.Load() {
			break
		}
		// Accumulation: per-chunk partials merged in CHUNK ORDER, so the
		// float addition order is fixed by the constant chunk size —
		// deterministic on any machine, any worker count.
		partialCounts := make([][]int, numChunks)
		partialSums := make([][][]float64, numChunks)
		spfreshParallelChunks(n, func(chunk, lo, hi int) {
			pc := make([]int, k)
			ps := make([][]float64, k)
			for i := lo; i < hi; i++ {
				c := assign[i]
				pc[c]++
				if ps[c] == nil {
					ps[c] = make([]float64, dims)
				}
				for d, x := range vectors[i] {
					ps[c][d] += x
				}
			}
			partialCounts[chunk] = pc
			partialSums[chunk] = ps
		})
		for c := range sums {
			counts[c] = 0
			for d := range sums[c] {
				sums[c][d] = 0
			}
		}
		for chunk := 0; chunk < numChunks; chunk++ {
			for c := 0; c < k; c++ {
				counts[c] += partialCounts[chunk][c]
				if partialSums[chunk][c] != nil {
					for d, x := range partialSums[chunk][c] {
						sums[c][d] += x
					}
				}
			}
		}
		for c := range centroids {
			if counts[c] == 0 {
				// Empty cluster: re-seed from the point farthest from its
				// current centroid (deterministic).
				far, farD := 0, -1.0
				for i, v := range vectors {
					if d := spfreshSquaredDistance(v, centroids[assign[i]]); d > farD {
						far, farD = i, d
					}
				}
				copy(centroids[c], vectors[far])
				assign[far] = c
				continue
			}
			inv := 1.0 / float64(counts[c])
			for d := range centroids[c] {
				centroids[c][d] = sums[c][d] * inv
			}
		}
	}
	// Final assignment against the converged centroids.
	spfreshParallelChunks(n, func(_, lo, hi int) {
		for i := lo; i < hi; i++ {
			v := vectors[i]
			best, bestD := 0, math.Inf(1)
			for c := range centroids {
				if d := spfreshSquaredDistance(v, centroids[c]); d < bestD {
					best, bestD = c, d
				}
			}
			assign[i] = best
		}
	})
	return centroids, assign
}

// spfreshSquaredDistance is squared L2 — the k-means objective and the routing
// comparator (monotone in true L2, so nearest-centroid selection never needs
// the sqrt).
func spfreshSquaredDistance(a, b []float64) float64 {
	var sum float64
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// spfreshCandidate is a (id, squared-distance) pair used by routing and
// closure assignment.
type spfreshCandidate struct {
	id int64
	d2 float64
}

// spfreshNearestK returns the k nearest candidates by squared distance,
// ascending, deterministic tie-break by id. ids[i] corresponds to vecs[i].
//
// TOP-K SELECTION, not a full sort: k is tiny (replication r≈2 on the build
// closure path, w/kc ≤ ~200 on the routing path) while len(ids) reaches tens
// of thousands of fine centroids at 1M+ scale. The full sort.Slice here made
// the bulk build effectively unbounded — wave B sorted all ~11k fines per
// staged vector to take the top 2; ~2 billion comparator calls at SIFT-1M
// (caught by a SIGQUIT stack dump of a build that outlived 2 hours).
func spfreshNearestK(query []float64, ids []int64, vecs [][]float64, k int) []spfreshCandidate {
	if k <= 0 || len(ids) == 0 {
		return nil
	}
	less := func(a, b spfreshCandidate) bool {
		if a.d2 != b.d2 {
			return a.d2 < b.d2
		}
		return a.id < b.id
	}
	out := make([]spfreshCandidate, 0, min(k, len(ids))+1)
	for i := range ids {
		c := spfreshCandidate{id: ids[i], d2: spfreshSquaredDistance(query, vecs[i])}
		if len(out) == k && !less(c, out[k-1]) {
			continue // common case after warmup: not in the top k
		}
		pos := sort.Search(len(out), func(j int) bool { return less(c, out[j]) })
		out = append(out, spfreshCandidate{})
		copy(out[pos+1:], out[pos:])
		out[pos] = c
		if len(out) > k {
			out = out[:k]
		}
	}
	return out
}

// spfreshClosure applies SPANN's RNG closure rule (RFC-094 §5): from the
// r-nearest candidates (ascending), keep candidate c_i iff
//
//	dist(v, c_i) <= alpha * dist(v, c_1)
//
// — in squared-distance space the threshold is alpha² · d2(c_1). alpha > 1
// admits boundary replicas; alpha == 1 degenerates to r == 1 (the rev-3 RFC
// bug, rejected at config validation when r > 1). Always returns at least the
// nearest candidate.
func spfreshClosure(cands []spfreshCandidate, r int, alpha float64) []spfreshCandidate {
	if len(cands) == 0 {
		return nil
	}
	if r > len(cands) {
		r = len(cands)
	}
	threshold := alpha * alpha * cands[0].d2
	out := cands[:1:1]
	for _, c := range cands[1:r] {
		if c.d2 <= threshold {
			out = append(out, c)
		}
	}
	return out
}
