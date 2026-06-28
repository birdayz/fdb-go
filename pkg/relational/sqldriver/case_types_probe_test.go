package sqldriver_test

// Probes CASE result typing + function composition: searched CASE (int/int and
// int/double branches — the latter unify to double, 1.5), string branches, no-ELSE
// (match → value, no-match → NULL), and simple CASE (CASE a WHEN ...). Nested
// function calls compose (UPPER(SUBSTR(...)), ABS(a-10)). Note: CASE result type is
// polymorphic (UnknownType) so mixed-type branches like `THEN 1 ELSE 'x'` are NOT
// branch-unified/rejected — the taken branch's value is returned; not exercised
// here as it yields a row-dependent type.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CaseTypesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_casetp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_casetp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE casetp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_casetp/s WITH TEMPLATE casetp")
	dsn := fmt.Sprintf("fdbsql:///testdb_casetp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 5)")

	str := func(expr string) sql.NullString {
		var v sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	ck := func(name, expr, want string) {
		t.Run(name, func(t *testing.T) {
			got := str(expr)
			if !got.Valid || got.String != want {
				t.Errorf("%s = %q (valid=%v), want %q", expr, got.String, got.Valid, want)
			}
		})
	}

	ck("searched_then", "CASE WHEN a = 5 THEN 1 ELSE 2 END", "1")
	ck("searched_else", "CASE WHEN a = 9 THEN 1 ELSE 2 END", "2")
	ck("int_double_branches_unify", "CASE WHEN a = 5 THEN 1.5 ELSE 2 END", "1.5")
	ck("string_branches", "CASE WHEN a = 5 THEN 'hi' ELSE 'lo' END", "hi")
	ck("no_else_match", "CASE WHEN a = 5 THEN 1 END", "1")
	ck("simple_case", "CASE a WHEN 5 THEN 'five' WHEN 6 THEN 'six' ELSE 'other' END", "five")
	ck("simple_case_else", "CASE a WHEN 1 THEN 'one' ELSE 'other' END", "other")
	ck("nested_string_funcs", "UPPER(SUBSTR('hello', 1, 3))", "HEL")

	t.Run("no_else_no_match_is_null", func(t *testing.T) {
		if got := str("CASE WHEN a = 9 THEN 1 END"); got.Valid {
			t.Errorf("CASE no-ELSE no-match = %q, want NULL", got.String)
		}
	})
	t.Run("nested_math_func", func(t *testing.T) {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT ABS(a - 10) FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 5 {
			t.Errorf("ABS(a - 10) = %d, want 5", v)
		}
	})
}
