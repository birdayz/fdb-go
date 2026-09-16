package sqldriver

// SQL autocommit owns one statement transaction. The generic FDB transaction
// runner may retry 1021, but Java SQL does not enter that runner: it opens one
// context and commits once. These pins retain both ambiguity branches and guard
// against reintroducing SQL replay (a duplicate INSERT or double relative UPDATE).
// The existing 40003 error-surface extension distinguishes unknown completion
// from a definite rollback; the application, not the driver, owns any retry.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// injectSimFDBWithHandle is injectSimFDB plus the *simfdb.SimDB itself, so a test can place a
// fault at an exact commit. Kept separate from injectSimFDB so the non-fault tests keep the
// narrower helper.
func injectSimFDBWithHandle(t *testing.T, seed uint64) (string, *simfdb.SimDB) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	simDB := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	return key, sim
}

// openSimSchema builds a database + schema over a SimFDB backend and returns a connection
// bound to the schema, plus the SimDB handle for fault placement.
func openSimSchema(t *testing.T, seed uint64, tableDDL string) (*sql.DB, *simfdb.SimDB) {
	t.Helper()
	key, sim := injectSimFDBWithHandle(t, seed)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tmpl "+tableDDL)
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, sim
}

// TestSQLFault_UpdateRelative_DoubleApply prevents the old silent replay:
// a durable ambiguous +1 must stay +1 and report 40003, never become +2.
func TestSQLFault_UpdateRelative_DoubleApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 7,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")

	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")

	readA := func() int64 {
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
			t.Fatalf("read a: %v", err)
		}
		return a
	}
	if got := readA(); got != 100 {
		t.Fatalf("seed value a = %d, want 100", got)
	}

	// The next commit reports commit_unknown_result on its APPLIED branch: the write is durable
	// and the caller is told nothing. That branch is the hazard's precondition, so it is named
	// explicitly rather than left to the run's coin.
	sim.InjectOnce(simfdb.CommitUnknownApplied)

	_, err := db.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1")
	wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "autocommit UPDATE")
	if got := readA(); got != 101 {
		t.Fatalf("a = %d, want 101: an ambiguous durable UPDATE must not be replayed", got)
	}

	// Data integrity otherwise holds: exactly one row, no duplicate.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("COUNT(*) = %d, want 1 — the hazard is a double-APPLY, not a duplicate row", n)
	}
}

// TestSQLFault_InsertDurablyCommitted_Spurious23505 prevents reporting a
// duplicate-key error for the first INSERT's own durable ambiguous commit.
func TestSQLFault_InsertDurablyCommitted_Spurious23505(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 11,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")

	sim.InjectOnce(simfdb.CommitUnknownApplied)

	_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
	wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "autocommit INSERT")

	// The injected applied branch is durable exactly once despite the ambiguity.

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("COUNT(*) = %d, want 1 — the durably-committed row must be present exactly "+
			"once; anything else is a data-integrity failure, which is a REAL bug, not this pin", n)
	}
	var a int64
	if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if a != 100 {
		t.Fatalf("a = %d, want 100 — the durable row must carry the value the statement wrote", a)
	}
}

// TestSQLFault_1021HazardsAreDeterministic pins the corrected ambiguous-commit
// contract across repeated identical schedules. Both the error and value are
// asserted: a deleted injection would leave the same value without an error.
func TestSQLFault_1021HazardsAreDeterministic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const runs = 3
	for r := range runs {
		t.Run(fmt.Sprint(r), func(t *testing.T) {
			t.Parallel()
			db, sim := openSimSchema(t, 7,
				"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
			mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
			sim.InjectOnce(simfdb.CommitUnknownApplied)
			_, err := db.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1")
			wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "autocommit UPDATE")
			if got := readTxFaultA(t, ctx, db); got != 101 {
				t.Fatalf("run %d: a=%d, want 101 without replay", r, got)
			}
		})
	}
}

// TestSQLFault_DiscardedCommitUnknownAppliesExactlyOnce pins an application
// retry AFTER independently determining that the first transaction did not
// commit. SQL itself must surface 40003 and leave the discarded state untouched.
func TestSQLFault_DiscardedCommitUnknownAppliesExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("relative UPDATE", func(t *testing.T) {
		t.Parallel()
		db, sim := openSimSchema(t, 23,
			"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
		mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
		sim.InjectOnce(simfdb.CommitUnknownDiscarded)
		query := "UPDATE t SET a = a + 1 WHERE id = 1"
		_, err := db.ExecContext(ctx, query)
		wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "discarded UPDATE")
		if got := readTxFaultA(t, ctx, db); got != 100 {
			t.Fatalf("discarded UPDATE changed a to %d, want 100", got)
		}
		mustExecSQL(t, db, ctx, query)
		if got := readTxFaultA(t, ctx, db); got != 101 {
			t.Fatalf("application retry changed a to %d, want 101", got)
		}
	})

	t.Run("INSERT", func(t *testing.T) {
		t.Parallel()
		db, sim := openSimSchema(t, 29,
			"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
		sim.InjectOnce(simfdb.CommitUnknownDiscarded)
		query := "INSERT INTO t (id, a) VALUES (1, 100)"
		_, err := db.ExecContext(ctx, query)
		wantCommitSQLState(t, err, api.ErrCodeStatementCompletionUnknown, "discarded INSERT")
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("discarded INSERT left %d rows, want 0 before application retry", n)
		}
		mustExecSQL(t, db, ctx, query)
		if got := readTxFaultA(t, ctx, db); got != 100 {
			t.Fatalf("application retry inserted %d, want 100", got)
		}
	})
}

// TestSQLAutoCommitDML_CommitUnknownIsNotReplayed distinguishes the commit's
// unknown outcome from SQL execution errors. Java commits the statement's
// context once; an application retry is not an implicit part of SQL INSERT
// or relative UPDATE. Both actual FDB outcomes must report the same 40003.
func TestSQLAutoCommitDML_CommitUnknownIsNotReplayed(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"insert", "relative_update"} {
		for _, fault := range []struct {
			name    string
			code    int
			applied bool
		}{
			{"applied", simfdb.CommitUnknownApplied, true},
			{"discarded", simfdb.CommitUnknownDiscarded, false},
		} {
			t.Run(operation+"/"+fault.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				db, sim := openSimSchema(t, 745, "CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
				query := "INSERT INTO t VALUES (1, 100)"
				var wantCount, wantSum int64
				if operation == "relative_update" {
					mustExecSQL(t, db, ctx, query)
					query = "UPDATE t SET a = a + 1 WHERE id = 1"
					wantCount, wantSum = 1, 100
					if fault.applied {
						wantSum = 101
					}
				} else if fault.applied {
					wantCount, wantSum = 1, 100
				}
				sim.InjectOnce(fault.code)
				_, err := db.ExecContext(ctx, query)
				// Read durability even if the error assertion fails, so the
				// transcript distinguishes replay from a missing fault.
				var count int64
				var sum sql.NullInt64
				if readErr := db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(a) FROM t").Scan(&count, &sum); readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("commit branch=%s error=%v rows=%d sum=%+v", fault.name, err, count, sum)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeStatementCompletionUnknown {
					t.Errorf("one-shot SQL commit error=%v, want 40003; do not replay the statement", err)
				}
				if count != wantCount || sum.Valid != (wantCount > 0) || (sum.Valid && sum.Int64 != wantSum) {
					t.Errorf("durable state=(%d,%+v), want rows=%d sum=%d without SQL replay", count, sum, wantCount, wantSum)
				}
			})
		}
	}
}

func TestSQLAutoCommitDML_AtomicityAndOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := openSimSchema(t, 746, "CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t VALUES (1, 100)")
	// The first row's mutation precedes a genuine duplicate in the same
	// statement. A statement-owned transaction must discard the first row too.
	_, err := db.ExecContext(ctx, "INSERT INTO t VALUES (2, 200), (1, 999)")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("genuine duplicate=%v, want 23505", err)
	}
	check := func(wantCount, wantSum int64) {
		t.Helper()
		var count, sum int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(a) FROM t").Scan(&count, &sum); err != nil {
			t.Fatal(err)
		}
		if count != wantCount || sum != wantSum {
			t.Fatalf("committed state=(%d,%d), want (%d,%d)", count, sum, wantCount, wantSum)
		}
	}
	check(1, 100)
	result, err := db.ExecContext(ctx, "INSERT INTO t VALUES (3, 300), (4, 400)")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 2 {
		t.Fatalf("committed affected count=(%d,%v), want 2", n, err)
	}
	check(3, 800)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, "UPDATE t SET a = a + 1"); err != nil {
		t.Fatal(err)
	}
	check(3, 800) // Statement completion may not commit a borrowed transaction.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check(3, 800)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.ExecContext(canceled, "DELETE FROM t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled statement error=%v, want context.Canceled", err)
	}
	check(3, 800)
}
