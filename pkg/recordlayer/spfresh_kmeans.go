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
	spfreshParallelChunksSized(n, spfreshKMeansChunk, 0, fn)
}

// spfreshParallelChunksSized is the parameterized core. workers == 0 means
// NumCPU; tests pin worker-count invariance — the actual cross-machine
// determinism guarantee — by comparing workers=1 against the parallel run at
// the SAME chunk size (chunk boundaries fix the float-reduction order, so
// chunk size itself is NOT invariant and must match).
func spfreshParallelChunksSized(n, chunkSize, workers int, fn func(chunk, lo, hi int)) {
	chunks := (n + chunkSize - 1) / chunkSize
	if chunks <= 1 {
		if n > 0 {
			fn(0, 0, n)
		}
		return
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
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
				lo := c * chunkSize
				hi := lo + chunkSize
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
	return spfreshKMeansWorkers(vectors, k, seed, maxIters, 0)
}

// spfreshKMeansWorkers is the worker-count-parameterized core (0 = NumCPU).
// The FIXED chunk size pins the float-accumulation order of the parallel
// reductions, so any two runs with the same (vectors, k, seed) are
// bit-identical regardless of worker count — pinned by the workers=1 vs
// parallel comparison test.
func spfreshKMeansWorkers(vectors [][]float64, k int, seed int64, maxIters, workers int) (centroids [][]float64, assign []int) {
	n := len(vectors)
	if n == 0 {
		return nil, nil
	}
	const chunkSize = spfreshKMeansChunk
	chunked := func(fn func(chunk, lo, hi int)) { spfreshParallelChunksSized(n, chunkSize, workers, fn) }
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
	chunked(func(_, lo, hi int) {
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
		chunked(func(_, lo, hi int) {
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
	numChunks := (n + chunkSize - 1) / chunkSize
	// Partial-reduction buffers live OUTSIDE the iteration loop: at the
	// coarse-1M shape (62 chunks × k≈3k × 128 dims) reallocating them every
	// Lloyd iteration churned ~190MB per iteration through the allocator.
	// Zeroed at chunk start each iteration instead; per-cluster rows are
	// still allocated lazily (most chunks see a small subset of clusters)
	// and zeroing is unconditional once allocated — a row whose cluster
	// skips an iteration must not leak stale sums into a later one.
	partialCounts := make([][]int, numChunks)
	partialSums := make([][][]float64, numChunks)
	for chunk := range partialCounts {
		partialCounts[chunk] = make([]int, k)
		partialSums[chunk] = make([][]float64, k)
	}
	for iter := 0; iter < maxIters; iter++ {
		// Assignment: index-disjoint writes, order-independent — parallel.
		var changedFlag atomic.Bool
		chunked(func(_, lo, hi int) {
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
		chunked(func(chunk, lo, hi int) {
			pc := partialCounts[chunk]
			ps := partialSums[chunk]
			for c := range pc {
				pc[c] = 0
				if ps[c] != nil {
					for d := range ps[c] {
						ps[c][d] = 0
					}
				}
			}
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
	chunked(func(_, lo, hi int) {
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
// the sqrt). Four independent accumulator lanes break the loop-carried
// dependency the scalar form serializes on (Go does not auto-vectorize; this
// kernel is the single largest flat CPU cost in build and routing profiles).
// The lane-summed float order differs from the scalar form only in last-ulp
// rounding, and is itself fixed per length — determinism per (vectors, k,
// seed) is unchanged.
func spfreshSquaredDistance(a, b []float64) float64 {
	b = b[:len(a)] // one bounds check; lets the compiler drop the per-lane ones
	var s0, s1, s2, s3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		d0 := a[i] - b[i]
		d1 := a[i+1] - b[i+1]
		d2 := a[i+2] - b[i+2]
		d3 := a[i+3] - b[i+3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	for ; i < len(a); i++ {
		d := a[i] - b[i]
		s0 += d * d
	}
	return (s0 + s1) + (s2 + s3)
}

// spfreshCandidate is a (id, squared-distance) pair used by routing and
// closure assignment. vec is the candidate's centroid vector when the caller
// has it — required for the closure's RNG diversity test, which compares
// centroid-to-centroid distances (nil degrades the closure to the pure ratio
// rule; see spfreshRNGAccept).
type spfreshCandidate struct {
	id  int64
	d2  float64
	vec []float64
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
		c := spfreshCandidate{id: ids[i], d2: spfreshSquaredDistance(query, vecs[i]), vec: vecs[i]}
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

// spfreshClosure applies SPANN's closure assignment (RFC-094 §5) with the
// §3.2.2 RNG representative-replication rule (Figure 5). Scanning candidates
// ascending by distance, keep candidate c iff
//
//  1. ratio:	dist(v, c) <= alpha * dist(v, c_1)	(boundary test, Eq. 2)
//  2. RNG:	dist(v, c)  < dist(s, c) for every already-kept s	(diversity)
//
// — in squared-distance space the ratio threshold is alpha² · d2(c_1).
// The RNG rule skips a replica that sits closer to an already-kept centroid
// than to the vector itself: such posting lists are near-duplicates in the
// same direction and both get recalled by the router anyway (SPANN Fig. 5 —
// better to spend the copy on a list in a different direction). Because RNG
// can skip, the scan runs past index r until r diverse replicas are kept or
// the ratio bound breaks (candidates are ascending, so the first ratio
// failure ends the scan). alpha > 1 admits boundary replicas; alpha == 1
// degenerates to r == 1 (the rev-3 RFC bug, rejected at config validation
// when r > 1). Always returns at least the nearest candidate.
func spfreshClosure(cands []spfreshCandidate, r int, alpha float64) []spfreshCandidate {
	if len(cands) == 0 {
		return nil
	}
	threshold := alpha * alpha * cands[0].d2
	out := cands[:1:1]
	for _, c := range cands[1:] {
		if len(out) >= r {
			break
		}
		if c.d2 > threshold {
			break // ascending order: every later candidate fails the ratio test too
		}
		if !spfreshRNGAccept(c, out) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// spfreshClosurePool is the candidate-pool width that gives the closure's
// RNG rule room to skip same-direction replicas: a pool of exactly r would
// turn every RNG rejection into a silently smaller copy-set (under-
// replication instead of diversity). 8× the replica target is SPTAG's
// headroom (replicaCount 8, candidate set 64). A fixed pool can still
// truncate ahead of a diverse in-ratio candidate (codex 094.4 r2) — the
// build path widens iteratively until the RATIO bound terminates the scan;
// the insert path deliberately caps here (each verified candidate is a
// sequential REAL read inside the user's save transaction) and leaves the
// rare beyond-cap miss to NPA, which re-runs the closure over the full
// neighborhood pool after every split.
func spfreshClosurePool(r int) int {
	return max(8*r, 16)
}

// spfreshRNGAccept is SPANN §3.2.2's RNG test: candidate c is a useful
// replica only if the vector is strictly closer to c than every already-kept
// centroid is (SPTAG rejects on equality; squared distances preserve the
// comparison). A candidate without a centroid vector falls open to accepted —
// the closure then degrades to the pure ratio rule rather than failing, since
// replica diversity is a recall optimization, not a correctness requirement.
func spfreshRNGAccept(c spfreshCandidate, kept []spfreshCandidate) bool {
	if c.vec == nil {
		return true
	}
	for _, s := range kept {
		if s.vec == nil {
			continue
		}
		if spfreshSquaredDistance(s.vec, c.vec) <= c.d2 {
			return false
		}
	}
	return true
}
