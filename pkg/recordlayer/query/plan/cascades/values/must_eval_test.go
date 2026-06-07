package values

// mustEvalForTest bridges the Phase-A interface change — Value.Evaluate
// now returns (any, error) — for the in-package tests that were written
// against the single-return form. The happy path returns the value; a
// data-dependent runtime error (arithmetic overflow, division by zero,
// invalid cast, type mismatch) is re-panicked with the SAME typed error
// value the implementation used to panic with, so existing
// require.Panics / recover assertions keep matching unchanged.
//
// This is a temporary shim: Phase E re-points the tests to consume the
// error channel directly (`got, err := v.Evaluate(ctx)`) and deletes
// this helper.
func mustEvalForTest(v Value, evalCtx any) any {
	out, err := v.Evaluate(evalCtx)
	if err != nil {
		panic(err)
	}
	return out
}
