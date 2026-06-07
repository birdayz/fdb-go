package values

// IndexOnlyAggregateOp enumerates the index-only aggregate operators
// — aggregations that MUST be backed by an aggregate index because
// they can't be evaluated by a streaming aggregator at runtime.
// Mirrors Java's
// `IndexOnlyAggregateValue.PhysicalOperator` (MAX_EVER_LONG /
// MIN_EVER_LONG).
type IndexOnlyAggregateOp int

const (
	// IndexOnlyMaxEverLong is the running-max-since-time-zero
	// aggregate, backed by FDB's MAX_EVER_LONG index. Returns the
	// largest value ever seen across all writes to the indexed
	// column — even if the row has since been deleted.
	IndexOnlyMaxEverLong IndexOnlyAggregateOp = iota
	// IndexOnlyMinEverLong is the corresponding MIN_EVER aggregate.
	IndexOnlyMinEverLong
)

// String returns the canonical operator name (matches Java's
// PhysicalOperator enum names).
func (op IndexOnlyAggregateOp) String() string {
	switch op {
	case IndexOnlyMaxEverLong:
		return "MAX_EVER_LONG"
	case IndexOnlyMinEverLong:
		return "MIN_EVER_LONG"
	}
	return "INVALID"
}

// IndexOnlyAggregateValue represents a compile-time aggregation
// that MUST be backed by an aggregate index — it cannot be
// evaluated by a streaming aggregator at runtime. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.IndexOnlyAggregateValue`.
//
// Java has two abstract subclasses (MaxEverValue, MinEverValue) per
// operator. The Go port unifies via an Op field — same matchability
// pattern as DistanceRowNumberValue.
//
// At plan time, the planner must match this Value against an
// aggregate index of the corresponding type (MAX_EVER_LONG /
// MIN_EVER_LONG). If no matching index exists, the plan fails to
// compile — Java throws SemanticException; Go would surface the
// failure at the rule level (IsIndexOnly() returns true so the
// planner knows to refuse to optimise without an index).
//
// Eval is a placeholder — IndexOnlyAggregateValue is non-evaluable
// by definition (Java's eval throws IllegalStateException; Go's
// surface returns nil per the existing pattern).
//
// Implements the IndexableAggregate interface — GetIndexTypeName
// returns the operator name for index lookup.
type IndexOnlyAggregateValue struct {
	Op    IndexOnlyAggregateOp
	Child Value
}

// NewIndexOnlyAggregateValue constructs a compile-time aggregate
// of the given operator over child.
func NewIndexOnlyAggregateValue(op IndexOnlyAggregateOp, child Value) *IndexOnlyAggregateValue {
	return &IndexOnlyAggregateValue{Op: op, Child: child}
}

// Children returns the single child Value.
func (v *IndexOnlyAggregateValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the operator's canonical name.
func (v *IndexOnlyAggregateValue) Name() string { return v.Op.String() }

// Type returns the child's type — Java's getResultType returns
// child.getResultType() unchanged.
func (v *IndexOnlyAggregateValue) Type() Type {
	if v.Child == nil {
		return UnknownType
	}
	return v.Child.Type()
}

// Evaluate is a placeholder — Java throws IllegalStateException
// since this aggregate is compile-time-only. Go surfaces nil per
// the placeholder pattern.
func (*IndexOnlyAggregateValue) Evaluate(any) (any, error) { return nil, nil }

// IsIndexOnly returns true — this aggregate MUST be backed by an
// index. Planner rules consult this to refuse to optimise without
// a matching index.
func (*IndexOnlyAggregateValue) IsIndexOnly() bool { return true }

// GetIndexTypeName returns the FDB index-type name backing this
// aggregate. Implements the IndexableAggregate interface so
// matchers + planner rules can pick aggregates eligible for index-
// scan lowering.
func (v *IndexOnlyAggregateValue) GetIndexTypeName() string {
	return v.Op.String()
}

// WithChildren returns a fresh IndexOnlyAggregateValue with the
// new child. Op carries through unchanged.
func (v *IndexOnlyAggregateValue) WithChildren(newChildren []Value) *IndexOnlyAggregateValue {
	if len(newChildren) == 0 {
		return NewIndexOnlyAggregateValue(v.Op, nil)
	}
	return NewIndexOnlyAggregateValue(v.Op, newChildren[0])
}

// IsNonEvaluable returns true — IndexOnlyAggregateValue is
// compile-time-only by definition. Implements NonEvaluable.
func (*IndexOnlyAggregateValue) IsNonEvaluable() bool { return true }

var (
	_ IndexableAggregate = (*IndexOnlyAggregateValue)(nil)
	_ NonEvaluable       = (*IndexOnlyAggregateValue)(nil)
)
