package sqldriver_test

// Probes transaction-control statements issued as raw SQL via Exec. The proper
// way to use transactions with database/sql is BeginTx/Commit/Rollback (which the
// driver tracks via activeTx); raw COMMIT/ROLLBACK/START TRANSACTION SQL is
// non-standard (JDBC uses setAutoCommit, not raw SQL). Pins the clean edges:
// COMMIT with no active transaction → 0A000 (clear error, not a crash); ROLLBACK
// with no active transaction → harmless no-op.
//
// A raw `START TRANSACTION` opens a driver-tracked transaction on the
// connection that ran it. On a *sql.Conn the following statements join it; on
// a *sql.DB the connection goes back to the pool, whose ResetSession rolls the
// transaction back before the next statement, so that statement auto-commits
// and a raw COMMIT finds no transaction. Use BeginTx for transactions.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_TxCtlStatementsProbe(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_txctlp")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_txctlp")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txctlp CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_txctlp/s WITH TEMPLATE txctlp")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_TXCTLP?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Run("commit_no_active_tx_errors", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "COMMIT")
		if err == nil || !strings.Contains(err.Error(), "0A000") {
			t.Errorf("COMMIT with no tx error = %v, want 0A000 (no active transaction)", err)
		}
	})
	t.Run("rollback_no_active_tx_is_noop", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Errorf("ROLLBACK with no tx = %v, want no-op (nil)", err)
		}
	})
	t.Run("start_transaction_tracked_on_a_conn", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer conn.Close()
		for _, stmt := range []string{"START TRANSACTION", "INSERT INTO t (id, v) VALUES (2, 20)", "ROLLBACK"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if n := countID(t, ctx, conn, 2); n != 0 {
			t.Fatalf("row inserted after START TRANSACTION survived ROLLBACK (count %d): the insert auto-committed", n)
		}
		for _, stmt := range []string{"START TRANSACTION", "INSERT INTO t (id, v) VALUES (3, 30)", "COMMIT"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if n := countID(t, ctx, conn, 3); n != 1 {
			t.Fatalf("row inserted after START TRANSACTION and COMMIT: count %d, want 1", n)
		}
	})
	t.Run("start_transaction_reset_by_the_pool", func(t *testing.T) {
		pool, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer pool.Close()
		pool.SetMaxOpenConns(1)
		for _, stmt := range []string{"START TRANSACTION", "INSERT INTO t (id, v) VALUES (4, 40)"} {
			if _, err := pool.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if _, err := pool.ExecContext(ctx, "COMMIT"); err == nil || !strings.Contains(err.Error(), "0A000") {
			t.Fatalf("COMMIT after the pool reset the session = %v, want 0A000 (no active transaction)", err)
		}
		if n := countID(t, ctx, pool, 4); n != 1 {
			t.Fatalf("insert after the pool reset the session: count %d, want 1 (auto-committed)", n)
		}
	})
	// the proper transaction API (BeginTx) still works for atomicity.
	t.Run("begintx_commit_works", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 10)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 1").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 1 {
			t.Errorf("after BeginTx+INSERT+Commit count = %d, want 1", c)
		}
	})
}

func countID(t *testing.T, ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64,
) int64 {
	t.Helper()
	var n int64
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count id %d: %v", id, err)
	}
	return n
}
