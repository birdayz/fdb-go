package expr_test

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// mustEvalPred / mustEvalVal bridge the RFC-087 interface change —
// QueryPredicate.Eval now returns (TriBool, error) and Value.Evaluate
// returns (any, error) — for the expr-package tests written against the
// single-return form. The happy path returns the value; a data-dependent
// runtime error (arithmetic overflow, division by zero, invalid cast,
// type mismatch) is re-panicked with the SAME typed error value the
// implementation used to panic with, so existing require.Panics / recover
// assertions keep matching unchanged.
//
// Temporary shim mirroring the values package's mustEvalForTest; Phase E
// re-points these tests to consume the error channel directly.
func mustEvalPred(p predicates.QueryPredicate, evalCtx any) predicates.TriBool {
	r, err := p.Eval(evalCtx)
	if err != nil {
		panic(err)
	}
	return r
}

func mustEvalVal(v values.Value, evalCtx any) any {
	out, err := v.Evaluate(evalCtx)
	if err != nil {
		panic(err)
	}
	return out
}
