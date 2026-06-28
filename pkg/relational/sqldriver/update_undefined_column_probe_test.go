package sqldriver_test

// Regression: UPDATE that assigns a NONEXISTENT column must be rejected with a clean
// 42703 (undefined column) at build time — the same SQLSTATE INSERT and SELECT
// already produce for an unknown column. Previously this reached the executor and
// surfaced a LEAKY raw error ("executor: update field %q not found in descriptor",
// no SQLSTATE). A valid column UPDATE still works (the case-insensitive validation
// must not over-reject).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UpdateUndefinedColumnProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuc CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuc/s WITH TEMPLATE uuc")
	dsn := fmt.Sprintf("fdbsql:///testdb_uuc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 10)")

	t.Run("undefined_set_column_is_42703", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE t SET nope = 5 WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Errorf("UPDATE SET nope error = %v, want 42703 (undefined column)", err)
		}
		// must NOT be the old leaky raw executor error.
		if err != nil && strings.Contains(err.Error(), "not found in descriptor") {
			t.Errorf("leaky executor error still surfaced: %v", err)
		}
	})
	t.Run("undefined_among_valid_is_42703", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE t SET a = 1, nope = 5 WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "42703") {
			t.Errorf("UPDATE SET a=1, nope=5 error = %v, want 42703", err)
		}
	})
	t.Run("valid_column_update_still_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE t SET a = 42 WHERE id = 1"); err != nil {
			t.Fatalf("valid UPDATE: %v", err)
		}
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
			t.Fatalf("read: %v", err)
		}
		if a != 42 {
			t.Errorf("after valid UPDATE, a = %d, want 42", a)
		}
	})
	t.Run("case_insensitive_column_accepted", func(t *testing.T) {
		// 'A' declared; lowercase 'a' and mixed work (no false rejection).
		if _, err := db.ExecContext(ctx, "UPDATE t SET A = 7 WHERE id = 1"); err != nil {
			t.Errorf("UPDATE SET A (uppercase) unexpectedly failed: %v", err)
		}
	})
}
