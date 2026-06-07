package values

// ObjectValue is a generic typed-object placeholder bound to a
// CorrelationIdentifier. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ObjectValue`.
//
// Used by Java to represent "any object" in expression contexts —
// generic counterpart to QuantifiedObjectValue (which specifically
// represents a Quantifier's flowed object). ObjectValue is more
// general: used in non-quantifier contexts where the planner needs
// a typed placeholder bound to a specific alias.
//
// Type is whatever the planner determined at capture time.
//
// Non-evaluable: ObjectValue is a placeholder; specialized
// evaluation paths (quantifier dereferencing, etc.) handle it
// before reaching the per-row Eval contract. The seed Evaluate
// returns nil to make the no-row-eval contract explicit.
type ObjectValue struct {
	Alias      CorrelationIdentifier
	ResultType Type
}

// NewObjectValue constructs a typed object placeholder bound to
// the given alias.
func NewObjectValue(alias CorrelationIdentifier, resultType Type) *ObjectValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &ObjectValue{Alias: alias, ResultType: resultType}
}

// Children returns the empty slice — leaf.
func (*ObjectValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*ObjectValue) Name() string { return "object" }

// Type returns the bound result type.
func (v *ObjectValue) Type() Type { return v.ResultType }

// Evaluate returns nil — ObjectValue is a placeholder. Specialized
// evaluation paths handle it before reaching per-row Eval.
func (*ObjectValue) Evaluate(any) any { return nil }

// EvaluateErr is the error-returning twin (RFC-091). Placeholder eval
// never fails.
func (*ObjectValue) EvaluateErr(any) (any, error) { return nil, nil }

// GetCorrelatedTo returns the singleton set containing the bound
// alias.
func (v *ObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
