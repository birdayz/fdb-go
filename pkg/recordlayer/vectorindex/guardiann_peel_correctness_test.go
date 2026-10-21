package vectorindex

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// Keep the peel's encoded-operand and full-iteration refit paths exercised
// under -race without measuring instrumentation against a production CPU budget.
func TestGuardiannPeelRefitCorrectness(t *testing.T) {
	t.Parallel()
	for _, metric := range []VectorMetric{VectorMetricEuclidean, VectorMetricCosine} {
		for _, quantized := range []bool{false, true} {
			t.Run(fmt.Sprintf("metric=%v/RaBitQ=%t", metric, quantized), func(t *testing.T) {
				t.Parallel()
				const n, d = 11, 4
				cfg := defaultGuardiannConfig(d)
				cfg.metric, cfg.useRaBitQ = metric, quantized
				g := newGuardiann(subspace.FromBytes([]byte(t.Name())), cfg, nil, nil)
				if quantized {
					g = g.withAccessInfo(&guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: make([]float64, d)})
				}
				primaries := make([]guardiannVectorRef, n)
				mean := make([]float64, d)
				for i := range primaries {
					v := []float64{1, 0.75, 0, 0}
					if i >= 5 {
						v[1] = -0.75
					}
					if i == n-1 {
						v = []float64{-1, 0, 0, 0}
					}
					half, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(v))
					if err != nil {
						t.Fatal(err)
					}
					vector := gVector{data: half, typ: vectorcodec.TypeHalf}
					if quantized {
						vector, err = g.codec.decode(g.codec.encode(vector))
						if err != nil {
							t.Fatal(err)
						}
						if vector.code == nil {
							t.Fatal("RaBitQ case did not produce an encoded operand")
						}
					}
					primaries[i] = guardiannVectorRef{id: guardiannVectorID{pk: tuple.Tuple{int64(i)}}, vector: vector, primary: true}
					addInto(mean, vector.data)
				}
				scale(mean, 1.0/n)
				current := guardiannCluster{centroid: gVector{data: mean, typ: vectorcodec.TypeDouble}, refs: primaries}

				// Start from an invalid 10:1 outlier partition; the refit must
				// recover the two five-member core groups and reassign the outlier.
				assignment := make([]int, n)
				assignment[n-1] = 1
				c12 := &repartitioningCandidate{cls: &clusterClassification{}, primaries: primaries, kMeans: kMeansResult{
					centroids: []gVector{
						{data: []float64{1, 0, 0, 0}, typ: vectorcodec.TypeDouble},
						{data: []float64{-1, 0, 0, 0}, typ: vectorcodec.TypeDouble},
					},
					assignment: assignment,
				}}
				initial, err := g.scoreCandidate([]guardiannCluster{current}, c12)
				if err != nil || initial.decision != decisionInvalidCandidate {
					t.Fatalf("initial partition: %+v, %v; want INVALID", initial, err)
				}
				cand, exit, err := g.peelSplit(newSplittableRandomForUUID(tuple.UUID{2}), current, c12)
				if err != nil || exit != peelSelected || cand == nil {
					t.Fatalf("peel exit=%v candidate=%v err=%v; want selected", exit, cand, err)
				}
				if len(cand.primaries) != n || len(cand.kMeans.assignment) != n || len(cand.kMeans.centroids) != 2 {
					t.Fatalf("peel lost its partition: primaries=%d assignments=%d centroids=%d", len(cand.primaries), len(cand.kMeans.assignment), len(cand.kMeans.centroids))
				}
				sizes := [2]int{}
				for i, a := range cand.kMeans.assignment {
					if a < 0 || a >= len(sizes) {
						t.Fatalf("primary %d assigned to %d", i, a)
					}
					sizes[a]++
					if r := cand.primaries[i]; !r.primary || len(r.id.pk) != 1 || r.id.pk[0] != int64(i) {
						t.Fatalf("primary %d lost its identity: %+v", i, r)
					}
					d0, err := g.distance(cand.primaries[i].vector, cand.kMeans.centroids[0])
					if err != nil {
						t.Fatal(err)
					}
					d1, err := g.distance(cand.primaries[i].vector, cand.kMeans.centroids[1])
					if err != nil {
						t.Fatal(err)
					}
					want := 0
					if d1 < d0 {
						want = 1
					}
					if a != want {
						t.Errorf("primary %d assigned to %d, want nearest %d", i, a, want)
					}
					if i < n-1 && a != cand.kMeans.assignment[i/5*5] {
						t.Errorf("core group %d was split", i/5)
					}
				}
				if min(sizes[0], sizes[1]) != 5 || max(sizes[0], sizes[1]) != 6 || cand.kMeans.assignment[0] == cand.kMeans.assignment[5] {
					t.Fatalf("peel partition sizes=%v; want distinct core groups and a 5:6 partition", sizes)
				}
				result, err := g.scoreCandidate([]guardiannCluster{current}, cand)
				if err != nil || result.decision != decisionAcceptCandidate || math.IsNaN(result.scoreGain) || math.IsInf(result.scoreGain, 0) {
					t.Fatalf("peeled partition: %+v, %v; want finite ACCEPT", result, err)
				}

				mass, inMass := make([]gVector, n-1), make([]bool, n)
				for i := range mass {
					mass[i], inMass[i] = primaries[i].vector, true
				}
				fit, err := kMeansLloyd(newSplittableRandomForUUID(tuple.UUID{3}), g.codec, mass, 2, cfg.kMeansMaxIterations, cfg.kMeansMaxRestarts, false)
				if err != nil {
					t.Fatal(err)
				}
				members, err := g.peelFarthest(primaries, inMass, fit.centroids[1])
				if err != nil || len(members) != len(mass) {
					t.Fatalf("farthest members=%d err=%v; want the ten-member mass", len(members), err)
				}
				seen := make([]bool, n-1)
				for i, member := range members {
					if member.index < 0 || member.index >= len(seen) || seen[member.index] {
						t.Fatalf("farthest list lost a mass member: %+v", members)
					}
					seen[member.index] = true
					if i > 0 && members[i-1].distance < member.distance {
						t.Fatalf("farthest list is not descending: %+v", members)
					}
				}
				reassigned, scored, err := g.peelCandidate(current, c12, fit.centroids)
				if err != nil || scored.decision != decisionAcceptCandidate || reassigned == nil || len(reassigned.kMeans.assignment) != n || math.IsNaN(scored.scoreGain) || math.IsInf(scored.scoreGain, 0) {
					t.Fatalf("full-iteration refit: %+v, %v; want ACCEPT over every primary", scored, err)
				}
			})
		}
	}
}
