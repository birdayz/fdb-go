package recordlayer

import (
	"errors"
	"fmt"
	"testing"
)

// Golden values from Java 4.14.2.0 KMeans.fit (SplittableRandom 11, 8
// iterations, 3 restarts, lambda 0) and PartitionEvaluator.evaluate.
func TestKMeansAndPartitionEvaluatorMatchJava(t *testing.T) {
	t.Parallel()
	data := &splittableRandom{seed: 3, gamma: goldenGamma}
	vectors := make([]gVector, 40)
	for i := range vectors {
		c := float64(i%3) * 5
		x := c + data.nextDouble()
		y := -c + data.nextDouble()
		vectors[i] = gVector{data: []float64{x, y, data.nextDouble()}, typ: 2}
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
			r, err := kMeansFit(&splittableRandom{seed: 11, gamma: goldenGamma}, &guardiannVectorCodec{config: guardiannConfig{metric: m.metric}}, vectors, k, 8, 3)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s%d", m.name, k)
			if got := fmt.Sprint(r.objective, r.assignment); got != want[key] {
				t.Errorf("%s: %s, want %s", key, got, want[key])
			}
			if k != 2 {
				continue
			}
			centroids := make([]gVector, len(r.centroids))
			for i, c := range r.centroids {
				centroids[i] = c
			}
			e, err := evaluatePartitions(vectors, partition{centroids: []gVector{{data: []float64{5, -5, 0.5}, typ: 2}}, assignments: make([]int, len(vectors))},
				vectors, partition{centroids: centroids, assignments: r.assignment}, defaultPartitionParameters(m.metric))
			if err != nil {
				t.Fatal(err)
			}
			decision := map[partitionDecision]string{decisionKeepCurrent: "keep", decisionAcceptCandidate: "accept", decisionInvalidCandidate: "invalid"}
			if got := fmt.Sprint(decision[e.decision], " ", e.scoreGain); got != want[m.name+"eval"] {
				t.Errorf("%s evaluate: %s, want %s", m.name, got, want[m.name+"eval"])
			}
		}
	}
}

// TestKMeansQuantizedMatchJava preserves the estimator's encoded-operand path.
func TestKMeansQuantizedMatchJava(t *testing.T) {
	t.Parallel()
	cfg := defaultGuardiannConfig(3)
	cfg.useRaBitQ = true
	codec, err := newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: []float64{0, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	var vectors []gVector
	for i := 0; i < 8; i++ {
		v := gVector{data: []float64{float64(i / 4 * 10), float64(i % 4), 1}, typ: 2}
		if i >= 3 {
			v, err = codec.decode(codec.encode(v))
			if err != nil {
				t.Fatal(err)
			}
		}
		vectors = append(vectors, v)
	}
	for k, want := range []string{"207.93312416612986 [0 0 0 0 0 0 0 0]", "8.930730185115753 [0 0 0 0 1 1 1 1]"} {
		r, err := kMeansFit(&splittableRandom{seed: 11, gamma: goldenGamma}, codec, vectors, k+1, 8, 3)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(r.objective, r.assignment); got != want {
			t.Errorf("k=%d: got %s want %s", k+1, got, want)
		}
		if k == 1 {
			params := defaultPartitionParameters(cfg.metric)
			params.codec = codec
			result, err := evaluatePartitions(vectors, partition{centroids: []gVector{{data: []float64{5, 1.5, 1}, typ: 2}}, assignments: make([]int, 8)}, vectors, partition{centroids: r.centroids, assignments: r.assignment}, params)
			if err != nil || result.decision != decisionAcceptCandidate || result.scoreGain != 4.295281704527504 {
				t.Fatalf("Java quantized partition: %+v, %v", result, err)
			}
		}
	}
}

func TestKMeansQuantizedErrors(t *testing.T) {
	t.Parallel()
	cfg := defaultGuardiannConfig(3)
	cfg.useRaBitQ = true
	codec, err := newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: []float64{0, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	// The decoded shape is valid, but its encoded distance-estimation payload is truncated.
	bad := gVector{data: []float64{1, 2, 3}, typ: 3, encoded: []byte{3}}
	plain := gVector{data: []float64{4, 5, 6}, typ: 2}
	for _, k := range []int{1, 2} {
		_, err := kMeansFit(&splittableRandom{seed: 11, gamma: goldenGamma}, codec, []gVector{bad, plain}, k, 8, 3)
		var invalid *IllegalArgumentError
		if !errors.As(err, &invalid) {
			t.Fatalf("k=%d lost estimator error: %v", k, err)
		}
	}
	params := defaultPartitionParameters(cfg.metric)
	params.codec = codec
	_, err = evaluatePartition([]gVector{bad}, partition{centroids: []gVector{plain}, assignments: []int{0}}, params)
	var invalid *IllegalArgumentError
	if !errors.As(err, &invalid) {
		t.Fatalf("partition lost estimator error: %v", err)
	}
}

// KMeans.fit's preconditions (KMeans.java:135-139), checked before any vector is
// read: an empty merge core, a candidate with fewer vectors than k and the
// degenerate knobs are each Java's IllegalArgumentException.
func TestKMeansFitPreconditionsAreJavas(t *testing.T) {
	t.Parallel()
	codec := &guardiannVectorCodec{config: guardiannConfig{metric: VectorMetricEuclidean}}
	one := []gVector{{data: []float64{1, 2}, typ: 2}}
	for _, c := range []struct {
		name                    string
		vectors                 []gVector
		k, iterations, restarts int
		want                    string
	}{
		{"k zero", one, 0, 8, 3, "k must be >= 1"},
		{"empty merge core", nil, 1, 8, 3, "vectors.size() must be >= k"},
		{"fewer vectors than k", one, 2, 8, 3, "vectors.size() must be >= k"},
		{"no iterations", one, 1, 0, 3, "maxIterations must be >= 1"},
		{"negative restarts", one, 1, 8, -1, "maxRestarts must be >= 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				_, err = kMeansFit(&splittableRandom{seed: 11, gamma: goldenGamma}, codec, c.vectors, c.k, c.iterations, c.restarts)
			}()
			var iae *IllegalArgumentError
			if !errors.As(err, &iae) || iae.Message != c.want {
				t.Fatalf("err = %v, want IllegalArgumentError %q", err, c.want)
			}
		})
	}
}
