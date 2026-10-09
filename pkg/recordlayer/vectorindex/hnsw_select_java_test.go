package vectorindex

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TestSelectNeighbors_IsJavasSelectCandidates pins Primitives.selectCandidates:
// the diversity heuristic runs even when every candidate fits under m (a
// candidate closer to an already selected neighbour than to the query is
// dropped), candidates are polled by distance then primary key, and a metric
// without the triangle inequality takes the nearest min(m, n) as they are.
func TestSelectNeighbors_IsJavasSelectCandidates(t *testing.T) {
	t.Parallel()
	cand := func(pk int64, dist float64, v ...float64) hnswCandidate {
		return hnswCandidate{pkSpan: nestPK(tuple.Tuple{pk}), vec: v, dist: dist}
	}
	ids := func(cs []hnswCandidate) []int64 {
		var out []int64
		for _, c := range cs {
			pk, err := decodeNestedPK(c.pkSpan)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, pk[0].(int64))
		}
		return out
	}
	// Query at (0.15, 0.024): A (0.095, 0.02) is nearest, B (0.03, 0.017) is
	// closer to A (0.065) than to the query (0.12), C is far from everything.
	euclid := &hnswGraph{config: HNSWConfig{Metric: VectorMetricEuclidean}}
	got := ids(euclid.selectNeighbors([]hnswCandidate{
		cand(2, 0.12, 0.03, 0.017), cand(3, 141.2, 100, 100), cand(1, 0.055, 0.095, 0.02),
	}, 16))
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("selected %v, want [1 3]: B is pruned though all three fit under m", got)
	}

	// Ties on distance are broken by primary key.
	got = ids(euclid.selectNeighbors([]hnswCandidate{cand(9, 1, 0, 9), cand(4, 1, 9, 0)}, 16))
	if len(got) != 2 || got[0] != 4 || got[1] != 9 {
		t.Fatalf("selected %v, want [4 9]", got)
	}

	// No triangle inequality: the nearest min(m, n), never past the slice.
	square := &hnswGraph{config: HNSWConfig{Metric: VectorMetricEuclideanSquare}}
	got = ids(square.selectNeighbors([]hnswCandidate{cand(2, 2, 0), cand(1, 1, 0)}, 16))
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("selected %v, want [1 2]", got)
	}
}
