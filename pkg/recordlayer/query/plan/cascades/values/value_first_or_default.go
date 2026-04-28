package values

// FirstOrDefaultValue returns the first element of an array, OR a
// default value if the array is empty / NULL. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// FirstOrDefaultValue`.
//
//	FIRST_OR_DEFAULT(arr, default)
//	  ↔  FirstOrDefaultValue{Array: arr, Default: default}
//
// Used by the planner for materializing scalar subquery results
// where the subquery may return zero rows.
//
// CONFORMANCE: matches Java's eval semantics:
//   - NULL array → NULL (the default isn't returned for NULL).
//   - Empty array → Default's evaluated value.
//   - Non-empty array → first element.
//
// Type is the array's element type (Java's constructor enforces
// the array.elementType == default.type invariant; the seed
// accepts whatever type the caller provides).
type FirstOrDefaultValue struct {
	Array   Value
	Default Value
	// Typ is the result type — Java's constructor sets it to the
	// array element type. Defaults to UnknownType.
	Typ Type
}

// NewFirstOrDefaultValue constructs the operator.
func NewFirstOrDefaultValue(array, defaultVal Value, resultType Type) *FirstOrDefaultValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &FirstOrDefaultValue{Array: array, Default: defaultVal, Typ: resultType}
}

// Children returns [Array, Default].
func (v *FirstOrDefaultValue) Children() []Value {
	out := make([]Value, 0, 2)
	if v.Array != nil {
		out = append(out, v.Array)
	}
	if v.Default != nil {
		out = append(out, v.Default)
	}
	return out
}

// Name returns the debug-print kind.
func (*FirstOrDefaultValue) Name() string { return "first_or_default" }

// Type returns the bound result type.
func (v *FirstOrDefaultValue) Type() Type { return v.Typ }

// Evaluate returns Array[0] OR Default.Evaluate when Array is empty.
//
// Returns nil if:
//   - Array is nil-Value or evaluates to nil.
//   - Array doesn't evaluate to a slice.
func (v *FirstOrDefaultValue) Evaluate(evalCtx any) any {
	if v.Array == nil {
		return nil
	}
	val := v.Array.Evaluate(evalCtx)
	if val == nil {
		return nil
	}
	in, ok := val.([]any)
	if !ok {
		return nil
	}
	if len(in) == 0 {
		if v.Default == nil {
			return nil
		}
		return v.Default.Evaluate(evalCtx)
	}
	return in[0]
}
