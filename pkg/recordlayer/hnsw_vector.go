package recordlayer

import "math"

// VectorMetric identifies a distance metric for HNSW vector search.
type VectorMetric int

const (
	// VectorMetricEuclidean is true L2: sqrt(Σ(a_i−b_i)²). Matches Java's
	// EUCLIDEAN_METRIC (MetricDefinition.EuclideanMetric.distance = Math.sqrt(...)).
	VectorMetricEuclidean VectorMetric = iota
	VectorMetricCosine
	VectorMetricInnerProduct
	// VectorMetricEuclideanSquare is squared L2: Σ(a_i−b_i)² (no sqrt). Matches
	// Java's EUCLIDEAN_SQUARE_METRIC — same KNN ordering as L2, cheaper, but it is
	// NOT a true metric (does not satisfy the triangle inequality). Added at the end
	// so the existing iota values are unchanged (the int is never persisted; configs
	// round-trip via the option string, e.g. "EUCLIDEAN_SQUARE_METRIC").
	VectorMetricEuclideanSquare
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
//   - Euclidean (true L2, sqrt): true (default in Java)
//   - EuclideanSquare: false (EuclideanSquareMetric overrides — squared L2 is not a true metric)
//   - Cosine: false (CosineMetric overrides)
//   - InnerProduct (DotProduct): false (DotProductMetric overrides)
func (m VectorMetric) satisfiesTriangleInequality() bool {
	switch m {
	case VectorMetricCosine, VectorMetricInnerProduct, VectorMetricEuclideanSquare:
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
	case VectorMetricEuclideanSquare:
		return euclideanSquareDistance(a, b)
	default:
		return euclideanDistance(a, b)
	}
}

// euclideanDistance computes true L2 distance: sqrt(Σ(a_i−b_i)²). Matches Java's
// EuclideanMetric.distance = Math.sqrt(EuclideanSquareMetric.distanceInternal(...)).
// sqrt is monotone, so the graph structure (built on distance comparisons) is
// identical to a squared-based build — only the reported distance value differs.
// For 1536-D vectors the single sqrt is ~1% over the Σ, so this is not a hot-path
// regression; the squared variant (euclideanSquareDistance) serves the
// EUCLIDEAN_SQUARE metric where a caller opts out of the sqrt.
func euclideanDistance(a, b []float64) float64 {
	return math.Sqrt(euclideanSquareDistance(a, b))
}

// euclideanSquareDistance computes squared L2 distance Σ(a_i−b_i)². The accumulation
// is unrolled into four independent sums to break the serial dependency of
// `sum += d*d` — modern CPUs have several FP units that otherwise sit idle waiting
// on the previous add. Distance is ~half of all HNSW CPU (search and insert), so
// this is load-bearing.
func euclideanSquareDistance(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s0, s1, s2, s3 float64
	i := 0
	for ; i+4 <= n; i += 4 {
		d0 := a[i] - b[i]
		d1 := a[i+1] - b[i+1]
		d2 := a[i+2] - b[i+2]
		d3 := a[i+3] - b[i+3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	sum := s0 + s1 + s2 + s3
	for ; i < n; i++ {
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
