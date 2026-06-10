package recordlayer

import (
	"math"
	"sort"
)

// SPFresh clustering and closure assignment (RFC-094 §5, §6, §8). Pure CPU,
// deterministic given the seed — splits and builds must be reproducible for
// the pinned lifecycle tests, so all randomness flows through the same
// splittableRandom the rest of the index uses.

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
	for i := range d2 {
		d2[i] = spfreshSquaredDistance(vectors[i], centroids[0])
	}
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
		for i := range d2 {
			if d := spfreshSquaredDistance(vectors[i], c); d < d2[i] {
				d2[i] = d
			}
		}
	}

	// Lloyd iterations.
	assign = make([]int, n)
	counts := make([]int, k)
	sums := make([][]float64, k)
	for i := range sums {
		sums[i] = make([]float64, dims)
	}
	for iter := 0; iter < maxIters; iter++ {
		changed := false
		for i, v := range vectors {
			best, bestD := 0, math.Inf(1)
			for c := range centroids {
				if d := spfreshSquaredDistance(v, centroids[c]); d < bestD {
					best, bestD = c, d
				}
			}
			if assign[i] != best || iter == 0 {
				changed = changed || assign[i] != best
				assign[i] = best
			}
		}
		if iter > 0 && !changed {
			break
		}
		for c := range sums {
			counts[c] = 0
			for d := range sums[c] {
				sums[c][d] = 0
			}
		}
		for i, v := range vectors {
			c := assign[i]
			counts[c]++
			for d, x := range v {
				sums[c][d] += x
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
	for i, v := range vectors {
		best, bestD := 0, math.Inf(1)
		for c := range centroids {
			if d := spfreshSquaredDistance(v, centroids[c]); d < bestD {
				best, bestD = c, d
			}
		}
		assign[i] = best
	}
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
func spfreshNearestK(query []float64, ids []int64, vecs [][]float64, k int) []spfreshCandidate {
	cands := make([]spfreshCandidate, len(ids))
	for i := range ids {
		cands[i] = spfreshCandidate{id: ids[i], d2: spfreshSquaredDistance(query, vecs[i])}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].d2 != cands[j].d2 {
			return cands[i].d2 < cands[j].d2
		}
		return cands[i].id < cands[j].id
	})
	if k < len(cands) {
		cands = cands[:k]
	}
	return cands
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
