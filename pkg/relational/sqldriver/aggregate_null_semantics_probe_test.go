package sqldriver_test

// Pins aggregate NULL semantics: COUNT(*) counts all rows including those with a
// NULL column; COUNT(col) counts only non-NULL; SUM/AVG/MIN/MAX ignore NULLs. Over
// an all-NULL column: COUNT(*) counts the rows, COUNT(col)=0, and SUM/AVG = NULL
// (not 0).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AggregateNullSemanticsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ans")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ans")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ans CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ans/s WITH TEMPLATE ans")
	dsn := fmt.Sprintf("fdbsql:///testdb_ans?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (2)") // a NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (3,30)")

	agg := func(expr string) sql.NullFloat64 {
		var v sql.NullFloat64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	val := func(name, expr string, want float64) {
		t.Run(name, func(t *testing.T) {
			v := agg(expr)
			if !v.Valid || v.Float64 != want {
				t.Errorf("%s = (valid=%v, %v), want %v", expr, v.Valid, v.Float64, want)
			}
		})
	}
	val("count_star_includes_null_row", "COUNT(*)", 3)
	val("count_col_excludes_null", "COUNT(a)", 2)
	val("sum_ignores_null", "SUM(a)", 40)
	val("avg_ignores_null", "AVG(a)", 20) // 40 / 2, not / 3
	val("min_ignores_null", "MIN(a)", 10)
	val("max_ignores_null", "MAX(a)", 30)

	// reduce to a single row whose only value is NULL.
	mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id IN (1, 3)")
	val("count_star_over_null_row", "COUNT(*)", 1)
	val("count_col_all_null", "COUNT(a)", 0)
	t.Run("sum_all_null_is_null", func(t *testing.T) {
		if v := agg("SUM(a)"); v.Valid {
			t.Errorf("SUM(all-NULL) = %v, want NULL", v.Float64)
		}
	})
	t.Run("avg_all_null_is_null", func(t *testing.T) {
		if v := agg("AVG(a)"); v.Valid {
			t.Errorf("AVG(all-NULL) = %v, want NULL", v.Float64)
		}
	})
}
