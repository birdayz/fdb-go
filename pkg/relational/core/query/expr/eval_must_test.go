package expr_test

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"

// must* helpers replicate the deleted panic-on-error Evaluate/Eval
// wrappers (RFC-091 collapse): each calls the error-returning method and
// panics on error, preserving the pre-collapse test semantics.

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
	Eval(any) (predicates.TriBool, error)
}](p T, ctx any) predicates.TriBool {
	v, err := p.Eval(ctx)
	if err != nil {
		panic(err)
	}
	return v
}
