package values

// SubscriptValue is the Value-layer SQL array subscript: yields
// the element of `Source` at `Index`. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.SubscriptValue`.
//
//	arr[1]  ↔  SubscriptValue{Source: arr, Index: 1}
//
// CONFORMANCE: Index is 1-BASED per SQL standard (Foundation
// Section 4.10.2). Java's eval explicitly says: "If n is the
// cardinality of A, then the ordinal position p of an element is
// an integer in the range 1 ≤ p ≤ n."
//
// Out-of-bounds Index returns NULL (UNKNOWN) — Java DOES NOT
// raise an out-of-bound error, matches SQL semantics.
//
// NULL propagation: NULL Index OR NULL Source → NULL result.
type SubscriptValue struct {
	Source Value
	Index  Value
	// Typ is the bound element type. Defaults to UnknownType.
	Typ Type
}

// NewSubscriptValue constructs the subscript Value with the given
// source array Value, index Value, and result Type.
func NewSubscriptValue(source, index Value, resultType Type) *SubscriptValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &SubscriptValue{Source: source, Index: index, Typ: resultType}
}

// Children returns [Source, Index].
func (v *SubscriptValue) Children() []Value {
	out := make([]Value, 0, 2)
	if v.Source != nil {
		out = append(out, v.Source)
	}
	if v.Index != nil {
		out = append(out, v.Index)
	}
	return out
}

// Name returns the debug-print kind.
func (*SubscriptValue) Name() string { return "subscript" }

// Type returns the bound element type.
func (v *SubscriptValue) Type() Type { return v.Typ }

// Evaluate returns Source[Index-1] (1-based per SQL standard).
//
// Returns nil (UNKNOWN) if:
//   - Source or Index is nil-Value or evaluates to nil
//   - Source doesn't evaluate to a slice ([]any)
//   - Index isn't an integer kind
//   - Index is out of bounds
func (v *SubscriptValue) Evaluate(evalCtx any) any {
	res, err := v.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (v *SubscriptValue) EvaluateErr(evalCtx any) (any, error) {
	if v.Source == nil || v.Index == nil {
		return nil, nil
	}
	indexVal, err := v.Index.EvaluateErr(evalCtx)
	if err != nil {
		return nil, err
	}
	if indexVal == nil {
		return nil, nil
	}
	sourceVal, err := v.Source.EvaluateErr(evalCtx)
	if err != nil {
		return nil, err
	}
	if sourceVal == nil {
		return nil, nil
	}
	sourceList, ok := sourceVal.([]any)
	if !ok {
		return nil, nil
	}
	// Index must be an integer-kind. SQL standard: 1-based.
	var idx int
	switch i := indexVal.(type) {
	case int:
		idx = i
	case int32:
		idx = int(i)
	case int64:
		idx = int(i)
	default:
		return nil, nil
	}
	adjusted := idx - 1
	if adjusted < 0 || adjusted >= len(sourceList) {
		// Java conformance: out-of-bounds returns NULL, doesn't error.
		return nil, nil
	}
	return sourceList[adjusted], nil
}
