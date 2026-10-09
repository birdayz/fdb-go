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
