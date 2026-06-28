package sqldriver_test

// Probes multi-statement DML batches via a single Exec ("stmt1; stmt2"): all
// statements run and RowsAffected is the sum. In AUTO-COMMIT each statement
// commits independently (MultiPlan.Execute loops, each child runs its own tx), so
// a mid-batch failure leaves earlier statements committed — NOT atomic; wrap in
// an explicit transaction for all-or-nothing. (A multi-statement *query* is
// rejected — Exec only.)

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_MultiStatementProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_multip")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_multip")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE multip CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_multip/s WITH TEMPLATE multip")
	dsn := fmt.Sprintf("fdbsql:///testdb_multip?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() int64 {
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c
	}

	t.Run("batch_runs_all_statements", func(t *testing.T) {
		r, err := db.ExecContext(ctx, "INSERT INTO t (id,v) VALUES (1,10); INSERT INTO t (id,v) VALUES (2,20)")
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if n, _ := r.RowsAffected(); n != 2 {
			t.Errorf("batch RowsAffected = %d, want 2 (summed across statements)", n)
		}
		if count() != 2 {
			t.Errorf("after 2-INSERT batch count = %d, want 2", count())
		}
	})

	t.Run("batch_insert_then_update", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id,v) VALUES (4,40); UPDATE t SET v=41 WHERE id=4"); err != nil {
			t.Fatalf("batch: %v", err)
		}
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id=4").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 41 {
			t.Errorf("id=4 v after INSERT;UPDATE batch = %d, want 41 (both statements ran)", v)
		}
	})

	t.Run("autocommit_batch_not_atomic_on_midfailure", func(t *testing.T) {
		before := count()
		// id=3 inserts; id=1 is a duplicate → 23505. In auto-commit the first
		// statement has already committed, so id=3 persists (non-atomic).
		_, err := db.ExecContext(ctx, "INSERT INTO t (id,v) VALUES (3,30); INSERT INTO t (id,v) VALUES (1,99)")
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Fatalf("expected 23505 from the duplicate, got %v", err)
		}
		if count() != before+1 {
			t.Errorf("after mid-failing batch count = %d, want %d (id=3 committed; auto-commit is per-statement, not atomic)", count(), before+1)
		}
		// the failed duplicate did not overwrite id=1.
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id=1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 10 {
			t.Errorf("id=1 v = %d, want 10 (the failed INSERT must not change it)", v)
		}
	})

	t.Run("multi_statement_query_rejected", func(t *testing.T) {
		// a multi-statement *query* (Query, not Exec) is not supported.
		_, err := db.QueryContext(ctx, "SELECT id FROM t; SELECT v FROM t")
		if err == nil {
			t.Errorf("multi-statement query unexpectedly succeeded; want rejection")
		}
	})
}
