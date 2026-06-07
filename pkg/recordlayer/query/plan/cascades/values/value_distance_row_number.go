package values

// DistanceRowNumberValue is the K-NN search Value: ROW_NUMBER()
// computed within an HNSW vector index traversal, ORDERED BY a
// specific distance metric. Mirrors Java's
// `EuclideanDistanceRowNumberValue` / `EuclideanSquareDistanceRowNumberValue` /
// `CosineDistanceRowNumberValue` / `DotProductDistanceRowNumberValue`
// — Java has FOUR concrete classes, one per metric.
//
// The Go seed UNIFIES the four into a single concrete type with a
// `Metric` field discriminator. The Java distinction matters
// because the K-NN match rule selects on the concrete class type;
// the Go unified design makes K-NN rules switch on Metric instead
// (a one-line `if v.Metric == DistanceCosine`-style check). Both
// expressions are equally matchable; the unified design avoids
// 4× class-per-metric duplication.
//
// Used by the HNSW K-NN search-rewrite rule: when a query of the
// form
//
//	ROW_NUMBER() OVER (PARTITION BY ... ORDER BY <metric>(field, queryVec)) <= K
//
// is detected, the planner rewrites it into a ScanIndex over the
// HNSW index with a DistanceRankValueComparison capturing K +
// queryVec; the resulting plan emits row-numbered candidates from
// the index's graph traversal directly. This Value is the
// post-rewrite shape — its eval is INDEX-ONLY (the ROW_NUMBER value
// is computed during the index search, not from the base record).
//
// Result type: NotNullLong. ROW_NUMBER is always populated, 1-based.
//
// The HNSW config (EfSearch + IsReturningVectors) carries through
// from the higher-order ROW_NUMBER form — same fields as
// RowNumberValue's HNSW knobs.
type DistanceRowNumberValue struct {
	WindowedValue
	Metric             DistanceOperator
	EfSearch           *int
	IsReturningVectors *bool
}

// NewDistanceRowNumberValue constructs a metric-specific row-number
// value. partitioningValues are the OVER PARTITION BY columns;
// argumentValues typically contain the distance arguments (vector
// field + query vector) that the ORDER BY references.
func NewDistanceRowNumberValue(metric DistanceOperator, partitioningValues, argumentValues []Value, efSearch *int, isReturningVectors *bool) *DistanceRowNumberValue {
	return &DistanceRowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
		Metric:             metric,
		EfSearch:           efSearch,
		IsReturningVectors: isReturningVectors,
	}
}

// Name returns a metric-specific function name matching Java's
// per-class naming convention:
//
//	euclidean_distance_row_number
//	euclidean_square_distance_row_number
//	cosine_distance_row_number
//	dot_product_distance_row_number
func (v *DistanceRowNumberValue) Name() string {
	return v.Metric.String() + "_row_number"
}

// Type returns NotNullLong — ROW_NUMBER is always populated.
func (*DistanceRowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — like base RowNumberValue, K-NN
// row-numbers are computed during HNSW index traversal and
// can't be reproduced from base record data alone.
func (*DistanceRowNumberValue) IsIndexOnly() bool { return true }

// Evaluate returns the current row number from the row-shape harness
// pattern (`_row_number` key) — same as base RowNumberValue. Real
// execution wires the HNSW search graph; the harness exposes the
// per-row counter for testability.
func (*DistanceRowNumberValue) Evaluate(evalCtx any) (any, error) {
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

// WithChildren returns a fresh DistanceRowNumberValue with split
// children — partition + argument lists rebuilt via SplitNewChildren,
// metric + HNSW config carry through unchanged.
func (v *DistanceRowNumberValue) WithChildren(newChildren []Value) *DistanceRowNumberValue {
	partition, argument := v.SplitNewChildren(newChildren)
	return NewDistanceRowNumberValue(v.Metric, partition, argument, v.EfSearch, v.IsReturningVectors)
}

var _ IndexOnly = (*DistanceRowNumberValue)(nil)
