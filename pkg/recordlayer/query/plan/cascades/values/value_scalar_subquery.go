package values

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
}

func NewScalarSubqueryValue(alias CorrelationIdentifier) *ScalarSubqueryValue {
	return &ScalarSubqueryValue{Alias: alias}
}

func (*ScalarSubqueryValue) Children() []Value { return nil }
func (*ScalarSubqueryValue) Name() string      { return "scalar_subquery" }
func (*ScalarSubqueryValue) Type() Type        { return UnknownType }

// Evaluate retrieves the pre-computed scalar subquery result from
// the evaluation context. The executor stores scalar subquery results
// under a dedicated ScalarSubqueryBinding key.
func (v *ScalarSubqueryValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	switch ctx := evalCtx.(type) {
	case *RowEvalContext:
		if ctx.ScalarSubqueries != nil {
			return ctx.ScalarSubqueries[v.Alias], nil
		}
	case map[string]any:
		// For simple eval contexts, scalar subqueries are not available.
		return nil, nil
	}
	return nil, nil
}

// GetCorrelatedTo returns the alias so the planner knows this value
// depends on the scalar subquery's quantifier.
func (v *ScalarSubqueryValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
