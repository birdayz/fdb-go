package values

// EvaluatesToValue tests whether a child Value's runtime evaluation
// matches one of four boolean-shaped predicates: IS TRUE, IS FALSE,
// IS NULL, IS NOT NULL. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// EvaluatesToValue`.
//
//	x IS TRUE    ↔  EvaluatesToValue{Child: x, Eval: EvaluatesToTrue}
//	x IS NULL    ↔  EvaluatesToValue{Child: x, Eval: EvaluatesToNull}
//
// Used by:
//   - SQL `IS [NOT] {NULL,TRUE,FALSE}` predicates lowered to the
//     Value layer.
//   - Plan rewrites that need to pattern-match on these specific
//     truth-value shapes.
//
// Type is always non-null boolean (these predicates have a defined
// result for any operand — even NULL maps to TRUE/FALSE).
type EvaluatesToValue struct {
	Child Value
	Eval  EvaluatesTo
}

// EvaluatesTo discriminates the four supported evaluations.
type EvaluatesTo int

const (
	// EvaluatesToTrue is `x IS TRUE`.
	EvaluatesToTrue EvaluatesTo = iota
	// EvaluatesToFalse is `x IS FALSE`.
	EvaluatesToFalse
	// EvaluatesToNull is `x IS NULL`.
	EvaluatesToNull
	// EvaluatesToNotNull is `x IS NOT NULL`.
	EvaluatesToNotNull
)

// NewEvaluatesToValue constructs the predicate Value.
func NewEvaluatesToValue(child Value, eval EvaluatesTo) *EvaluatesToValue {
	return &EvaluatesToValue{Child: child, Eval: eval}
}

// Children returns the single child.
func (v *EvaluatesToValue) Children() []Value {
	if v.Child == nil {
		return []Value{}
	}
	return []Value{v.Child}
}

// Name returns the debug-print kind.
func (*EvaluatesToValue) Name() string { return "evaluates_to" }

// Type returns NotNullBoolean — these predicates always return a
// definite truth value (UNKNOWN propagation is handled by the
// IS [NOT] {NULL,TRUE,FALSE} semantics).
func (*EvaluatesToValue) Type() Type { return NotNullBoolean }

// Evaluate computes the predicate.
//
// Rules:
//   - x IS TRUE: true iff x evaluates to bool true; false otherwise.
//   - x IS FALSE: true iff x evaluates to bool false; false otherwise.
//   - x IS NULL: true iff x evaluates to nil; false otherwise.
//   - x IS NOT NULL: true iff x evaluates to non-nil; false otherwise.
//
// Type mismatches (non-bool x with IS TRUE / IS FALSE) return false —
// the runtime value isn't a boolean true / false, so the predicate
// is false (not UNKNOWN).
func (v *EvaluatesToValue) Evaluate(evalCtx any) (any, error) {
	if v.Child == nil {
		switch v.Eval {
		case EvaluatesToNull:
			return true, nil
		case EvaluatesToNotNull:
			return false, nil
		default:
			return false, nil
		}
	}
	val, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	switch v.Eval {
	case EvaluatesToTrue:
		b, ok := val.(bool)
		return ok && b, nil
	case EvaluatesToFalse:
		b, ok := val.(bool)
		return ok && !b, nil
	case EvaluatesToNull:
		return val == nil, nil
	case EvaluatesToNotNull:
		return val != nil, nil
	}
	return false, nil
}
