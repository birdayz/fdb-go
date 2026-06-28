package sqldriver_test

// Regression for a SEVERE data-loss bug: `DELETE ... LIMIT n` silently IGNORED the
// LIMIT and deleted ALL rows matching the WHERE (e.g. `DELETE WHERE a>0 LIMIT 1`
// wiped every matching row, not one). The shared grammar accepts a limitClause on
// DELETE, but the builder dropped it. Now rejected with 0AF00 "limit is not
// supported", matching Java (QueryVisitor.visitDeleteStatement asserts
// limitClause==null). Fail-closed: the table is left UNCHANGED. A normal DELETE
// (no LIMIT) is unaffected.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DeleteLimitRejectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dlr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dlr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dlr CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dlr/s WITH TEMPLATE dlr")
	dsn := fmt.Sprintf("fdbsql:///testdb_dlr?cluster_file=%s&schema=s", clusterFilePath)
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

	t.Run("delete_limit_rejected_no_data_loss", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30),(4,40),(5,50)")
		_, err := db.ExecContext(ctx, "DELETE FROM t WHERE a > 0 LIMIT 1")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("DELETE ... LIMIT error = %v, want 0AF00 (limit is not supported)", err)
		}
		// CRITICAL: the table must be UNCHANGED — no rows deleted by the rejected stmt.
		if c := count(); c != 5 {
			t.Errorf("after rejected DELETE LIMIT, count = %d, want 5 (NO data loss)", c)
		}
	})
	t.Run("delete_no_where_limit_rejected_worst_case", func(t *testing.T) {
		// The nastiest variant: `DELETE FROM t LIMIT 1` (no WHERE) — if the LIMIT were
		// ignored this would wipe the ENTIRE table. The guard is unconditional on the
		// limitClause, so it rejects regardless of WHERE; pin it + assert no data loss.
		_, err := db.ExecContext(ctx, "DELETE FROM t LIMIT 1")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("DELETE (no WHERE) LIMIT error = %v, want 0AF00 (limit is not supported)", err)
		}
		if c := count(); c != 5 {
			t.Errorf("after rejected no-WHERE DELETE LIMIT, count = %d, want 5 (table NOT wiped)", c)
		}
	})
	t.Run("delete_without_limit_still_works", func(t *testing.T) {
		// id=1 still present from above; delete it specifically.
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE id = 1")
		if err != nil {
			t.Fatalf("plain DELETE: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("DELETE WHERE id=1 RowsAffected = %d, want 1", n)
		}
		if c := count(); c != 4 {
			t.Errorf("after DELETE id=1, count = %d, want 4", c)
		}
	})
	t.Run("delete_all_matching_no_limit", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE a > 0")
		if err != nil {
			t.Fatalf("delete all: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 4 {
			t.Errorf("DELETE WHERE a>0 RowsAffected = %d, want 4", n)
		}
		if c := count(); c != 0 {
			t.Errorf("after DELETE all, count = %d, want 0", c)
		}
	})
}
