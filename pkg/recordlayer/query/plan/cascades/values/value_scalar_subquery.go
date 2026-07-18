package values

import "fmt"

// ScalarSubqueryValue represents a scalar subquery expression
// `(SELECT MAX(v) FROM t2)` in the value tree. The Alias field is
// the correlation identifier for the inner plan — the executor
// pre-runs the inner plan and binds its single scalar result under
// this alias in the evaluation context. Evaluate reads it back.
//
// SQL standard semantics:
//   - Exactly one column (else 42601 syntax error)
//   - At most one row (else 21000 cardinality violation)
//   - Zero rows → NULL
//
// Uncorrelated only — correlated scalar subqueries would require
// per-row re-execution (not in scope).
type ScalarSubqueryValue struct {
	Alias CorrelationIdentifier
	// Typ is the inner plan's single output column type, threaded from
	// SubqueryPlanner.BuildScalar so the plan-time gates (comparison
	// promotion, cast pairs) see the real type — an Unknown-typed scalar
	// subquery evaded every gate a direct column reference hits. nil
	// (underivable inner shape) reports UnknownType, the pre-threading
	// behavior.
	Typ Type
}

// UnboundScalarSubqueryError reports a runtime evaluation of a
// ScalarSubqueryValue whose result was never bound in the evaluation
// context — an orchestration bug in the caller (the plan's scalar
// subqueries must be pre-evaluated and bound via
// EvaluationContext.WithScalarSubqueries before execution). Distinct
// from a subquery that legitimately returned zero rows: that binds a
// PRESENT nil (SQL NULL). Before this error existed, an absent
// binding silently evaluated to NULL and comparisons against it
// vanished rows with no signal.
type UnboundScalarSubqueryError struct {
	Alias CorrelationIdentifier
}

func (e *UnboundScalarSubqueryError) Error() string {
	return fmt.Sprintf("scalar subquery result not bound for alias %s: the executor must pre-evaluate the plan's scalar subqueries and bind them via WithScalarSubqueries before running the plan", e.Alias.Name())
}

func NewScalarSubqueryValue(alias CorrelationIdentifier, typ Type) *ScalarSubqueryValue {
	return &ScalarSubqueryValue{Alias: alias, Typ: typ}
}

func (*ScalarSubqueryValue) Children() []Value { return nil }
func (*ScalarSubqueryValue) Name() string      { return "scalar_subquery" }
func (v *ScalarSubqueryValue) Type() Type {
	if v.Typ != nil {
		return v.Typ
	}
	return UnknownType
}

// Evaluate retrieves the pre-computed scalar subquery result from
// the evaluation context — correct-or-loud:
//
//   - *RowEvalContext with the alias PRESENT: the bound result (nil =
//     the subquery legitimately yielded zero rows → SQL NULL).
//   - *RowEvalContext with the alias ABSENT, or ANY other non-nil
//     context (a raw datum map, a bare scalar row): loud
//     *UnboundScalarSubqueryError. A context that cannot carry the
//     binding is unanswerable at row time — silently answering NULL
//     made every comparison against it UNKNOWN and vanished rows with
//     no signal (the executor's no-bindings filter paths pass the raw
//     Datum map as the row context, so the map arm is a REAL runtime
//     read, not a plan-time probe).
//   - nil: plan-time speculative probe (Comparison.Eval's documented
//     constant-RHS contract; rule fold probes) — stays a non-committal
//     nil. No binding can exist before execution, and subquery values
//     are correlated (GetCorrelatedTo) so compile-time comparison
//     classification defers them anyway.
func (v *ScalarSubqueryValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	if ctx, ok := evalCtx.(*RowEvalContext); ok {
		if val, bound := ctx.ScalarSubqueries[v.Alias]; bound {
			return val, nil
		}
	}
	return nil, &UnboundScalarSubqueryError{Alias: v.Alias}
}

// GetCorrelatedTo returns the alias so the planner knows this value
// depends on the scalar subquery's quantifier.
func (v *ScalarSubqueryValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
