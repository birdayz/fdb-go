package recordlayer

import "math"

// dot computes the dot product of two vectors.
func dot(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += a[i] * b[i]
	}
	return sum
}

// l2Norm computes the L2 norm of a vector.
func l2Norm(x []float64) float64 {
	return math.Sqrt(dot(x, x))
}
