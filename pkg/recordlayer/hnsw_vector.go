package recordlayer

import "math"

// VectorMetric identifies a distance metric for HNSW vector search.
type VectorMetric int

const (
	VectorMetricEuclidean VectorMetric = iota
	VectorMetricCosine
	VectorMetricInnerProduct
)

// satisfiesPreservedUnderTranslation reports whether distances are preserved
// under vector translation. Matches Java's MetricDefinition.satisfiesPreservedUnderTranslation():
//   - Euclidean: true (default)
//   - Cosine: false (CosineMetric overrides)
//   - InnerProduct (DotProduct): false (DotProductMetric overrides)
//
// When false and RaBitQ is enabled, vectors are rotated immediately from the first
// insert (with zero centroid). When true, centroid bootstrapping is needed first.
func (m VectorMetric) satisfiesPreservedUnderTranslation() bool {
	switch m {
	case VectorMetricCosine, VectorMetricInnerProduct:
		return false
	default:
		return true
	}
}

// satisfiesTriangleInequality reports whether this metric satisfies the triangle inequality.
// Matches Java's MetricDefinition.satisfiesTriangleInequality():
//   - Euclidean (L2, not squared): true (default in Java)
//   - Cosine: false (CosineMetric overrides)
//   - InnerProduct (DotProduct): false (DotProductMetric overrides)
//
// Note: Go's VectorMetricEuclidean actually computes squared L2 (matching Java's
// EuclideanSquareMetric). However, the Java HNSW Config defaults to EUCLIDEAN_METRIC
// (true metric, satisfies triangle inequality), so we return true here to match
// the default behavior users get.
func (m VectorMetric) satisfiesTriangleInequality() bool {
	switch m {
	case VectorMetricCosine, VectorMetricInnerProduct:
		return false
	default:
		return true
	}
}

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
	sim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	// Clamp to [-1, 1] to handle floating-point rounding.
	if sim > 1.0 {
		sim = 1.0
	} else if sim < -1.0 {
		sim = -1.0
	}
	return 1.0 - sim
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
