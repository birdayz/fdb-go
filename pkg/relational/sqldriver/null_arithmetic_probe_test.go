package sqldriver_test

// Probes NULL propagation through arithmetic and scalar functions: any arithmetic
// with a NULL operand yields NULL — including the classic NULL * 0 = NULL (NOT 0)
// and NULL - NULL = NULL; ABS(NULL) = NULL; and COALESCE breaks the propagation so
// COALESCE(NULL,0)*0 = 0. Non-NULL arithmetic is unaffected.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NullArithmeticProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nap CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nap/s WITH TEMPLATE nap")
	dsn := fmt.Sprintf("fdbsql:///testdb_nap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")       // a NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (2, 5)") // a=5

	eval := func(expr string, id int) sql.NullInt64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM t WHERE id = %d", expr, id)).Scan(&v); err != nil {
			t.Fatalf("%s (id=%d): %v", expr, id, err)
		}
		return v
	}
	null := func(name, expr string, id int) {
		t.Run(name, func(t *testing.T) {
			if v := eval(expr, id); v.Valid {
				t.Errorf("%s (id=%d) = %d, want NULL", expr, id, v.Int64)
			}
		})
	}
	val := func(name, expr string, id int, want int64) {
		t.Run(name, func(t *testing.T) {
			v := eval(expr, id)
			if !v.Valid || v.Int64 != want {
				t.Errorf("%s (id=%d) = (valid=%v, %d), want %d", expr, id, v.Valid, v.Int64, want)
			}
		})
	}

	null("null_plus", "a + 5", 1)
	null("null_times_zero", "a * 0", 1) // classic: NULL*0 = NULL, not 0
	null("null_minus_null", "a - a", 1)
	null("abs_null", "ABS(a)", 1)
	val("nonnull_plus", "a + 5", 2, 10)
	val("nonnull_times_zero", "a * 0", 2, 0)
	val("coalesce_breaks_propagation", "COALESCE(a, 0) * 0", 1, 0)
}
