package values

// ConditionSelectorValue evaluates a list of boolean "implication"
// Values in order and returns the 0-based INDEX of the first TRUE
// implication. Returns nil if no implication evaluates TRUE.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ConditionSelectorValue`.
//
// Companion to PickValue: together they implement SQL CASE
// expressions:
//
//	CASE WHEN c1 THEN v1
//	     WHEN c2 THEN v2
//	     ELSE def END
//	  ↓ Cascades planner lowering
//	PickValue(
//	  selector = ConditionSelectorValue(c1, c2, TRUE),
//	  alternatives = [v1, v2, def],
//	  type = inferredType,
//	)
//
// The trailing TRUE implication captures the implicit ELSE — the
// selector returns the def's index when no earlier predicate matches.
//
// Result type: INT (NotNullInt — except when no implication matches,
// where eval returns nil; the type is still INT as a discriminator
// even though the value can be NULL at runtime).
//
// Eval contract:
//   - Walks each implication in source order.
//   - First implication that evaluates TRUE → returns its 0-based
//     index as int64.
//   - All implications FALSE / NULL / non-bool → returns nil.
//
// Per Java's eval, only Boolean.TRUE matches — Boolean.FALSE and
// non-boolean / null results don't trigger the index return.
type ConditionSelectorValue struct {
	Implications []Value
}

// NewConditionSelectorValue constructs the selector with the given
// implication list. Defensive copy of the slice so caller mutations
// don't bleed into Value state.
func NewConditionSelectorValue(implications []Value) *ConditionSelectorValue {
	cp := make([]Value, len(implications))
	copy(cp, implications)
	return &ConditionSelectorValue{Implications: cp}
}

// Children returns the implications list — the only Value children.
func (v *ConditionSelectorValue) Children() []Value { return v.Implications }

// Name returns the SQL function name.
func (*ConditionSelectorValue) Name() string { return "ConditionSelector" }

// Type returns NotNullInt — the selector returns an integer index.
//
// Note: Java's getResultType() returns Type.primitiveType(INT) which
// is the *Java-level* type signature; the eval may return null at
// runtime when no implication matches. The Type accessor is the
// declared type, not the dynamic type. SQL's nullable wrapping
// happens at the consumer (PickValue) when interpreting the
// selector's nil-runtime-result.
func (*ConditionSelectorValue) Type() Type { return NotNullInt }

// Evaluate walks implications in order. Returns the 0-based int64
// index of the first TRUE implication, nil if none match.
//
// Strict-TRUE check: only `bool == true` triggers the index return.
// Boolean.FALSE, NULL, or non-boolean results don't match. Mirrors
// Java's `Boolean.TRUE.equals(result)` strict check.
func (v *ConditionSelectorValue) Evaluate(evalCtx any) any {
	res, err := v.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (v *ConditionSelectorValue) EvaluateErr(evalCtx any) (any, error) {
	for i, impl := range v.Implications {
		if impl == nil {
			continue
		}
		raw, err := impl.EvaluateErr(evalCtx)
		if err != nil {
			return nil, err
		}
		if b, ok := raw.(bool); ok && b {
			return int64(i), nil
		}
	}
	return nil, nil
}

// WithChildren returns a fresh ConditionSelectorValue with the
// given implications substituted. Used by the simplification
// driver when child rewrites land on the implications.
func (v *ConditionSelectorValue) WithChildren(newChildren []Value) *ConditionSelectorValue {
	return NewConditionSelectorValue(newChildren)
}
