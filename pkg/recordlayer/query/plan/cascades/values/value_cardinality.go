package values

// CardinalityValue is the SQL `CARDINALITY` operator: yields the
// number of elements in an array. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// CardinalityValue`.
//
//	CARDINALITY(arr)  ↔  CardinalityValue{Child: arr}
//
// CONFORMANCE: matches Java's eval — returns the array length as
// an integer. NULL array → NULL (Java: childResult == null ? null
// : size()).
//
// The result Type is a nullable 32-bit INT — Java's
// `Type.primitiveType(Type.TypeCode.INT)` ("array indexes and sizes
// are 32-bit integers"), nullable because a NULL array yields NULL.
// The metadata layer reports this column as INTEGER.
//
// Java's ctor asserts the child is array-typed
// (`SemanticException.check(childValue.getResultType().isArray(),
// INCOMPATIBLE_TYPE)`). In Go the array-type validation lives at the
// SQL walk site (expr.walkCardinality), the earliest point with the
// resolved argument Type and access to the SQLSTATE error codes — a
// non-array argument raises CANNOT_CONVERT_TYPE there, matching the
// yamsql. This constructor stays a permissive data builder so the
// tree-rewrite machinery (withChildren) can reconstruct the node
// without re-validating.
type CardinalityValue struct {
	Child Value
}

// NewCardinalityValue constructs the operator over the given
// array-typed child Value. Array-type validation is performed at the
// walk site (see the type doc); this builder does not re-check.
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

// Type returns nullable INT — Java's
// `Type.primitiveType(Type.TypeCode.INT)`. A NULL array makes the
// result NULL, so the type is nullable; the width is 32-bit INT
// (reported as INTEGER), not LONG.
func (*CardinalityValue) Type() Type { return NullableInt }

// Evaluate returns the array length. Mirrors Java's eval:
// childResult == null ? null : ((List)childResult).size(). A NULL
// array (nil child result) yields NULL; an empty array yields 0; a
// populated array yields its element count. Returns int64 (the
// codebase's integer eval representation; the column metadata is
// INTEGER via Type()). A non-slice child result yields nil — Java
// would ClassCastException, but array-type validation at the walk
// site keeps the child array-typed, so this is an unreachable
// defensive guard, not a silent type-degrade.
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
