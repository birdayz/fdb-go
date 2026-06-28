package sqldriver_test

// Probes the null-coalescing functions. COALESCE (multi-arg first-non-null) and
// IFNULL work; COALESCE over all-NULL → NULL. NULLIF is NOT supported (42883) —
// conformant with Java (which also lacks it); the documented substitute is
// `CASE WHEN a = b THEN NULL ELSE a END`, which works.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CoalesceNullifProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_coal")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_coal")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE coal CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_coal/s WITH TEMPLATE coal")
	dsn := fmt.Sprintf("fdbsql:///testdb_coal?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a=NULL, b=NULL, c=5
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, c) VALUES (1, 5)")

	nv := func(expr string) sql.NullInt64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	ck := func(name, expr string, wantValid bool, want int64) {
		t.Run(name, func(t *testing.T) {
			got := nv(expr)
			if got.Valid != wantValid || (wantValid && got.Int64 != want) {
				t.Errorf("%s = (valid=%v, %d), want (valid=%v, %d)", expr, got.Valid, got.Int64, wantValid, want)
			}
		})
	}

	ck("coalesce_multi_first_nonnull", "COALESCE(a, b, c)", true, 5)
	ck("coalesce_all_null", "COALESCE(a, b)", false, 0)
	ck("coalesce_literal_short_circuit", "COALESCE(10, a)", true, 10)
	ck("coalesce_col_before_null", "COALESCE(c, a)", true, 5)
	ck("ifnull_null_arg", "IFNULL(a, 99)", true, 99)
	ck("ifnull_nonnull_arg", "IFNULL(c, 99)", true, 5)

	// NULLIF is unsupported (42883) — conformant with Java; use CASE instead.
	t.Run("nullif_unsupported", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT NULLIF(c, 5) FROM t WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "42883") {
			t.Errorf("NULLIF error = %v, want 42883 (unsupported; use CASE WHEN a=b THEN NULL ELSE a END)", err)
		}
	})
	// the documented NULLIF substitute works: CASE WHEN c=5 THEN NULL ELSE c END → NULL.
	ck("case_nullif_substitute_null", "CASE WHEN c = 5 THEN NULL ELSE c END", false, 0)
	ck("case_nullif_substitute_value", "CASE WHEN c = 3 THEN NULL ELSE c END", true, 5)
}
