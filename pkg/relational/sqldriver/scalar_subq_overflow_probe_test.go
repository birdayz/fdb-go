package sqldriver_test

// Pins two silent-wrong-result traps that Go handles correctly:
//  - scalar-subquery cardinality: a (SELECT ...) used as a scalar (projection or WHERE
//    comparand) that returns >1 row must ERROR (21000), not silently pick one row.
//    Empty → NULL; single row → that value.
//  - aggregate overflow: SUM that exceeds int64 must ERROR (22003 "long overflow"),
//    not silently wrap to a wrong value.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ScalarSubqOverflowProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sso")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sso")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sso "+
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE o (id BIGINT NOT NULL, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sso/s WITH TEMPLATE sso")
	dsn := fmt.Sprintf("fdbsql:///testdb_sso?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,10),(2,20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id, w) VALUES (1,100),(2,200),(3,300)")

	wantErr := func(name, q, code string) {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Fatalf("%s unexpectedly succeeded, want %s", name, code)
			}
			if !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want %s", name, err, code)
			}
		})
	}
	wantErr("scalar_subq_multirow_projection_21000", "SELECT id, (SELECT w FROM o) FROM t", "21000")
	wantErr("scalar_subq_multirow_where_21000", "SELECT id FROM t WHERE v = (SELECT w FROM o)", "21000")

	t.Run("scalar_subq_empty_is_null", func(t *testing.T) {
		var w sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT (SELECT w FROM o WHERE w > 9999) FROM t WHERE id = 1").Scan(&w); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if w.Valid {
			t.Errorf("empty scalar subquery = %v, want NULL", w.Int64)
		}
	})
	t.Run("scalar_subq_single_row_value", func(t *testing.T) {
		var w sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT (SELECT w FROM o WHERE id = 1) FROM t WHERE id = 1").Scan(&w); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !w.Valid || w.Int64 != 100 {
			t.Errorf("single-row scalar subquery = %v (valid=%v), want 100", w.Int64, w.Valid)
		}
	})
	t.Run("sum_overflow_22003", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (3, 9223372036854775807)") // max int64
		var s sql.NullInt64
		err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM t").Scan(&s)
		if err == nil || !strings.Contains(err.Error(), "22003") {
			t.Errorf("SUM overflow error = %v, want 22003 (long overflow, not silent wrap)", err)
		}
	})
}
