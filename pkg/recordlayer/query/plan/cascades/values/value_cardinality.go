package values

// CardinalityValue is the SQL `CARDINALITY` operator: yields the
// number of elements in an array. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// CardinalityValue`.
//
//	CARDINALITY(arr)  ↔  CardinalityValue{Child: arr}
//
// CONFORMANCE: matches Java's eval — returns the array length as
// an integer. NULL input → NULL.
//
// Type is non-null long (CARDINALITY always returns a definite
// count, even 0 for empty arrays).
type CardinalityValue struct {
	Child Value
}

// NewCardinalityValue constructs the operator over the given
// array-typed child Value.
func NewCardinalityValue(child Value) *CardinalityValue {
	return &CardinalityValue{Child: child}
}

// Children returns [Child].
func (v *CardinalityValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*CardinalityValue) Name() string { return "cardinality" }

// Type returns NotNullLong — CARDINALITY always returns a
// definite count.
func (*CardinalityValue) Type() Type { return NotNullLong }

// Evaluate returns the array length (as int64). Returns nil if
// Child is nil-Value or evaluates to nil. Returns nil if the
// Child evaluates to a non-slice (type-degraded UNKNOWN).
func (v *CardinalityValue) Evaluate(evalCtx any) (any, error) {
	if v.Child == nil {
		return nil, nil
	}
	val, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	in, ok := val.([]any)
	if !ok {
		return nil, nil
	}
	return int64(len(in)), nil
}
