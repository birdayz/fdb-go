package recordlayer

import "math"

// VectorMetric identifies a distance metric for HNSW vector search.
type VectorMetric int

const (
	VectorMetricEuclidean    VectorMetric = iota
	VectorMetricCosine
	VectorMetricInnerProduct
)

// vectorDistance computes the distance between two vectors using the given metric.
func vectorDistance(a, b []float64, metric VectorMetric) float64 {
	switch metric {
	case VectorMetricCosine:
		return cosineDistance(a, b)
	case VectorMetricInnerProduct:
		return innerProductDistance(a, b)
	default:
		return euclideanDistance(a, b)
	}
}

// euclideanDistance computes squared L2 distance.
func euclideanDistance(a, b []float64) float64 {
	sum := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// cosineDistance computes 1 - cosine_similarity.
func cosineDistance(a, b []float64) float64 {
	dot := 0.0
	normA := 0.0
	normB := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 1.0
	}
	return 1.0 - dot/(math.Sqrt(normA)*math.Sqrt(normB))
}

// innerProductDistance computes negative dot product (for maximization).
func innerProductDistance(a, b []float64) float64 {
	dot := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
	}
	return -dot
}
