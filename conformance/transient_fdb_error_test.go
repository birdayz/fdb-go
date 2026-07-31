//go:build bazelrunfiles

package conformance_test

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

// TestIsTransientFDBError pins which Java-side errors the corpus runner re-runs
// instead of reporting as a cross-engine divergence.
//
// The dimensional gap this closes: the predicate used to match ONLY the catalog
// conflict (1020), so a transaction starved past FDB's 5s wall-clock limit
// (1007) fell through to entryConforms and was published as
// "join_four_way_eq_chain: Java errored but Go succeeded" — a phantom semantic
// divergence on a corpus entry whose entire dataset is eight rows across four
// tables, which no engine needs five seconds to join. The classifier, not the
// engines, was the defect: 1007 is retryable by FDB's own definition
// (FDBExceptions.isRetriable delegates to FDBException.isRetryable, true for
// both codes), so it carries no information about SQL semantics.
//
// The negative arms are the load-bearing half. Broadening the predicate is only
// safe while it stays narrow: a planner exception or a wrong-rows disagreement
// must still reach the divergence report, or the RFC-082 lock would retry real
// regressions away instead of failing on them.
func TestIsTransientFDBError(t *testing.T) {
	t.Parallel()

	t.Run("retryable FDB errors are re-run, not reported", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{{
			// 1020: parallel workers contend on the shared catalog keyspace.
			name: "catalog conflict (1020)",
			err: &plandiff.JavaError{
				Message:        "Transaction not committed due to conflict",
				ExceptionClass: "FDBException",
			},
		}, {
			// 1007: the exact error that reddened the RunSql corpus in CI.
			name: "transaction too old (1007)",
			err: &plandiff.JavaError{
				Message:        "Transaction is too old to perform reads or be committed",
				ExceptionClass: "FDBException",
			},
		}} {
			if !isTransientFDBError(tc.err) {
				t.Errorf("%s must be retried, not published as a divergence; "+
					"it is retryable by FDB's own definition and says nothing "+
					"about SQL semantics. Got not-transient for: %v", tc.name, tc.err)
			}
		}
	})

	t.Run("a genuine engine disagreement is never retried away", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{{
			name: "planner exception",
			err: &plandiff.JavaError{
				Message:        "Cascades planner could not plan query",
				ExceptionClass: "UnableToPlanException",
			},
		}, {
			name: "complexity exception",
			err: &plandiff.JavaError{
				Message:        "plan complexity exceeded",
				ExceptionClass: "RecordQueryPlanComplexityException",
			},
		}, {
			name: "semantic error",
			err: &plandiff.JavaError{
				Message:        "Unknown column FOO",
				ExceptionClass: "RelationalException",
			},
		}, {
			// A transport failure is infra, but it is entryConforms' INFRA arm
			// that must claim it — retrying it here would mask a sick server.
			name: "transport failure",
			err:  fmt.Errorf("plandiff: HTTP POST: context deadline exceeded"),
		}} {
			if isTransientFDBError(tc.err) {
				t.Errorf("%s must reach the divergence report, not be retried "+
					"away: %v", tc.name, tc.err)
			}
		}
	})

	t.Run("nil is not transient", func(t *testing.T) {
		t.Parallel()
		if isTransientFDBError(nil) {
			t.Fatal("a successful run must not enter the retry loop")
		}
		if isTransientFDBError(errors.New("")) {
			t.Fatal("an empty error message must not match a transient code")
		}
	})
}
