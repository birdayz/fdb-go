package values

import "bytes"

// ArrayDistinctValue is the SQL `ARRAY_DISTINCT` operator: yields
// the input array with duplicate elements removed (preserving the
// first-occurrence order of the original). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// ArrayDistinctValue`.
//
// CONFORMANCE: matches Java's eval — returns the input list with
// `Stream.distinct()` applied (first-seen order). NULL input
// propagates to NULL.
//
// Type matches the Child's array Type (the seed assumes the Child
// produces an array — Java's constructor `Verify.verify(innerResultType.isArray())`
// enforces this; the seed accepts a Value of any Type but the
// Evaluate degrades to nil if Child doesn't return a slice).
type ArrayDistinctValue struct {
	Child Value
	// Typ is the result Type — matches Child's Type for arrays.
	// Defaults to UnknownType if not set.
	Typ Type
}

// NewArrayDistinctValue constructs the operator over the given
// child Value. Type defaults to UnknownType if not provided.
func NewArrayDistinctValue(child Value) *ArrayDistinctValue {
	t := UnknownType
	if child != nil {
		t = child.Type()
	}
	return &ArrayDistinctValue{Child: child, Typ: t}
}

// Children returns [Child].
func (v *ArrayDistinctValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*ArrayDistinctValue) Name() string { return "array_distinct" }

// Type returns the result type (matches Child's type).
func (v *ArrayDistinctValue) Type() Type {
	if v.Typ == nil {
		return UnknownType
	}
	return v.Typ
}

// Evaluate is the error-returning twin (RFC-091).
func (v *ArrayDistinctValue) Evaluate(evalCtx any) (any, error) {
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
	out := make([]any, 0, len(in))
	for _, elem := range in {
		if !arrayContainsByValue(out, elem) {
			out = append(out, elem)
		}
	}
	return out, nil
}

// arrayContainsByValue reports whether `arr` contains `target` by
// value. Uses bytes.Equal for []byte (slices not comparable via ==),
// Go's == for other types.
func arrayContainsByValue(arr []any, target any) bool {
	if tb, ok := target.([]byte); ok {
		for _, e := range arr {
			if eb, ok := e.([]byte); ok && bytes.Equal(eb, tb) {
				return true
			}
		}
		return false
	}
	for _, e := range arr {
		if _, ok := e.([]byte); ok {
			continue // target isn't []byte but element is — not equal
		}
		if e == target {
			return true
		}
	}
	return false
}
