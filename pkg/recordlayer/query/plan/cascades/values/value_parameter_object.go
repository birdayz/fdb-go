package values

// ParameterObjectValue represents a plan-cache parameter binding —
// a named placeholder whose value is supplied at execution time via
// the EvaluationContext. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.ParameterObjectValue`.
//
// Key fields:
//   - ParameterName: the parameter name (Java: parameterAlias stored
//     as a string, not a CorrelationIdentifier).
//   - ResultType: the declared Type of the parameter.
//
// Evaluate returns the parameter's value from the eval context's
// ParameterBinder capability, or nil when no binding exists.
//
// Not correlated: ParameterObjectValue's getCorrelatedToWithoutChildren()
// returns the empty set in Java — parameter names are NOT
// CorrelationIdentifiers. The parameter's runtime value is resolved
// from the EvaluationContext, not from a quantifier binding.
type ParameterObjectValue struct {
	ParameterName string
	ResultType    Type
}

// NewParameterObjectValue constructs a ParameterObjectValue.
func NewParameterObjectValue(parameterName string, resultType Type) *ParameterObjectValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &ParameterObjectValue{
		ParameterName: parameterName,
		ResultType:    resultType,
	}
}

// Children returns the empty slice — leaf.
func (*ParameterObjectValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*ParameterObjectValue) Name() string { return "paramobj" }

// Type returns the declared result type. Parameter bindings can be
// NULL, so the result is forced to nullable.
func (v *ParameterObjectValue) Type() Type {
	if v.ResultType == nil {
		return UnknownType
	}
	return WithNullability(v.ResultType, true)
}

// Evaluate returns the parameter's value from the eval context's
// ParameterBinder capability. Returns nil when the context doesn't
// implement ParameterBinder or when no binding exists.
//
// Mirrors Java's ParameterObjectValue.eval which calls
// context.getBinding(parameterName).
func (v *ParameterObjectValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	if b, ok := evalCtx.(ParameterBinder); ok {
		val, _ := b.BindParameter(0, v.ParameterName)
		return val
	}
	return nil
}

// GetCorrelatedTo returns the empty set — parameter names are NOT
// CorrelationIdentifiers. Matches Java's
// ParameterObjectValue.getCorrelatedToWithoutChildren() returning
// ImmutableSet.of().
func (*ParameterObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return nil
}

// RebaseLeaf returns this unchanged — ParameterObjectValue has no
// correlation to rebase. Mirrors Java's
// ParameterObjectValue.rebaseLeaf returning `this`.
func (v *ParameterObjectValue) RebaseLeaf(_ CorrelationIdentifier) Value {
	return v
}

var _ LeafValue = (*ParameterObjectValue)(nil)
