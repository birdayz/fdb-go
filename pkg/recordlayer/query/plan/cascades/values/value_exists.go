package values

// ExistsValue is the Value-layer SQL `EXISTS` operator: yields TRUE
// if a subquery's row stream is non-empty. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ExistsValue`.
//
//	EXISTS (SELECT ... FROM t WHERE ...)
//	  ↔  ExistsValue{Alias: αsubq}
//
// Like the QueryPredicate-layer ExistsPredicate (in the predicates
// package), the subquery is referenced indirectly via an
// existential alias — a CorrelationIdentifier ranging over the
// subquery's Reference. The pair (ExistsValue, ExistsPredicate)
// mirrors Java's two-layer approach: ExistsValue lives in
// projection / SELECT-list contexts; ExistsPredicate lives in
// WHERE / filter contexts.
//
// Type is non-null boolean (EXISTS always has a definite truth
// value — even on empty subqueries it returns FALSE).
//
// Non-evaluable: the seed's per-row Eval contract can't run a
// subquery; specialized planner rules handle EXISTS lowering.
// Evaluate panics loudly if mistakenly invoked.
type ExistsValue struct {
	Alias CorrelationIdentifier
}

// NewExistsValue constructs the Value with the given existential
// alias.
func NewExistsValue(alias CorrelationIdentifier) *ExistsValue {
	return &ExistsValue{Alias: alias}
}

// Children returns the empty slice — leaf.
func (*ExistsValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*ExistsValue) Name() string { return "exists" }

// Type returns NotNullBoolean — EXISTS always has a definite
// truth value.
func (*ExistsValue) Type() Type { return NotNullBoolean }

// Evaluate panics — ExistsValue is non-evaluable at the per-row
// level. Specialized planner rules / executor handling does the
// row-level test.
func (*ExistsValue) Evaluate(any) any {
	panic("ExistsValue.Evaluate: non-evaluable; specialized planner rule handles EXISTS lowering")
}

// EvaluateErr is the error-returning twin (RFC-091). ExistsValue is
// non-evaluable at the per-row level — a genuine invariant violation,
// so it stays a panic.
func (*ExistsValue) EvaluateErr(any) (any, error) {
	panic("ExistsValue.EvaluateErr: non-evaluable; specialized planner rule handles EXISTS lowering")
}

// GetCorrelatedTo returns the singleton set containing the
// existential alias.
func (v *ExistsValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
