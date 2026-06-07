package values

// EuclideanDistanceRowNumberValue is the Euclidean-distance K-NN
// ROW_NUMBER() window function. Assigns unique sequential row
// numbers (1-based) ordered by Euclidean distance from a reference
// vector. Closer vectors receive lower numbers.
//
// Mirrors Java's
// com.apple.foundationdb.record.query.plan.cascades.values.EuclideanDistanceRowNumberValue
// — a concrete WindowedValue + IndexOnlyValue subclass.
//
// Euclidean distance is sqrt(sum((a_i - b_i)^2)) — the standard L2
// distance.
//
// This value is index-only: the row number is computed during HNSW
// index traversal, not from base records.
//
// Result type: NotNullLong.
type EuclideanDistanceRowNumberValue struct {
	WindowedValue
}

// NewEuclideanDistanceRowNumberValue constructs a Euclidean-distance
// row number value. partitioningValues are the PARTITION BY columns;
// argumentValues are the distance arguments (vector field + query
// vector).
func NewEuclideanDistanceRowNumberValue(partitioningValues, argumentValues []Value) *EuclideanDistanceRowNumberValue {
	return &EuclideanDistanceRowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
	}
}

// Name returns the value name matching Java's NAME constant.
func (*EuclideanDistanceRowNumberValue) Name() string { return "EuclideanDistanceRowNumber" }

// Type returns NotNullLong — ROW_NUMBER is always populated, 1-based.
func (*EuclideanDistanceRowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — K-NN row numbers are computed during
// HNSW index traversal and cannot be reproduced from base records.
func (*EuclideanDistanceRowNumberValue) IsIndexOnly() bool { return true }

// Evaluate returns the current row number from the row-shape harness
// pattern (_row_number key). Real execution wires the HNSW search
// graph; the harness exposes the per-row counter for testability.
func (*EuclideanDistanceRowNumberValue) Evaluate(evalCtx any) (any, error) {
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

// WithChildren returns a fresh EuclideanDistanceRowNumberValue with
// children re-split via SplitNewChildren.
func (v *EuclideanDistanceRowNumberValue) WithChildren(newChildren []Value) *EuclideanDistanceRowNumberValue {
	partition, argument := v.SplitNewChildren(newChildren)
	return NewEuclideanDistanceRowNumberValue(partition, argument)
}

var _ IndexOnly = (*EuclideanDistanceRowNumberValue)(nil)
