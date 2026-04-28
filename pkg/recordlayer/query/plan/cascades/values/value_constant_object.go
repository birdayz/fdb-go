package values

// ConstantObjectValue is a NAMED reference to a constant captured
// during planning. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// ConstantObjectValue`.
//
// The constant value itself is stored in an EvaluationContext at
// execution time, keyed by (alias, constantId). At plan-time the
// Value carries only the placeholder reference + the bound Type;
// the actual value is dereferenced when Evaluate runs against an
// EvaluationContext.
//
// Why a named placeholder instead of a literal: parameter binding,
// plan-cache reuse, and constant capture during query rewriting all
// need to defer the actual constant until execution-time. Plan-time
// rewrites operate on the placeholder; execution dereferences.
//
// Type is whatever the planner determined at capture time —
// typically nullable to allow NULL constants.
//
// CONFORMANCE NOTE: Java's eval consults
// EvaluationContext.dereferenceConstant + PromoteValue.isPromotionNeeded
// for type promotion when the runtime constant's type doesn't match
// the bound result type. The seed Evaluate looks up via a
// ConstantDeref interface on evalCtx; promotion is NOT handled (the
// looked-up value is returned as-is). Wired-when-execution-lands.
type ConstantObjectValue struct {
	Alias      CorrelationIdentifier
	ConstantID string
	ResultType Type
}

// NewConstantObjectValue constructs the placeholder.
func NewConstantObjectValue(alias CorrelationIdentifier, constantID string, resultType Type) *ConstantObjectValue {
	if resultType == nil {
		resultType = UnknownType
	}
	return &ConstantObjectValue{
		Alias:      alias,
		ConstantID: constantID,
		ResultType: resultType,
	}
}

// Children returns the empty slice — leaf.
func (*ConstantObjectValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*ConstantObjectValue) Name() string { return "constobj" }

// Type returns the bound result type.
func (v *ConstantObjectValue) Type() Type { return v.ResultType }

// ConstantDeref is the optional EvaluationContext capability for
// dereferencing a ConstantObjectValue. Implementations look up the
// constant by (alias, constantID) in the planner's per-alias
// constant map.
//
// Mirrors Java's EvaluationContext.dereferenceConstant.
type ConstantDeref interface {
	// DereferenceConstant returns the value bound to (alias,
	// constantID) at evaluation time, or nil if no binding exists.
	DereferenceConstant(alias CorrelationIdentifier, constantID string) any
}

// Evaluate dereferences the constant via evalCtx's ConstantDeref
// capability. Returns nil if evalCtx doesn't implement
// ConstantDeref or if the binding is missing.
//
// Promotion (per Java's eval) is NOT applied — the returned value
// is the raw bound value. When a planner rule starts capturing
// constants whose runtime type may differ from the bound
// ResultType, this Evaluate must route through a promotion helper.
func (v *ConstantObjectValue) Evaluate(evalCtx any) any {
	deref, ok := evalCtx.(ConstantDeref)
	if !ok {
		return nil
	}
	return deref.DereferenceConstant(v.Alias, v.ConstantID)
}

// GetCorrelatedTo returns the singleton set containing the
// alias — ConstantObjectValue depends on the alias's binding.
func (v *ConstantObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
