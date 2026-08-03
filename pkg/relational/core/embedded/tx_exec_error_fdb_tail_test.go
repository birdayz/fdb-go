package embedded

import (
	"errors"
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

// translateExecError's FDB tail is the last thing standing between a raw FDB
// code and the application on a page fetch. Pages after the first are fetched
// from Rows.Next, long after QueryContext returned, so they never pass through
// the QueryContext/ExecContext translateFDBError wrap — that one only ever sees
// the eagerly-fetched first page. Inside an explicit transaction there is also
// no DB.Run retry loop to absorb anything. So whatever this tail does not
// translate escapes untranslated, with no SQLSTATE at all.
//
// WHY THIS IS A UNIT PIN AND NOT AN END-TO-END ONE — the reachability, measured
// rather than assumed, because the honest shape of the hazard is narrower than
// "a page can conflict":
//
//   - 1020 not_committed is a COMMIT-time verdict. FDB never returns it from a
//     read, and SimFDB models that faithfully: its whole injection surface
//     (InjectOnce and the seeded BUGGIFY draws alike) lives in the commit path
//     and there is no read-time fault site to drive. So no in-tree harness can
//     produce a 1020 from a page fetch.
//   - 1007 transaction_too_old IS a read-time code, and it is the one an
//     over-running in-transaction scan would earn. It does not reach this tail
//     either, and deliberately: the page preflight pre-empts at the 4s budget
//     and returns 40001 before FDB's window expires, which is exactly the
//     pre-emption's reason to exist.
//   - 1025 transaction_cancelled DOES reach it, and is pinned end-to-end by
//     TestCancelledTxContextPageFetchIs25F01.
//
// That leaves the 40001-producing lanes of this tail reachable only if one of
// those two facts changes — a read-time fault site appears, or the pre-emption
// is removed. Neither is unthinkable, and a lane nothing exercises is a lane
// that rots. This test therefore pins the tail at the seam it can actually be
// held at: feed it the codes directly and assert the SQLSTATE. It fails if the
// tail is reverted to a bare `return err`, which is the mutation that matters.
func TestTranslateExecErrorTranslatesFDBCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		code int
		want api.ErrorCode
	}{
		// The two codes that mean "definitely did not commit, re-run it".
		{"not_committed", 1020, api.ErrCodeSerializationFailure},
		{"transaction_too_old", 1007, api.ErrCodeSerializationFailure},
		// The one that reaches this tail today.
		{"transaction_cancelled", 1025, api.ErrCodeTransactionInactive},
		// Distinct from 40001 on purpose: the outcome is UNKNOWN, so a blind
		// retry double-applies. Collapsing it into 40001 here would hand the
		// caller a code that says "safe to re-run" about a transaction that
		// may already have committed.
		{"commit_unknown_result", 1021, api.ErrCodeStatementCompletionUnknown},
		{"used_during_commit", 2017, api.ErrCodeTransactionInactive},
		{"transaction_timed_out", 1031, api.ErrCodeTransactionTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Both producer shapes: the wire error the client returns and the
			// value-typed fdb.Error the fdb package surfaces. A tail that
			// handles one and not the other leaks half the time, which is
			// worse than leaking always because it looks tested.
			for _, in := range []error{
				fmt.Errorf("page fetch: %w", &wire.FDBError{Code: tc.code}),
				fmt.Errorf("page fetch: %w", fdb.Error{Code: tc.code}),
			} {
				got := translateExecError(in)
				var apiErr *api.Error
				if !errors.As(got, &apiErr) {
					t.Fatalf("translateExecError(%T code %d) = %v (%T), want *api.Error %s: "+
						"the FDB tail is not translating, so a page-fetch error reaches the "+
						"application with no SQLSTATE at all",
						in, tc.code, got, got, tc.want)
				}
				if apiErr.Code != tc.want {
					t.Fatalf("translateExecError(%T code %d) = %s, want %s",
						in, tc.code, apiErr.Code, tc.want)
				}
			}
		})
	}
}

// An already-translated error must survive the tail unchanged. The tail runs on
// every execution error, including the many that earlier lanes in
// translateExecError already turned into an *api.Error, so a tail that
// re-wrapped would relabel unrelated failures with an FDB code they never had.
func TestTranslateExecErrorLeavesTranslatedErrorsAlone(t *testing.T) {
	t.Parallel()

	in := api.NewError(api.ErrCodeUndefinedColumn, "no such column: nope")
	got := translateExecError(in)

	var apiErr *api.Error
	if !errors.As(got, &apiErr) {
		t.Fatalf("translateExecError(*api.Error) = %v (%T), want the *api.Error back", got, got)
	}
	if apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("translateExecError relabelled an already-translated error %s → %s: "+
			"the tail is not idempotent, so every non-FDB execution error now reports "+
			"whatever code the FDB lanes happen to fall through to",
			api.ErrCodeUndefinedColumn, apiErr.Code)
	}
}
