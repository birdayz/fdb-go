package sqldriver_test

// Regression: `UPDATE t SET col = DEFAULT` used to be SILENTLY no-op'd — the builder
// dropped the assignment (Expression() is nil for the DEFAULT alternative), so the
// column was left unchanged while the statement reported SUCCESS (a misleading silent
// ignore). Java doesn't support it either — ExpressionVisitor.visitUpdatedElement calls
// ctx.expression().accept(this), which NPEs on a DEFAULT RHS. Per the conformance
// principle (Java NPE → Go emits a clean error), it is now rejected with 0AF00
// "DEFAULT is not supported in UPDATE ... SET". A normal UPDATE is unaffected.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UpdateSetDefaultRejectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_usd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_usd")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE usd CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_usd/s WITH TEMPLATE usd")
	dsn := fmt.Sprintf("fdbsql:///testdb_usd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, s) VALUES (1, 99, 'orig')")

	getV := func() int64 {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return v
	}

	t.Run("set_default_rejected_0AF00", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE t SET v = DEFAULT WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("UPDATE SET v=DEFAULT error = %v, want 0AF00 (not a silent no-op success)", err)
		}
		if v := getV(); v != 99 {
			t.Errorf("v = %d after rejected UPDATE, want 99 (unchanged)", v)
		}
	})
	t.Run("set_default_mixed_with_real_assignment_rejected", func(t *testing.T) {
		// A DEFAULT anywhere in the SET list must reject the whole statement (no partial apply).
		_, err := db.ExecContext(ctx, "UPDATE t SET v = 5, s = DEFAULT WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("UPDATE SET v=5, s=DEFAULT error = %v, want 0AF00", err)
		}
		if v := getV(); v != 99 {
			t.Errorf("v = %d — the real assignment must NOT have applied when the stmt is rejected", v)
		}
	})
	t.Run("normal_update_still_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE t SET v = 42 WHERE id = 1"); err != nil {
			t.Fatalf("normal UPDATE: %v", err)
		}
		if v := getV(); v != 42 {
			t.Errorf("v = %d, want 42", v)
		}
	})
}
