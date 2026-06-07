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
// the bound result type. Go's Evaluate matches Java 1:1: dereference
// the constant, then apply promoteConstant when the runtime type
// differs from ResultType.
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
// Matches Java's ConstantObjectValue.eval: after dereferencing,
// applies numeric type promotion when the runtime object's type
// doesn't match the bound ResultType. Relation-typed results are
// returned as-is (no promotion for structured stream types).
func (v *ConstantObjectValue) Evaluate(evalCtx any) (any, error) {
	deref, ok := evalCtx.(ConstantDeref)
	if !ok {
		return nil, nil
	}
	obj := deref.DereferenceConstant(v.Alias, v.ConstantID)
	if obj == nil {
		return nil, nil
	}
	// Relation types pass through without promotion, matching Java.
	if IsRelation(v.ResultType) {
		return obj, nil
	}
	return promoteConstant(obj, v.ResultType), nil
}

// promoteConstant applies numeric type promotion to obj when its
// Go runtime type doesn't match the target Type's TypeCode.
// Mirrors Java's PromoteValue physical operators for primitive
// numeric widening:
//
//	INT → LONG:   int32 → int64
//	INT → FLOAT:  int32 → float32
//	INT → DOUBLE: int32 → float64
//	LONG → FLOAT: int64 → float32
//	LONG → DOUBLE:int64 → float64
//	FLOAT → DOUBLE:float32 → float64
//
// Returns obj unchanged when no promotion is needed (same type) or
// the conversion isn't in the promotion map.
func promoteConstant(obj any, target Type) any {
	if target == nil {
		return obj
	}
	tc := target.Code()

	switch v := obj.(type) {
	case int32:
		switch tc {
		case TypeCodeLong:
			return int64(v)
		case TypeCodeFloat:
			return float32(v)
		case TypeCodeDouble:
			return float64(v)
		case TypeCodeInt:
			return obj // already matches
		}
	case int64:
		switch tc {
		case TypeCodeFloat:
			return float32(v)
		case TypeCodeDouble:
			return float64(v)
		case TypeCodeLong:
			return obj // already matches
		}
	case int:
		// Go's int is platform-dependent; always widen to int64 for
		// TypeCodeLong (Java stores long, never bare int).
		switch tc {
		case TypeCodeLong:
			return int64(v)
		case TypeCodeFloat:
			return float32(v)
		case TypeCodeDouble:
			return float64(v)
		}
	case float32:
		switch tc {
		case TypeCodeDouble:
			return float64(v)
		case TypeCodeFloat:
			return obj // already matches
		}
	case float64:
		if tc == TypeCodeDouble {
			return obj // already matches
		}
	}
	// No promotion needed or not promotable — return as-is.
	return obj
}

// GetCorrelatedTo returns the singleton set containing the
// alias — ConstantObjectValue depends on the alias's binding.
func (v *ConstantObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.Alias: {}}
}
