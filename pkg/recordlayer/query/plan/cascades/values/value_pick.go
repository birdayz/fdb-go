package values

// PickValue picks one of N alternative Values based on an integer
// selector. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.PickValue`.
//
//	PickValue{Selector: 1, Alternatives: [a, b, c]}.Evaluate(ctx)
//	  ↔  Alternatives[1].Evaluate(ctx) = b.Evaluate(ctx)
//
// Used by the planner for case-style branching where the selector
// is computed at runtime (e.g. switching between alternative
// projection shapes based on row context).
//
// CONFORMANCE: matches Java's eval — Selector evaluates to an
// integer; alternatives[selector] is then evaluated. NULL selector
// → NULL. Out-of-bounds selector returns nil (defensive; Java
// would throw IndexOutOfBoundsException — the seed swallows for
// the row-eval contract).
//
// Type is the bound result type (Java's constructor resolves it
// from the alternatives' types — promotion lattice merge).
type PickValue struct {
	Selector     Value
	Alternatives []Value
	// Typ is the result type — Java resolves from alternative types.
	// Defaults to UnknownType.
	Typ Type
}

// NewPickValue constructs the picker with selector + alternatives
// + result Type.
func NewPickValue(selector Value, alternatives []Value, resultType Type) *PickValue {
	if resultType == nil {
		resultType = UnknownType
	}
	copied := make([]Value, len(alternatives))
	copy(copied, alternatives)
	return &PickValue{
		Selector:     selector,
		Alternatives: copied,
		Typ:          resultType,
	}
}

// Children returns [Selector, alt0, alt1, ...].
//
// Position-stable: nil entries are PRESERVED in the output (not
// filtered) so that the index returned by Selector.Evaluate stays
// aligned with the index into the children list. Filtering nils
// would silently shift later alternatives toward earlier positions
// — Evaluate would then index into a wrong slot. The caller's
// nil-handling lives in Evaluate (a nil-resolved alternative
// returns nil rather than dereferencing).
//
// PickValue intentionally does NOT have a WithChildren method —
// the simplification driver doesn't rebuild PickValues. If a
// future caller needs to rewrite PickValue's children, the
// rebuild must use indexed assignment (NOT a fresh constructor
// over Children()) since Selector + Alternatives are
// position-coupled.
func (v *PickValue) Children() []Value {
	out := make([]Value, 0, 1+len(v.Alternatives))
	if v.Selector != nil {
		out = append(out, v.Selector)
	}
	out = append(out, v.Alternatives...)
	return out
}

// Name returns the debug-print kind.
func (*PickValue) Name() string { return "pick" }

// Type returns the bound result type.
func (v *PickValue) Type() Type { return v.Typ }

// Evaluate computes Alternatives[Selector.Evaluate].
//
// Returns nil if:
//   - Selector is nil-Value or evaluates to nil.
//   - Selector doesn't evaluate to an integer kind.
//   - The resolved index is out of bounds for Alternatives.
//   - The chosen alternative is nil-Value.
func (v *PickValue) Evaluate(evalCtx any) any {
	res, err := v.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (v *PickValue) EvaluateErr(evalCtx any) (any, error) {
	if v.Selector == nil {
		return nil, nil
	}
	idxVal, err := v.Selector.EvaluateErr(evalCtx)
	if err != nil {
		return nil, err
	}
	if idxVal == nil {
		return nil, nil
	}
	var idx int
	switch i := idxVal.(type) {
	case int:
		idx = i
	case int32:
		idx = int(i)
	case int64:
		idx = int(i)
	default:
		return nil, nil
	}
	if idx < 0 || idx >= len(v.Alternatives) {
		return nil, nil
	}
	alt := v.Alternatives[idx]
	if alt == nil {
		return nil, nil
	}
	return alt.EvaluateErr(evalCtx)
}
