package values

import (
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// DistanceOperator enumerates the vector-distance metrics SQL can
// invoke as scalar functions over vector-typed columns. Mirrors
// Java's `DistanceValue.DistanceOperator` verbatim — the SQL
// infix notation matches Java's enum names lowered.
type DistanceOperator int

const (
	// DistanceEuclidean is sqrt(sum((a_i - b_i)^2)) — L2 distance.
	DistanceEuclidean DistanceOperator = iota
	// DistanceEuclideanSquare is sum((a_i - b_i)^2) — squared L2,
	// avoids the sqrt for speed (same ordering as L2 for KNN).
	DistanceEuclideanSquare
	// DistanceCosine is 1 - (a·b)/(|a|·|b|) — angle-based; range [0, 2].
	DistanceCosine
	// DistanceDotProduct is -(a·b) — negated dot product so smaller
	// values = MORE similar (matches the "distance" convention where
	// 0 = identical).
	DistanceDotProduct
)

// String returns the SQL function name (lowercase per Java's
// SQL grammar — `euclidean_distance` etc.).
func (op DistanceOperator) String() string {
	switch op {
	case DistanceEuclidean:
		return "euclidean_distance"
	case DistanceEuclideanSquare:
		return "euclidean_square_distance"
	case DistanceCosine:
		return "cosine_distance"
	case DistanceDotProduct:
		return "dot_product_distance"
	}
	return "INVALID"
}

// DistanceValue computes a distance metric between two vector
// expressions. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.DistanceValue`.
//
// SQL surface:
//
//	WHERE euclidean_distance(embedding, queryVec) < 0.5
//	ORDER BY cosine_distance(vec_field, target) ASC LIMIT 10
//
// Used in similarity-search + nearest-neighbor queries — typically
// paired with HNSW vector indexes via the K-NN query rewrite that
// transforms `ROW_NUMBER() OVER (ORDER BY distance(...)) <= K` into
// a DistanceRankValueComparison + index-backed scan. The distance
// computation itself is a scalar Value evaluated per-row.
//
// Result type: NotNullDouble. All distance metrics produce a
// non-NULL real number when both operands are non-NULL vectors.
//
// Eval contract:
//   - LeftChild + RightChild evaluate to []float64 (vector
//     representation in Go; Java uses RealVector).
//   - Mismatched-length vectors → eval returns nil (type-degraded).
//   - NULL vectors → Java throws RecordCoreException; Go returns nil
//     (the seed surfaces the error as nil per existing pattern;
//     downstream rules can choose to reject earlier at planner level).
//
// Note: eval is functional for `[]float64` operands — vector type
// support is gated on broader infrastructure (Type.Vector + binary
// vector encoding), but the metric math itself works directly on
// double slices for testability + plan-equivalence with Java.
type DistanceValue struct {
	Operator   DistanceOperator
	LeftChild  Value
	RightChild Value
}

// NewDistanceValue constructs a distance computation.
func NewDistanceValue(op DistanceOperator, left, right Value) *DistanceValue {
	return &DistanceValue{Operator: op, LeftChild: left, RightChild: right}
}

// Children returns [left, right].
func (v *DistanceValue) Children() []Value {
	return []Value{v.LeftChild, v.RightChild}
}

// Name returns the SQL function name for this distance metric.
func (v *DistanceValue) Name() string { return v.Operator.String() }

// Type returns NotNullDouble — distance metrics produce non-NULL
// real numbers given non-NULL vector operands.
func (*DistanceValue) Type() Type { return NotNullDouble }

// Evaluate computes the distance metric. Returns nil when either
// operand is NULL or when the operands aren't compatible vectors.
func (v *DistanceValue) Evaluate(evalCtx any) (any, error) {
	if v.LeftChild == nil || v.RightChild == nil {
		return nil, nil
	}
	leftRaw, err := v.LeftChild.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	rightRaw, err := v.RightChild.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	left := asFloat64Slice(leftRaw)
	right := asFloat64Slice(rightRaw)
	if left == nil || right == nil {
		return nil, nil
	}
	if len(left) != len(right) {
		return nil, nil // dimension mismatch — type-degraded
	}
	switch v.Operator {
	case DistanceEuclidean:
		return euclideanDistance(left, right), nil
	case DistanceEuclideanSquare:
		return euclideanSquareDistance(left, right), nil
	case DistanceCosine:
		return cosineDistance(left, right), nil
	case DistanceDotProduct:
		return dotProductDistance(left, right), nil
	}
	return nil, nil
}

// asFloat64Slice converts the supported runtime vector
// representations into []float64 — accepts []float64 directly and
// []float32 (lifted via element-wise float64 conversion).
func asFloat64Slice(v any) []float64 {
	switch s := v.(type) {
	case []float64:
		return s
	case []float32:
		out := make([]float64, len(s))
		for i, f := range s {
			out[i] = float64(f)
		}
		return out
	case []byte:
		// A stored VECTOR column reaches eval as its on-disk bytes (TYPE_BYTES).
		// Decode it to float64 components so a row-by-row distance expression over
		// a stored vector — e.g. SELECT euclidean_distance(embedding, [...]) —
		// computes the real distance instead of silently returning UNKNOWN. The
		// format is self-describing (precision in byte 0). RaBitQ-quantized columns
		// can't be decoded here and yield nil (distance UNKNOWN), as they require
		// the quantizer.
		vec, err := vectorcodec.Deserialize(s)
		if err != nil {
			return nil
		}
		return vec
	}
	return nil
}

// euclideanDistance computes sqrt(sum((a_i - b_i)^2)).
func euclideanDistance(a, b []float64) float64 {
	return math.Sqrt(euclideanSquareDistance(a, b))
}

// euclideanSquareDistance computes sum((a_i - b_i)^2) — same KNN
// ordering as L2 without the sqrt cost.
func euclideanSquareDistance(a, b []float64) float64 {
	var sum float64
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// cosineDistance computes 1 - (a·b)/(|a|·|b|). Range [0, 2]; 0 =
// identical direction, 1 = orthogonal, 2 = opposite.
//
// Returns 1.0 when either |a| or |b| is zero (degenerate — Java's
// metric returns 1.0 in this case too via division-by-zero
// fallback).
func cosineDistance(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 1.0
	}
	return 1.0 - dot/denom
}

// dotProductDistance is -(a·b) — negated so smaller values mean
// MORE similar (matches the "distance" convention).
func dotProductDistance(a, b []float64) float64 {
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	return -dot
}
