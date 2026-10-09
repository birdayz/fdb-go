package recordlayer

import (
	"math/rand"
	"testing"
)

// --- Test helper functions used across test files ---

func randomVector(rng *rand.Rand, dims int) []float64 {
	v := make([]float64, dims)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	return v
}

func normalizeVector(v []float64) []float64 {
	n := l2Norm(v)
	if n == 0 {
		return v
	}
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] / n
	}
	return out
}

func exactEuclideanSquare(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// --- Tests ---

func TestDeserializeVectorRejectsRABITQ(t *testing.T) {
	t.Parallel()

	// Type ordinal 3 should be rejected by deserializeVector.
	data := make([]byte, 30)
	data[0] = 3
	_, err := deserializeVector(data)
	if err == nil {
		t.Fatal("expected error for RABITQ type in deserializeVector")
	}
}
