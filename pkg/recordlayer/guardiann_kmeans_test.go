package recordlayer

import (
	"fmt"
	"testing"
)

// Golden values from Java 4.14.2.0 KMeans.fit (SplittableRandom 11, 8
// iterations, 3 restarts, lambda 0) and PartitionEvaluator.evaluate.
func TestKMeansAndPartitionEvaluatorMatchJava(t *testing.T) {
	t.Parallel()
	data := &splittableRandom{seed: 3, gamma: goldenGamma}
	vectors := make([][]float64, 40)
	for i := range vectors {
		c := float64(i%3) * 5
		x := c + data.nextDouble()
		y := -c + data.nextDouble()
		vectors[i] = []float64{x, y, data.nextDouble()}
	}
	want := map[string]string{
		"E1":    "1376.4505252442514 [0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0]",
		"E2":    "331.4601286389292 [0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0 1 1 0]",
		"E3":    "8.41222838806594 [2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2 0 1 2]",
		"C1":    "13.530068058483678 [0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0]",
		"C2":    "1.1221855215397527 [1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1 0 0 1]",
		"C3":    "0.45949116099882137 [1 0 0 2 0 0 1 0 0 1 0 0 2 0 0 2 0 0 2 0 0 2 0 0 2 0 0 2 0 0 2 0 0 2 0 0 2 0 0 1]",
		"Eeval": "accept 2.0573033559606397",
		"Ceval": "accept 3.7833933954171326",
	}
	for _, m := range []struct {
		name   string
		metric VectorMetric
	}{{"E", VectorMetricEuclidean}, {"C", VectorMetricCosine}} {
		for k := 1; k <= 3; k++ {
			r := kMeansFit(&splittableRandom{seed: 11, gamma: goldenGamma}, m.metric, vectors, k, 8, 3)
			key := fmt.Sprintf("%s%d", m.name, k)
			if got := fmt.Sprint(r.objective, r.assignment); got != want[key] {
				t.Errorf("%s: %s, want %s", key, got, want[key])
			}
			if k != 2 {
				continue
			}
			centroids := make([][]float64, len(r.centroids))
			for i, c := range r.centroids {
				centroids[i] = c.data
			}
			e := evaluatePartitions(vectors, partition{centroids: [][]float64{{5, -5, 0.5}}, assignments: make([]int, len(vectors))},
				vectors, partition{centroids: centroids, assignments: r.assignment}, defaultPartitionParameters(m.metric))
			decision := map[partitionDecision]string{decisionKeepCurrent: "keep", decisionAcceptCandidate: "accept", decisionInvalidCandidate: "invalid"}
			if got := fmt.Sprint(decision[e.decision], " ", e.scoreGain); got != want[m.name+"eval"] {
				t.Errorf("%s evaluate: %s, want %s", m.name, got, want[m.name+"eval"])
			}
		}
	}
}
