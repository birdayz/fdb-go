package sqldriver_test

// Probes explicit-transaction DML atomicity via database/sql Tx: ROLLBACK discards
// all DML in the transaction; COMMIT persists it atomically (visible to a
// subsequent query). (Read-your-writes WITHIN an open tx is a separate documented
// gap — SELECT auto-commits — TODO.md; this test reads AFTER commit/rollback.)

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_TxCommitRollbackProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_tcrp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_tcrp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tcrp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_tcrp/s WITH TEMPLATE tcrp")
	dsn := fmt.Sprintf("fdbsql:///testdb_tcrp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("rollback_discards", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 10)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (2, 20)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if c := count(); c != 0 {
			t.Errorf("after ROLLBACK count = %d, want 0 (rolled-back inserts must not persist)", c)
		}
	})

	t.Run("commit_persists_atomically", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (3, 30)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (4, 40)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if c := count(); c != 2 {
			t.Errorf("after COMMIT count = %d, want 2", c)
		}
		// committed rows are readable by value.
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 3").Scan(&a); err != nil {
			t.Fatalf("read committed: %v", err)
		}
		if a != 30 {
			t.Errorf("committed id=3 a = %d, want 30", a)
		}
	})
}
