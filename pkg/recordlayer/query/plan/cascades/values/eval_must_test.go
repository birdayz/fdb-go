package values

// mustEvaluate replicates the deleted panic-on-error Evaluate wrapper
// (RFC-091 collapse): it calls the error-returning Evaluate and panics on
// error, preserving the pre-collapse test semantics (including the
// panic-on-bad-input assertions).
func mustEvaluate[T interface {
	Evaluate(any) (any, error)
}](v T, ctx any) any {
	r, err := v.Evaluate(ctx)
	if err != nil {
		panic(err)
	}
	return r
}
