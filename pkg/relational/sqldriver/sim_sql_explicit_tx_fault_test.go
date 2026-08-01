package sqldriver

// RFC-198 criteria 9(d), 10: the explicit-transaction commit path under
// injected FDB faults, meeting TODO.md's booked bar — 1007 AND BOTH 1021
// branches by explicit InjectOnce, never by natural occurrence (SimFDB's
// version counter has no relationship to time, so 1007 is naturally
// unreachable; the 1021 branch is a per-seed coin, so a seed sweep certifies
// whichever branch it drew).
//
// These are the explicit-transaction SIBLINGS of the four auto-commit cases in
// sim_sql_fault_test.go (criterion 11 keeps those unchanged). The disposition
// differs because an explicit transaction does not auto-retry (RFC-198
// Decision 7a): the driver does not hold the statements it would have to
// replay, so the fault reaches the application as a SQLSTATE —
//
//	1007 / 1020 → 40001: the transaction definitely did NOT commit; re-run it.
//	1021        → 40003: the outcome is UNKNOWN — the transaction may or may
//	                     not have committed. A blind retry is a double-apply on
//	                     the applied branch; the application must determine the
//	                     outcome before retrying.
//
// The two 1021 branches are SEPARATE tests: a single test accepting either
// branch is exactly the "certifies whichever branch it drew" failure the
// booked bar names.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// beginRelUpdateTx opens an explicit transaction and applies a relative UPDATE
// (a = a + 1) inside it, returning the transaction ready to commit. The
// relative update is the non-idempotent statement whose double-apply is the
// documented 40003 hazard.
func beginRelUpdateTx(t *testing.T, ctx context.Context, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1"); err != nil {
		t.Fatalf("in-tx update: %v", err)
	}
	return tx
}

func wantCommitSQLState(t *testing.T, err error, want api.ErrorCode, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: COMMIT succeeded, want SQLSTATE %s — the injected fault was "+
			"swallowed, which on the explicit-transaction path means a silent retry "+
			"exists where RFC-198 Decision 7a forbids one", label, want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("%s: COMMIT failed with %v (%T), want *api.Error %s — "+
			"embeddedTx.Commit is bypassing translateFDBError", label, err, err, want)
	}
	if apiErr.Code != want {
		t.Fatalf("%s: COMMIT SQLSTATE %s, want %s: %v", label, apiErr.Code, want, err)
	}
}

func readTxFaultA(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var a int64
	if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
		t.Fatalf("read a: %v", err)
	}
	return a
}

// TestSQLFault_ExplicitTx1007SurfacesAs40001 pins the 1007 arm of the booked
// bar on the explicit-transaction commit path: transaction_too_old fires
// BEFORE apply, so nothing is durable, there is no silent retry, and the
// application sees 40001 — "definitely did not commit, re-run".
func TestSQLFault_ExplicitTx1007SurfacesAs40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 31,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")

	tx := beginRelUpdateTx(t, ctx, db)
	sim.InjectOnce(1007)
	wantCommitSQLState(t, tx.Commit(), api.ErrCodeSerializationFailure, "1007")

	if got := readTxFaultA(t, ctx, db); got != 100 {
		t.Fatalf("a = %d after a 1007-failed commit, want 100: 1007 fires before apply, "+
			"so nothing may be durable", got)
	}
}

// TestSQLFault_ExplicitTxCommitUnknownAppliedSurfacesAs40003 pins the APPLIED
// branch: the mutations are durable, the client is told the outcome is
// unknown, and the application sees 40003 — NOT 40001, because re-running is
// not automatically safe. The test then performs the blind re-run and asserts
// the double-apply as the documented consequence rather than hiding it.
func TestSQLFault_ExplicitTxCommitUnknownAppliedSurfacesAs40003(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 37,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")

	tx := beginRelUpdateTx(t, ctx, db)
	sim.InjectOnce(simfdb.CommitUnknownApplied)
	wantCommitSQLState(t, tx.Commit(), api.ErrCodeStatementCompletionUnknown, "1021 applied")

	// The mutations ARE durable — applied exactly once, because the explicit
	// path has no retry to meet its own durable write.
	if got := readTxFaultA(t, ctx, db); got != 101 {
		t.Fatalf("a = %d after a 1021-applied commit, want 101: on the applied branch "+
			"the transaction's write is durable exactly once", got)
	}

	// The documented consequence of a BLIND retry: the application re-runs the
	// whole transaction without determining the outcome first, and the
	// relative update double-applies. This is why 40003 is not 40001.
	tx2 := beginRelUpdateTx(t, ctx, db)
	if err := tx2.Commit(); err != nil {
		t.Fatalf("blind re-run commit: %v", err)
	}
	if got := readTxFaultA(t, ctx, db); got != 102 {
		t.Fatalf("a = %d after a blind re-run, want 102 (the double-apply 40003 warns about)", got)
	}
}

// TestSQLFault_ExplicitTxCommitUnknownDiscardedSurfacesAs40003 pins the
// DISCARDED branch — the one a seed sweep would have missed: the commit never
// reached the proxy, NOTHING is durable, and the application still sees 40003
// (the client cannot distinguish the branches; that is what 1021 means). On
// this branch a retry is safe, and the test performs it.
func TestSQLFault_ExplicitTxCommitUnknownDiscardedSurfacesAs40003(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 41,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")

	tx := beginRelUpdateTx(t, ctx, db)
	sim.InjectOnce(simfdb.CommitUnknownDiscarded)
	wantCommitSQLState(t, tx.Commit(), api.ErrCodeStatementCompletionUnknown, "1021 discarded")

	// Nothing durable on this branch.
	if got := readTxFaultA(t, ctx, db); got != 100 {
		t.Fatalf("a = %d after a 1021-discarded commit, want 100: on the discarded branch "+
			"nothing may be durable", got)
	}

	// A retry IS safe here — which is precisely why the driver cannot decide
	// it for the application: only the application can determine which branch
	// it is on.
	tx2 := beginRelUpdateTx(t, ctx, db)
	if err := tx2.Commit(); err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if got := readTxFaultA(t, ctx, db); got != 101 {
		t.Fatalf("a = %d after the retry, want 101 (applied exactly once)", got)
	}
}
