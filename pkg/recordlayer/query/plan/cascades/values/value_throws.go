package values

// ThrowsValue is a leaf Value that panics if evaluated. Mirrors
// Java's `com.apple.foundationdb.record.query.plan.cascades.values.
// ThrowsValue`.
//
// Used by the planner for "this code path should be unreachable"
// markers — a Value placeholder that signals a dead branch in the
// plan tree. If a planner bug causes the dead branch to actually
// execute, the panic surfaces immediately rather than silently
// returning nil.
//
// Type is whatever the planner wants the placeholder to advertise
// (so type-checking passes through the dead branch).
type ThrowsValue struct {
	ResultType Type
}

// NewThrowsValue constructs the placeholder with the given Type.
func NewThrowsValue(resultType Type) *ThrowsValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &ThrowsValue{ResultType: resultType}
}

// Children returns the empty slice — leaf.
func (*ThrowsValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*ThrowsValue) Name() string { return "throws" }

// Type returns the bound result type.
func (v *ThrowsValue) Type() Type { return v.ResultType }

// Evaluate panics — ThrowsValue marks an unreachable branch.
// Loud panic message ensures planner bugs that route data through
// the dead branch surface immediately.
func (*ThrowsValue) Evaluate(any) any {
	panic("ThrowsValue.Evaluate: unreachable branch executed — planner bug; this Value marks dead code")
}
