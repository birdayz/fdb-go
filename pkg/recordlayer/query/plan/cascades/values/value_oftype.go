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
//  2. Primitive TypeCode match (direct).
//  3. Cross-type promotion via IsPromotable — Java accepts a
//     value if it can be coerced to ExpectedType via the
//     promotion lattice (e.g. INT → LONG, INT → DOUBLE).
//
// One Java branch NOT yet replicated:
//   - DynamicMessage probe → returns `expectedType.isRecord()`.
//     Seed reports false for unknown shapes (Go doesn't have a
//     proto DynamicMessage equivalent in this layer; gated on
//     proto-record-shape introspection).
func (v *OfTypeValue) Evaluate(evalCtx any) any {
	if v.Child == nil || v.ExpectedType == nil {
		return nil
	}
	val := v.Child.Evaluate(evalCtx)
	if val == nil {
		// Java conformance: NULL is "of type T" iff T is nullable.
		return v.ExpectedType.IsNullable()
	}
	// Direct TypeCode match.
	if got, ok := runtimeMatchesTypeCode(val, v.ExpectedType.Code()).(bool); ok && got {
		return true
	}
	// Cross-type promotion: Java accepts the value if its runtime
	// type can be promoted to ExpectedType. Walk the runtime type
	// for each primitive TypeCode and check IsPromotable.
	for code := TypeCodeBoolean; code <= TypeCodeBytes; code++ {
		if got, ok := runtimeMatchesTypeCode(val, code).(bool); ok && got {
			from := NewPrimitiveType(code, false)
			if IsPromotable(from, v.ExpectedType) {
				return true
			}
			return false
		}
	}
	return false
}

// runtimeMatchesTypeCode reports whether `val`'s Go runtime type
// matches the given TypeCode. Returns nil for unrecognised codes —
// callers typically interpret nil as UNKNOWN.
func runtimeMatchesTypeCode(val any, code TypeCode) any {
	switch code {
	case TypeCodeBoolean:
		_, ok := val.(bool)
		return ok
	case TypeCodeLong:
		// Accept all int kinds at runtime — Go's untyped int
		// constants lower to int64 in most paths.
		switch val.(type) {
		case int, int32, int64:
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
