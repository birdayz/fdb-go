package values

// RowNumberHighOrderValue is a partially-applied ROW_NUMBER() window
// function — a curried form that carries the optional HNSW
// configuration parameters (EfSearch + IsReturningVectors) ahead of
// the actual partition + argument values. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.RowNumberHighOrderValue`.
//
// Usage flow (matches Java's higher-order resolution):
//
//  1. Parser encounters `ROW_NUMBER(ef_search: 100)` — this
//     constructs a RowNumberHighOrderValue with the configuration
//     baked in, but no partition or argument values yet.
//  2. Higher-order Apply receives the partition + argument values
//     (typically the OVER clause's PARTITION BY + ORDER BY columns).
//  3. Apply produces a fully-configured RowNumberValue with all
//     four pieces (partition, argument, ef_search, is_returning_vectors).
//
// The high-order pattern lets configuration parameters be specified
// separately from the window specification — Java's `OPTIONS` clause
// supports `ROW_NUMBER(ef_search: 100) OVER (PARTITION BY ...)`.
//
// LEAF Value: takes no children. The carried HNSW configuration is
// not Value-typed — it's static metadata.
//
// Eval is a placeholder — the real Apply path lands when the parser
// + higher-order resolution machinery is in place. RowNumberHighOrderValue
// itself never produces row-numbered output; it produces a RowNumberValue
// when applied.
type RowNumberHighOrderValue struct {
	EfSearch           *int
	IsReturningVectors *bool
}

// NewRowNumberHighOrderValue constructs the curried form. Both
// configuration parameters are optional — nil means "use HNSW
// index defaults".
func NewRowNumberHighOrderValue(efSearch *int, isReturningVectors *bool) *RowNumberHighOrderValue {
	return &RowNumberHighOrderValue{
		EfSearch:           efSearch,
		IsReturningVectors: isReturningVectors,
	}
}

// Children returns the empty slice — leaf Value.
func (*RowNumberHighOrderValue) Children() []Value { return []Value{} }

// Name returns the canonical higher-order function name.
func (*RowNumberHighOrderValue) Name() string { return "ROW_NUMBER_HIGH_ORDER" }

// Type returns UnknownType — high-order values don't have a direct
// runtime type until applied. Java's getResultType() inherits from
// the high-order superclass which doesn't pin a type.
func (*RowNumberHighOrderValue) Type() Type { return UnknownType }

// Evaluate is the error-returning twin (RFC-091). High-order values
// have no per-row eval; the placeholder never fails.
func (*RowNumberHighOrderValue) Evaluate(any) (any, error) { return nil, nil }

// Apply produces a fully-configured RowNumberValue from this
// curried form by attaching the partition + argument values.
//
// The Java equivalent is `evalWithoutStore` returning a
// BuiltInFunction that, when called with partition+argument
// arguments, allocates the final RowNumberValue. Go expresses this
// directly with a method.
//
// Configuration carries through verbatim — EfSearch + IsReturningVectors
// land in the resulting RowNumberValue's HNSW knobs.
func (h *RowNumberHighOrderValue) Apply(partitioningValues, argumentValues []Value) *RowNumberValue {
	return NewRowNumberValue(partitioningValues, argumentValues, h.EfSearch, h.IsReturningVectors)
}
