package embedded

// The two producers of "this transaction outlived FDB's 5-second MVCC window",
// pinned where they are RAISED rather than where they are consumed.
//
// The condition has exactly two guards: paginatingRows.preflightTxBudget, which
// pre-empts at 4 s, and translateFDBCode's 1007 arm, which relays FDB's own wall
// at 5 s. Both map to SQLSTATE 40001 — and so does 1020, a genuine read/write
// conflict, which is a completely different problem with the opposite remedy.
//
// THE DIMENSION THAT WAS UNPROBED. TestTranslateFDBError already covered the CODE
// each FDB error maps to, exhaustively. Nothing covered whether two errors
// sharing a code are still DISTINGUISHABLE, because until there was a marker they
// were not — and the consumer that needed to tell them apart (the DISTINCT cost
// probe's regime detector) enumerated error numbers instead, matched the producer
// that essentially never fires on this path, and hard-failed a CI lane the first
// time the other one did. Codes were tested; the collision between them was not.
// This is that axis.

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

func TestTranslateFDBError_SeparatesALostWindowFromAConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		err              error
		wantTimeLimit    bool
		wantSource       api.TimeLimitSource
		whatItActuallyIs string
	}{
		{
			"transaction_too_old, pointer-typed",
			&wire.FDBError{Code: 1007}, true, api.TimeLimitFDBTooOld,
			"the MVCC window closed; retrying identically fails identically",
		},
		{
			"transaction_too_old, value-typed",
			fdb.Error{Code: 1007},
			true, api.TimeLimitFDBTooOld,
			"the same condition off the other carrier the client can produce",
		},
		{
			"not_committed",
			&wire.FDBError{Code: 1020}, false, "",
			"a genuine conflict; retrying as-is usually succeeds",
		},
		{
			"transaction_timed_out",
			&wire.FDBError{Code: 1031}, false, "",
			"the client's own transaction timeout, not the MVCC window",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := translateFDBError(tc.err)
			if api.IsTransactionTimeLimit(got) != tc.wantTimeLimit {
				t.Fatalf("IsTransactionTimeLimit(%s) = %v, want %v — %s.\n"+
					"1007 and 1020 both map to SQLSTATE 40001, so the code cannot "+
					"separate them and the marker is the only thing that can. Getting "+
					"this wrong in either direction is costly: an unmarked 1007 makes "+
					"every consumer enumerate error numbers again, and a marked 1020 "+
					"makes a real concurrency bug read as an expired window.",
					tc.name, !tc.wantTimeLimit, tc.wantTimeLimit, tc.whatItActuallyIs)
			}
			if !tc.wantTimeLimit {
				return
			}
			var ttl *api.TransactionTimeLimitError
			if !errors.As(got, &ttl) || ttl.Source != tc.wantSource {
				t.Fatalf("%s carries source %v, want %q", tc.name, ttl, tc.wantSource)
			}
			// The raw FDB error must survive underneath the marker: this
			// translator is the single funnel for the SQL path, so anything it
			// blinds is blinded everywhere.
			var fdbPtr *wire.FDBError
			var fdbVal fdb.Error
			if !errors.As(got, &fdbPtr) && !errors.As(got, &fdbVal) {
				t.Fatalf("%s lost the underlying FDB error: %v. Inserting the marker "+
					"must add a link to the chain, never replace its tail.", tc.name, got)
			}
		})
	}
}
