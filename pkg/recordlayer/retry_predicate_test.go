package recordlayer

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// TestRetryPredicates_AnyCauseAndFirstCause pins the two retry rules of the
// target over error TREES, where they disagree: the runner's
// (FDBDatabaseRunnerImpl.RunRetriable.handle, any cause in the chain) and
// AutoContinuingCursor's (FDBExceptions.isRetriable, the first FDB error).
// The errors Go calls terminal under a route never read as retriable.
func TestRetryPredicates_AnyCauseAndFirstCause(t *testing.T) {
	t.Parallel()
	notRetryable := fdb.Error{Code: 2000} // client_invalid_operation
	conflict := fdb.Error{Code: 1020}
	cases := []struct {
		name            string
		err             error
		any, firstCause bool
	}{
		{"a bare conflict", conflict, true, true},
		{"a wrapped conflict", fmt.Errorf("save: %w", conflict), true, true},
		{"a non-retryable code", notRetryable, false, false},
		{"a join whose later branch is retryable", errors.Join(notRetryable, conflict), true, false},
		{"two %w, the later retryable", fmt.Errorf("%w: %w", notRetryable, conflict), true, false},
		{
			"a join whose later branch is a retriable record-core error",
			errors.Join(errors.New("x"), &RecordCoreRetriableTransactionError{Message: "store lock taken"}), true, true,
		},
		{
			"a retriable record-core error over a non-retryable code",
			&RecordCoreRetriableTransactionError{Message: "retry", Cause: notRetryable}, true, true,
		},
		{"a context no longer active", errTransactionNotActive(), false, false},
	}
	for _, c := range cases {
		if got := isRetriableAnyCause(c.err); got != c.any {
			t.Errorf("%s: any-cause %v, want %v", c.name, got, c.any)
		}
		if got := isRetriableFirstCause(c.err); got != c.firstCause {
			t.Errorf("%s: first-cause %v, want %v", c.name, got, c.firstCause)
		}
	}
}

// opaqueWrap is a wrapper outside fmt and errors: IsRetriable must walk any
// Unwrap() error, as a SQL-layer wrapper of a page's FDB error is.
type opaqueWrap struct{ cause error }

func (e opaqueWrap) Error() string { return "opaque: " + e.cause.Error() }
func (e opaqueWrap) Unwrap() error { return e.cause }

// TestIsRetriable pins the exported predicate the SQL layer uses to decide
// whether work that failed in a transaction it owns may be rerun in a fresh
// one (statement_read.go): it is FDBDatabase.Run's rule exactly, every
// retryable FDB code at any depth of the chain, and a
// RecordCoreRetriableTransactionError whatever its cause; nil, plain errors,
// terminal codes and a chain flattened by %v are not.
func TestIsRetriable(t *testing.T) {
	t.Parallel()
	type retriableCase struct {
		name string
		err  error
		want bool
	}
	var nilPtr *fdb.Error
	cases := []retriableCase{
		{"nil", nil, false},
		{"a plain error", errors.New("boom"), false},
		{"a typed nil *fdb.Error", nilPtr, false},
		{"transaction_cancelled (1025)", fdb.Error{Code: 1025}, false},
		{"transaction_timed_out (1031)", fdb.Error{Code: 1031}, false},
		{"client_invalid_operation (2000)", fdb.Error{Code: 2000}, false},
		{"transaction_too_large (2101)", fdb.Error{Code: 2101}, false},
		{"a context no longer active", errTransactionNotActive(), false},
		{"a conflict flattened by %v", fmt.Errorf("page: %v", fdb.Error{Code: 1020}), false},
		{"a *fdb.Error conflict", &fdb.Error{Code: 1020}, true},
		{"a conflict two %w deep", fmt.Errorf("statement: %w", fmt.Errorf("page: %w", fdb.Error{Code: 1020})), true},
		{"a too-old read under a non-fmt wrapper", opaqueWrap{fdb.Error{Code: 1007}}, true},
		{"a join whose second branch is retryable", errors.Join(errors.New("x"), fdb.Error{Code: 1009}), true},
		{"a retriable record-core error", &RecordCoreRetriableTransactionError{Message: "store lock taken"}, true},
		{"a wrapped retriable record-core error", fmt.Errorf("open: %w", &RecordCoreRetriableTransactionError{Message: "m"}), true},
		{
			"a retriable record-core error over a terminal code",
			&RecordCoreRetriableTransactionError{Message: "m", Cause: fdb.Error{Code: 2000}}, true,
		},
	}
	// Every code fdb.IsRetryable names, bare and wrapped.
	for _, code := range []int{1007, 1009, 1020, 1021, 1037, 1038, 1039, 1042, 1051, 1078, 1213, 1223} {
		if !fdb.IsRetryable(code) {
			t.Fatalf("code %d is not retryable per fdb.IsRetryable: the case list is stale", code)
		}
		cases = append(cases,
			retriableCase{fmt.Sprintf("code %d", code), fdb.Error{Code: code}, true},
			retriableCase{fmt.Sprintf("code %d wrapped", code), fmt.Errorf("x: %w", fdb.Error{Code: code}), true},
		)
	}
	for _, c := range cases {
		if got := IsRetriable(c.err); got != c.want {
			t.Errorf("%s: IsRetriable = %v, want %v", c.name, got, c.want)
		}
		if got, runner := IsRetriable(c.err), isRetriableAnyCause(c.err); got != runner {
			t.Errorf("%s: IsRetriable = %v, the runner's rule = %v", c.name, got, runner)
		}
	}
}
