package sqldriver

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// TestSQLFault_AutoCommit1021ExhaustionSurfacesAs40003 retains the original
// 101-fault schedule but asserts the SQL statement boundary, not the generic
// FDB runner's retry budget. Each caller invocation consumes exactly one fault.
// A driver that silently retries drains the schedule early and fails this pin.
func TestSQLFault_AutoCommit1021ExhaustionSurfacesAs40003(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 13,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	// Initialize the query connection before arming the schedule. QueryContext
	// bootstraps the catalog in a separate idempotent transaction even inside
	// BeginTx; that administrative commit must not consume a DML fault.
	var initial int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&initial); err != nil {
		t.Fatal(err)
	}
	if initial != 0 {
		t.Fatalf("initial row count=%d, want 0", initial)
	}
	const attempts = 101
	faults := make([]int, attempts)
	for i := range faults {
		faults[i] = simfdb.CommitUnknownDiscarded
	}
	sim.InjectSequence(faults...)
	for i := range attempts {
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
		t.Logf("statement %d/%d: %v", i+1, attempts, err)
		wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "autocommit INSERT")
		// Roll back the inspection transaction: even a future store-header
		// upgrade must not let verification advance the queued fault schedule.
		probe, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		readErr := probe.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n)
		rollbackErr := probe.Rollback()
		if readErr != nil || rollbackErr != nil {
			t.Fatalf("inspect discarded state: read=%v rollback=%v", readErr, rollbackErr)
		}
		if n != 0 {
			t.Fatalf("attempt %d left %d durable rows on the discarded branch", i, n)
		}
	}
	// The fault population was nonempty and exactly consumed. Independent
	// visibility checks above established absence before each application retry.
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
	if got := readTxFaultA(t, ctx, db); got != 100 {
		t.Fatalf("after the fault schedule: a=%d, want 100", got)
	}
}

// TestSQLFault_AutoCommit1021FirstAmbiguityIsVisible guards against waiting
// for an FDB retry budget to expire before reporting an unknown SQL outcome.
func TestSQLFault_AutoCommit1021FirstAmbiguityIsVisible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 17,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	sim.InjectOnce(simfdb.CommitUnknownDiscarded)
	_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
	wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "first autocommit ambiguity")
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("discarded statement left %d rows, want 0", n)
	}
}
