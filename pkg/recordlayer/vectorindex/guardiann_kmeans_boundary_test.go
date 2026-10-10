package vectorindex

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

func TestKMeansFinalAssignmentBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		last       float64
		iterations int
		centroids  []float64
		distances  []float64
		objective  float64
	}{
		{"cap_reassigns", 10, 1, []float64{0, 6}, []float64{0, 4, 16}, 20},
		{"cap_refreshes_scores", 10, 2, []float64{1, 10}, []float64{1, 1, 0}, 2},
		{"converged", 10, 8, []float64{1, 10}, []float64{1, 1, 0}, 2},
		{"cap_tie_prefers_first_centroid", 6, 1, []float64{0, 4}, []float64{0, 4, 4}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vectors := []gVector{
				{data: []float64{0}, typ: vectorcodec.TypeDouble},
				{data: []float64{2}, typ: vectorcodec.TypeDouble},
				{data: []float64{tc.last}, typ: vectorcodec.TypeDouble},
			}
			// Seed 20 selects initial centroids 0 and 2. After the first update,
			// point 2 must move to the first cluster in the final assignment.
			random := &splittableRandom{seed: 20, gamma: goldenGamma}
			codec := &guardiannVectorCodec{config: guardiannConfig{metric: VectorMetricEuclidean}}
			got, err := kMeansFit(random, codec, vectors, 2, tc.iterations, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.assignment, []int{0, 0, 1}) || !slices.Equal(got.clusterSizes, []int{2, 1}) {
				t.Fatalf("assignment=%v sizes=%v, want [0 0 1] and [2 1]", got.assignment, got.clusterSizes)
			}
			if !slices.Equal(got.distances, tc.distances) || got.objective != tc.objective {
				t.Fatalf("distances=%v objective=%v, want %v and %v", got.distances, got.objective, tc.distances, tc.objective)
			}
			if len(got.centroids) != len(tc.centroids) {
				t.Fatalf("centroids=%v, want %v", got.centroids, tc.centroids)
			}
			for i, want := range tc.centroids {
				if got.centroids[i].typ != vectorcodec.TypeDouble || !slices.Equal(got.centroids[i].data, []float64{want}) {
					t.Fatalf("centroid %d=%v, want DOUBLE [%v]", i, got.centroids[i], want)
				}
			}
			if random.seed != 0x3c6ef372fe94f83e || random.gamma != goldenGamma {
				t.Fatalf("RNG state=%+v, want exactly the two k-means++ draws", random)
			}
		})
	}
}
