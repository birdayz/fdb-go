package recordlayer

import (
	"encoding/binary"
	"math"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// vectorDistanceFromBytes computes the metric distance between a query vector
// and a stored vector, reading the stored components straight from their
// on-disk IEEE-754 bytes — no intermediate []float64 is materialized. This is
// the search hot path: every visited node's distance flows through here, and
// the []float64 allocation it replaces was ~31% of all search-path allocation.
//
// It returns ok=false for encodings it does not fast-path (RaBitQ), so the
// caller can fall back to deserialize + vectorDistance. Results are bit-for-bit
// identical to deserializeVector followed by vectorDistance.
func vectorDistanceFromBytes(query []float64, stored []byte, metric VectorMetric) (float64, bool) {
	typeOrd, payload, stride, ok := vectorcodec.Payload(stored)
	if !ok {
		return 0, false
	}
	n := len(payload) / stride
	if len(query) < n {
		n = len(query)
	}

	// DOUBLE is the common case (Serialize writes DOUBLE); keep it a tight,
	// call-free loop per metric.
	if typeOrd == vectorcodec.TypeDouble {
		switch metric {
		case VectorMetricCosine:
			var dot, na, nb float64
			for j := 0; j < n; j++ {
				q := query[j]
				v := math.Float64frombits(binary.BigEndian.Uint64(payload[j*8:]))
				dot += q * v
				na += q * q
				nb += v * v
			}
			return cosineFromAccums(dot, na, nb), true
		case VectorMetricInnerProduct:
			var dot float64
			for j := 0; j < n; j++ {
				dot += query[j] * math.Float64frombits(binary.BigEndian.Uint64(payload[j*8:]))
			}
			return -dot, true
		default: // Euclidean (sqrt) or EuclideanSquare (no sqrt) — four independent lanes.
			var s0, s1, s2, s3 float64
			j := 0
			for ; j+4 <= n; j += 4 {
				d0 := query[j] - math.Float64frombits(binary.BigEndian.Uint64(payload[j*8:]))
				d1 := query[j+1] - math.Float64frombits(binary.BigEndian.Uint64(payload[j*8+8:]))
				d2 := query[j+2] - math.Float64frombits(binary.BigEndian.Uint64(payload[j*8+16:]))
				d3 := query[j+3] - math.Float64frombits(binary.BigEndian.Uint64(payload[j*8+24:]))
				s0 += d0 * d0
				s1 += d1 * d1
				s2 += d2 * d2
				s3 += d3 * d3
			}
			sum := s0 + s1 + s2 + s3
			for ; j < n; j++ {
				d := query[j] - math.Float64frombits(binary.BigEndian.Uint64(payload[j*8:]))
				sum += d * d
			}
			if metric == VectorMetricEuclideanSquare {
				return sum, true
			}
			return math.Sqrt(sum), true
		}
	}

	// SINGLE / HALF: still allocation-free, via a per-component reader.
	read := func(j int) float64 {
		off := j * stride
		if typeOrd == vectorcodec.TypeSingle {
			return float64(math.Float32frombits(binary.BigEndian.Uint32(payload[off:])))
		}
		return float64(vectorcodec.HalfToFloat32(binary.BigEndian.Uint16(payload[off:])))
	}
	switch metric {
	case VectorMetricCosine:
		var dot, na, nb float64
		for j := 0; j < n; j++ {
			q := query[j]
			v := read(j)
			dot += q * v
			na += q * q
			nb += v * v
		}
		return cosineFromAccums(dot, na, nb), true
	case VectorMetricInnerProduct:
		var dot float64
		for j := 0; j < n; j++ {
			dot += query[j] * read(j)
		}
		return -dot, true
	default: // Euclidean (sqrt) or EuclideanSquare (no sqrt)
		var sum float64
		for j := 0; j < n; j++ {
			d := query[j] - read(j)
			sum += d * d
		}
		if metric == VectorMetricEuclideanSquare {
			return sum, true
		}
		return math.Sqrt(sum), true
	}
}

// cosineFromAccums finishes the cosine-distance computation from the running
// dot product and squared norms, in Java's CosineMetric order: a zero norm is
// +Inf, then a non-finite norm or dot product is NaN; no clamp.
func cosineFromAccums(dot, normA, normB float64) float64 {
	if normA == 0 || normB == 0 {
		return math.Inf(1)
	}
	if math.IsInf(normA, 0) || math.IsNaN(normA) || math.IsInf(normB, 0) || math.IsNaN(normB) ||
		math.IsInf(dot, 0) || math.IsNaN(dot) {
		return math.NaN()
	}
	return 1.0 - dot/(math.Sqrt(normA)*math.Sqrt(normB))
}
