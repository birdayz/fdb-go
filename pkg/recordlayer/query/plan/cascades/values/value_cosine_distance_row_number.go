package values

// CosineDistanceRowNumberValue is the cosine-distance K-NN
// ROW_NUMBER() window function. Assigns unique sequential row
// numbers (1-based) ordered by cosine distance from a reference
// vector. More similar vectors (smaller cosine distance) get lower
// numbers.
//
// Mirrors Java's
// com.apple.foundationdb.record.query.plan.cascades.values.CosineDistanceRowNumberValue
// — a concrete WindowedValue + IndexOnlyValue subclass.
//
// Cosine distance measures angular difference: 0 = identical
// direction, 1 = orthogonal, 2 = opposite.
//
// This value is index-only: the row number is computed during HNSW
// index traversal, not from base records.
//
// Result type: NotNullLong.
type CosineDistanceRowNumberValue struct {
	WindowedValue
}

// NewCosineDistanceRowNumberValue constructs a cosine-distance row
// number value. partitioningValues are the PARTITION BY columns;
// argumentValues are the distance arguments (vector field + query
// vector).
func NewCosineDistanceRowNumberValue(partitioningValues, argumentValues []Value) *CosineDistanceRowNumberValue {
	return &CosineDistanceRowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
	}
}

// Name returns the value name matching Java's NAME constant.
func (*CosineDistanceRowNumberValue) Name() string { return "CosineDistanceRowNumber" }

// Type returns NotNullLong — ROW_NUMBER is always populated, 1-based.
func (*CosineDistanceRowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — K-NN row numbers are computed during
// HNSW index traversal and cannot be reproduced from base records.
func (*CosineDistanceRowNumberValue) IsIndexOnly() bool { return true }

// Evaluate returns the current row number from the row-shape harness
// pattern (_row_number key). Real execution wires the HNSW search
// graph; the harness exposes the per-row counter for testability.
func (*CosineDistanceRowNumberValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if r, ok := m["_row_number"]; ok {
			return r
		}
	}
	return nil
}

// WithChildren returns a fresh CosineDistanceRowNumberValue with
// children re-split via SplitNewChildren.
func (v *CosineDistanceRowNumberValue) WithChildren(newChildren []Value) *CosineDistanceRowNumberValue {
	partition, argument := v.SplitNewChildren(newChildren)
	return NewCosineDistanceRowNumberValue(partition, argument)
}

var _ IndexOnly = (*CosineDistanceRowNumberValue)(nil)
