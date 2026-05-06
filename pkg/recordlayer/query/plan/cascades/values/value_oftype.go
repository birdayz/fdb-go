package values

// OfTypeValue is a runtime type guard: tests whether a child Value's
// runtime evaluation matches an expected Type. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.OfTypeValue`.
//
//	OfTypeValue{Child: x, ExpectedType: int}  ↔  "is x a runtime int?"
//
// Used by:
//   - Type-aware rule rewrites that want to gate transformations on a
//     runtime-type assertion (e.g. an arithmetic rule that only fires
//     when both operands are numeric at evaluation time).
//   - The planner's PartialMatch infrastructure (Java) — type guards
//     factor into match-candidate compatibility checks.
//
// Evaluate semantics:
//   - Returns true if the child's evaluated value matches ExpectedType.
//   - Returns nil (UNKNOWN) if the child evaluates to nil — NULL is
//     compatible with any nullable type but the seed treats it as
//     UNKNOWN to be conservative; a follow-up shift can extend the
//     rule when nullable / non-nullable semantics matter.
//   - Returns false otherwise.
//
// Type is always nullable boolean (Kleene-3VL guarded).
//
// The seed implementation is a Type-code match (TypeCodeBoolean ==
// TypeCodeBoolean). It does NOT walk RecordType / ArrayType
// structurally; that's a future extension.
type OfTypeValue struct {
	Child        Value
	ExpectedType Type
}

// NewOfTypeValue constructs the type-guard Value.
func NewOfTypeValue(child Value, expectedType Type) *OfTypeValue {
	return &OfTypeValue{Child: child, ExpectedType: expectedType}
}

// Children returns the single child Value.
func (v *OfTypeValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*OfTypeValue) Name() string { return "oftype" }

// Type is always nullable boolean.
func (*OfTypeValue) Type() Type { return NullableBoolean }

// Evaluate checks the child's runtime value against ExpectedType
// via TypeCode match. Returns nil if either operand is nil-shaped.
//
// The seed compares only TypeCodes — TypeCodeBoolean matches a
// runtime bool, TypeCodeLong matches a runtime int64, etc. Field-
// level structural comparison (e.g. RecordType field-set match) is
// gated on a future extension.
//
// CONFORMANCE: matches Java's OfTypeValue.eval semantics:
//  1. NULL probe → returns ExpectedType.IsNullable().
//  2. Primitive-to-primitive: STRICT TypeCode match (Java's
//     `type.nullable().equals(expectedType.nullable())` reduces
//     to a TypeCode comparison since nullability is normalised
//     on both sides).
//
// Verified against Java's OfTypeValueTest: `OfType(42 (int), LONG)`
// returns FALSE in Java even though INT is promotable to LONG in
// other contexts. The seed matches this strict primitive behavior.
//
// Two Java branches NOT yet replicated:
//   - DynamicMessage probe → returns `expectedType.isRecord()`.
//   - Non-primitive cross-type promotion via PromoteValue.
//     resolvePhysicalOperator (only triggers for non-primitive
//     sources — records, arrays).
//
// Both gated on proto-record-shape introspection / cross-type
// promotion machinery — wired-when-execution-lands.
func (v *OfTypeValue) Evaluate(evalCtx any) any {
	if v.Child == nil || v.ExpectedType == nil {
		return nil
	}
	val := v.Child.Evaluate(evalCtx)
	if val == nil {
		// Java conformance: NULL is "of type T" iff T is nullable.
		return v.ExpectedType.IsNullable()
	}
	// Strict TypeCode match — matches Java's primitive-to-primitive
	// behavior. OfType(42 (int), LONG) returns false (NOT promoted)
	// per OfTypeValueTest.
	if got, ok := runtimeMatchesTypeCode(val, v.ExpectedType.Code()).(bool); ok {
		return got
	}
	return false
}

// runtimeMatchesTypeCode reports whether `val`'s Go runtime type
// matches the given TypeCode. Returns nil for unrecognised codes —
// callers typically interpret nil as UNKNOWN.
//
// Conformance: TypeCodeInt accepts int32 (Java int = 32-bit);
// TypeCodeLong accepts int64 (Java long = 64-bit). Go's
// platform-dependent `int` is treated as int64 on 64-bit builds
// (the FDB target). Strict Java conformance demands these
// distinctions: `OfType(42 (int), LONG)` returns false in Java.
func runtimeMatchesTypeCode(val any, code TypeCode) any {
	switch code {
	case TypeCodeBoolean:
		_, ok := val.(bool)
		return ok
	case TypeCodeInt:
		_, ok := val.(int32)
		return ok
	case TypeCodeLong:
		switch val.(type) {
		case int, int64:
			return true
		default:
			return false
		}
	case TypeCodeFloat:
		_, ok := val.(float32)
		return ok
	case TypeCodeDouble:
		_, ok := val.(float64)
		return ok
	case TypeCodeString:
		_, ok := val.(string)
		return ok
	case TypeCodeBytes:
		_, ok := val.([]byte)
		return ok
	}
	return nil
}
