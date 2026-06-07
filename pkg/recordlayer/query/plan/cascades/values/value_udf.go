package values

// UdfValue represents a user-defined function (UDF) call.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.UdfValue`.
//
// Java's UdfValue is an ABSTRACT class — concrete UDFs subclass it,
// override `call(List<Object>)` to supply business logic, and pair
// with a UdfFunction for parameter-type description / planner
// registration. The Go port uses a function-typed field
// (`Call func(args []any) any`) instead of inheritance — the
// idiomatic Go shape for "subclass-supplied behaviour".
//
// Stateless and stateful UDFs are both representable: a stateless
// UDF's Call is a pure function over args; a stateful UDF closes
// over an accumulator (the implementor's responsibility — UdfValue
// itself does not enforce purity).
//
// Plan-level identity: each UDF gets a stable Name (the
// implementor-supplied function name) so two UdfValue calls with
// the same Name + same children compare semantically equal. This
// is the Go-side stand-in for Java's `getClass().getCanonicalName()`
// keying — the FQ class name uniquely identifies the UDF
// implementation in Java; Name does the same in Go.
//
// Eval: walks each child Value's Evaluate, collects results into
// `[]any`, hands to the user-supplied Call function. Per Java,
// argument NULLs propagate at the user's discretion — Call is
// called with the raw evaluated args; the user is responsible for
// NULL-handling within their UDF body.
type UdfValue struct {
	// FunctionName is the user-facing UDF name (e.g. "MY_FN"). Two
	// UdfValues with the same FunctionName + same arg types are
	// planner-equivalent (same UDF call shape).
	FunctionName string

	// ResultType is the declared return type of the UDF. Java's
	// equivalent comes from UdfFunction.getReturnType().
	ResultType Type

	// Args are the operand Values whose evaluations feed Call.
	Args []Value

	// Call is the UDF body — receives the evaluated argument values
	// in source order, returns the UDF's result. Required: a UdfValue
	// with a nil Call evaluates to nil regardless of args (matches the
	// "non-evaluable yet" placeholder pattern).
	Call func(args []any) any
}

// NewUdfValue constructs a UDF call.
//
// The Call function is REQUIRED for runtime evaluation; passing nil
// is allowed at construction time so the planner can build UDF call
// shapes before the implementation is wired (e.g. during analyser
// phase). A nil Call surfaces nil from Evaluate per the seed's
// placeholder-Value contract.
func NewUdfValue(name string, resultType Type, args []Value, call func(args []any) any) *UdfValue {
	if resultType == nil {
		resultType = UnknownType
	}
	cp := make([]Value, len(args))
	copy(cp, args)
	return &UdfValue{
		FunctionName: name,
		ResultType:   resultType,
		Args:         cp,
		Call:         call,
	}
}

// Children returns the Args list — UDF arguments are the only Value
// children.
func (u *UdfValue) Children() []Value {
	return u.Args
}

// Name returns the UDF function name (used for debug print +
// planner equivalence keying).
func (u *UdfValue) Name() string { return u.FunctionName }

// Type returns the declared return type.
func (u *UdfValue) Type() Type { return u.ResultType }

// Evaluate walks each arg's Evaluate and hands the resulting `[]any`
// to Call. Returns nil if Call is nil (placeholder mode).
func (u *UdfValue) Evaluate(evalCtx any) (any, error) {
	if u.Call == nil {
		return nil, nil
	}
	args := make([]any, len(u.Args))
	for i, a := range u.Args {
		if a != nil {
			av, err := a.Evaluate(evalCtx)
			if err != nil {
				return nil, err
			}
			args[i] = av
		}
	}
	return u.Call(args), nil
}

// WithChildren returns a new UdfValue with the given children
// substituted for Args. Function name, result type, and Call body
// carry through unchanged.
func (u *UdfValue) WithChildren(newChildren []Value) *UdfValue {
	return NewUdfValue(u.FunctionName, u.ResultType, newChildren, u.Call)
}
