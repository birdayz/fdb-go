package values

// DotProductDistanceRowNumberValue is the dot-product-distance K-NN
// ROW_NUMBER() window function. Assigns unique sequential row
// numbers (1-based) ordered by dot product distance from a reference
// vector. Vectors with larger dot products (more aligned) get lower
// numbers.
//
// Mirrors Java's
// com.apple.foundationdb.record.query.plan.cascades.values.DotProductDistanceRowNumberValue
// — a concrete WindowedValue + IndexOnlyValue subclass.
//
// Dot product distance is the negative dot product: vectors with
// higher dot products (more similar) have smaller distances.
//
// This value is index-only: the row number is computed during HNSW
// index traversal, not from base records.
//
// Result type: NotNullLong.
type DotProductDistanceRowNumberValue struct {
	WindowedValue
}

// NewDotProductDistanceRowNumberValue constructs a dot-product-distance
// row number value. partitioningValues are the PARTITION BY columns;
// argumentValues are the distance arguments (vector field + query
// vector).
func NewDotProductDistanceRowNumberValue(partitioningValues, argumentValues []Value) *DotProductDistanceRowNumberValue {
	return &DotProductDistanceRowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
	}
}

// Name returns the value name matching Java's NAME constant.
func (*DotProductDistanceRowNumberValue) Name() string { return "DotProductDistanceRowNumber" }

// Type returns NotNullLong — ROW_NUMBER is always populated, 1-based.
func (*DotProductDistanceRowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — K-NN row numbers are computed during
// HNSW index traversal and cannot be reproduced from base records.
func (*DotProductDistanceRowNumberValue) IsIndexOnly() bool { return true }

// Evaluate returns the current row number from the row-shape harness
// pattern (_row_number key). Real execution wires the HNSW search
// graph; the harness exposes the per-row counter for testability.
func (*DotProductDistanceRowNumberValue) Evaluate(evalCtx any) (any, error) {
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

// WithChildren returns a fresh DotProductDistanceRowNumberValue with
// children re-split via SplitNewChildren.
func (v *DotProductDistanceRowNumberValue) WithChildren(newChildren []Value) *DotProductDistanceRowNumberValue {
	partition, argument := v.SplitNewChildren(newChildren)
	return NewDotProductDistanceRowNumberValue(partition, argument)
}

var _ IndexOnly = (*DotProductDistanceRowNumberValue)(nil)
