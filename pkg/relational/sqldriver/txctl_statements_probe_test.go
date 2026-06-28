package sqldriver_test

// Probes transaction-control statements issued as raw SQL via Exec. The proper
// way to use transactions with database/sql is BeginTx/Commit/Rollback (which the
// driver tracks via activeTx); raw COMMIT/ROLLBACK/START TRANSACTION SQL is
// non-standard (JDBC uses setAutoCommit, not raw SQL). Pins the clean edges:
// COMMIT with no active transaction → 0A000 (clear error, not a crash); ROLLBACK
// with no active transaction → harmless no-op.
//
// NOTE (documented, not asserted): a raw `START TRANSACTION` via Exec does NOT
// open a driver-tracked transaction — a following raw COMMIT reports "no active
// transaction" and intervening DML auto-commits. Use BeginTx for transactions.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_TxCtlStatementsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_txctlp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txctlp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txctlp CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txctlp/s WITH TEMPLATE txctlp")
	dsn := fmt.Sprintf("fdbsql:///testdb_txctlp?cluster_file=%s&schema=s", clusterFilePath)
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
