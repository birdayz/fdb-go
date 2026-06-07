package values

// EuclideanSquareDistanceRowNumberValue is the squared-Euclidean-
// distance K-NN ROW_NUMBER() window function. Assigns unique
// sequential row numbers (1-based) ordered by squared Euclidean
// distance from a reference vector. Closer vectors receive lower
// numbers.
//
// Mirrors Java's
// com.apple.foundationdb.record.query.plan.cascades.values.EuclideanSquareDistanceRowNumberValue
// — a concrete WindowedValue + IndexOnlyValue subclass.
//
// Squared Euclidean distance is sum((a_i - b_i)^2) — same ordering
// as L2 without the sqrt cost, making it computationally cheaper for
// nearest-neighbor searches where only relative ordering matters.
//
// This value is index-only: the row number is computed during HNSW
// index traversal, not from base records.
//
// Result type: NotNullLong.
type EuclideanSquareDistanceRowNumberValue struct {
	WindowedValue
}

// NewEuclideanSquareDistanceRowNumberValue constructs a squared-
// Euclidean-distance row number value. partitioningValues are the
// PARTITION BY columns; argumentValues are the distance arguments
// (vector field + query vector).
func NewEuclideanSquareDistanceRowNumberValue(partitioningValues, argumentValues []Value) *EuclideanSquareDistanceRowNumberValue {
	return &EuclideanSquareDistanceRowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
	}
}

// Name returns the value name matching Java's NAME constant.
func (*EuclideanSquareDistanceRowNumberValue) Name() string {
	return "EuclideanSquareDistanceRowNumber"
}

// Type returns NotNullLong — ROW_NUMBER is always populated, 1-based.
func (*EuclideanSquareDistanceRowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — K-NN row numbers are computed during
// HNSW index traversal and cannot be reproduced from base records.
func (*EuclideanSquareDistanceRowNumberValue) IsIndexOnly() bool { return true }

// Evaluate is the error-returning twin (RFC-091).
func (*EuclideanSquareDistanceRowNumberValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if r, ok := m["_row_number"]; ok {
			return r, nil
		}
	}
	return nil, nil
}

// WithChildren returns a fresh EuclideanSquareDistanceRowNumberValue
// with children re-split via SplitNewChildren.
func (v *EuclideanSquareDistanceRowNumberValue) WithChildren(newChildren []Value) *EuclideanSquareDistanceRowNumberValue {
	partition, argument := v.SplitNewChildren(newChildren)
	return NewEuclideanSquareDistanceRowNumberValue(partition, argument)
}

var _ IndexOnly = (*EuclideanSquareDistanceRowNumberValue)(nil)
