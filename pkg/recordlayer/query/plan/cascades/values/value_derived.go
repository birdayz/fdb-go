package values

// DerivedValue is a placeholder Value that wraps a list of children
// without computing a result. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.DerivedValue`.
//
// Used by:
//   - The planner during plan rewrites that need to track child
//     dependencies but don't yet know how to compute a result.
//   - Match-candidate compatibility checks that consult the
//     derived-from-which-children property.
//
// DerivedValue is "non-evaluable" — Evaluate panics. Pattern-match
// against it; don't try to run it at the per-row level.
type DerivedValue struct {
	ChildrenList []Value
	ResultType   Type
}

// NewDerivedValue constructs a DerivedValue with UnknownType.
func NewDerivedValue(children []Value) *DerivedValue {
	return &DerivedValue{
		ChildrenList: append([]Value(nil), children...),
		ResultType:   UnknownType,
	}
}

// NewDerivedValueWithType constructs a DerivedValue with the given
// result Type.
func NewDerivedValueWithType(children []Value, resultType Type) *DerivedValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &DerivedValue{
		ChildrenList: append([]Value(nil), children...),
		ResultType:   resultType,
	}
}

// Children returns the wrapped children.
func (v *DerivedValue) Children() []Value { return v.ChildrenList }

// Name returns the debug-print kind.
func (*DerivedValue) Name() string { return "derived" }

// Type returns the bound result type.
func (v *DerivedValue) Type() Type { return v.ResultType }

// Evaluate panics — DerivedValue is non-evaluable. Pattern-match
// at the planner level instead.
func (*DerivedValue) Evaluate(any) any {
	panic("DerivedValue.Evaluate: derived-value placeholder is non-evaluable")
}

// EvaluateErr is the error-returning twin (RFC-091). DerivedValue is
// non-evaluable — a genuine invariant violation, so it stays a panic.
func (*DerivedValue) EvaluateErr(any) (any, error) {
	panic("DerivedValue.EvaluateErr: derived-value placeholder is non-evaluable")
}
