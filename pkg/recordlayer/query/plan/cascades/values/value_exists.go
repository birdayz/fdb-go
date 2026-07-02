package values

import "reflect"

// ExistsValue is the Value-layer SQL `EXISTS` operator: yields TRUE
// if a subquery's row stream is non-empty. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ExistsValue`
// (4.12 `c9274172c`), which refactored it from a non-evaluable leaf
// quantifier wrapper into a proper EVALUABLE ValueWithChild.
//
//	EXISTS (SELECT ... FROM t WHERE ...)
//	  ↔  ExistsValue{Value: QuantifiedObjectValue{Correlation: αsubq}}
//
// The child is a *QuantifiedObjectValue over the subquery's
// existential quantifier. EXISTS is true iff that quantifier's object
// (the current row of the subplan) is non-null — i.e. the subplan
// yielded at least one row (Java's `getChild().eval() != null`).
//
// There is ONE EXISTS representation (RFC-141): WHERE-EXISTS is this
// value funnelled through ToQueryPredicate() → ExistentialValuePredicate;
// a projected EXISTS uses the value directly as a column. The standalone
// alias-leaf ExistsPredicate was deleted.
//
// Type is non-null boolean (EXISTS always has a definite truth value —
// even on empty subqueries it returns FALSE).
type ExistsValue struct {
	// Value is the child Value — a *QuantifiedObjectValue over the
	// existential quantifier's object. The correlation is carried by
	// this child, NOT by ExistsValue itself.
	Value Value
}

// NewExistsValue constructs the Value over the existential alias. The
// signature is preserved so callers that pass an alias don't change:
// it wraps the alias in a QuantifiedObjectValue child.
func NewExistsValue(alias CorrelationIdentifier) *ExistsValue {
	return &ExistsValue{Value: NewQuantifiedObjectValue(alias)}
}

// NewExistsValueWithChild constructs the Value over an explicit child
// (a *QuantifiedObjectValue). Mirrors Java's `new ExistsValue(value)`.
func NewExistsValueWithChild(v Value) *ExistsValue {
	return &ExistsValue{Value: v}
}

// GetChild returns the child Value (the existential QuantifiedObjectValue).
// Mirrors Java's ValueWithChild.getChild().
func (v *ExistsValue) GetChild() Value { return v.Value }

// WithNewChild returns a copy of this ExistsValue over a rebased/translated
// child. Mirrors Java's ValueWithChild.withNewChild().
func (v *ExistsValue) WithNewChild(c Value) *ExistsValue {
	return &ExistsValue{Value: c}
}

// Children returns the singleton list containing the child Value —
// ExistsValue is now a transparent composite, so all alias-aware
// walks (correlation, rebase, hash, equals) descend into the child
// QuantifiedObjectValue, which carries the correlation.
func (v *ExistsValue) Children() []Value { return []Value{v.Value} }

// Name returns the debug-print kind.
func (*ExistsValue) Name() string { return "exists" }

// Type returns NotNullBoolean — EXISTS always has a definite
// truth value.
func (*ExistsValue) Type() Type { return NotNullBoolean }

// Evaluate returns whether the child quantifier's object is non-null —
// i.e. the subplan yielded at least one row. Java:
// `getChild().eval(store, context) != null`.
func (v *ExistsValue) Evaluate(ctx any) (any, error) {
	if v.Value == nil {
		return false, nil
	}
	// EXISTS is true iff the existential quantifier's object is BOUND to a non-null row (the
	// subplan yielded ≥1 row). Java's `getChild().eval() != null` works because Java's
	// QuantifiedObjectValue.eval returns null for an unbound quantifier. Go's
	// QuantifiedObjectValue.Evaluate has a single-source `ctx.Datum` fallback shim that returns
	// the OUTER row for an *unbound* existential alias — which would wrongly report TRUE for an
	// empty subquery. So when the child is a quantifier, look up its existential binding
	// DIRECTLY (no outer-row fallback): unbound or null ⇒ FALSE.
	if qov, ok := v.Value.(*QuantifiedObjectValue); ok {
		switch c := ctx.(type) {
		case CorrelationBinder: // *RowEvalContext also satisfies this
			bound, ok := c.GetCorrelationBinding(qov.Correlation)
			// !isNilBinding (not bare `bound != nil`): the binder returns an `any`, so a
			// typed-nil row (e.g. a nil map[string]any boxed into the interface) is non-nil to
			// `!=` and would wrongly report TRUE for an empty subquery.
			return ok && !isNilBinding(bound), nil
		case map[CorrelationIdentifier]map[string]any:
			bound, ok := c[qov.Correlation]
			return ok && bound != nil, nil
		default:
			// No existential binding is reachable in this context shape ⇒ FALSE (never the
			// outer-row fallback).
			return false, nil
		}
	}
	// Non-quantifier child (not produced by the analyzer today) — fall back to Java's contract.
	r, err := v.Value.Evaluate(ctx)
	if err != nil {
		return nil, err
	}
	return r != nil, nil
}

// GetCorrelatedTo delegates to the child — the correlation is carried
// by the child QuantifiedObjectValue, not by ExistsValue.
func (v *ExistsValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return GetCorrelatedToOfValue(v.Value)
}

// isNilBinding reports whether an existential correlation binding is absent — nil, or a
// TYPED nil (e.g. a nil map / slice / pointer boxed into an `any`, which a bare `v != nil`
// would treat as present). Used by ExistsValue.Evaluate so a typed-nil binding correctly
// reads as "no row" (EXISTS ⇒ false) rather than a phantom row.
func isNilBinding(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
