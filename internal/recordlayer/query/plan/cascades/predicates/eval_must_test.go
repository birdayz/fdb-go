package predicates

// must* helpers replicate the deleted panic-on-error Evaluate/Eval/
// EvalAgainst wrappers (RFC-091 collapse): each calls the error-returning
// method and panics on error, preserving the pre-collapse test semantics
// (including the panic-on-bad-input assertions).

func mustEvaluate[T interface {
	Evaluate(any) (any, error)
}](v T, ctx any) any {
	r, err := v.Evaluate(ctx)
	if err != nil {
		panic(err)
	}
	return r
}

func mustEval[T interface {
	Eval(any) (TriBool, error)
}](p T, ctx any) TriBool {
	v, err := p.Eval(ctx)
	if err != nil {
		panic(err)
	}
	return v
}

func mustEvalAgainst[T interface {
	EvalAgainst(any, any) (TriBool, error)
}](c T, l, r any) TriBool {
	v, err := c.EvalAgainst(l, r)
	if err != nil {
		panic(err)
	}
	return v
}
