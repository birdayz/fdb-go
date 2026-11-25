package sqltest

// Pins the boundary of string-function support beyond what string_functions_probe
// covers. Supported: plain TRIM/LTRIM/RTRIM, REPLACE, nested composition.
// Unsupported, cleanly rejected: LPAD/RPAD (0AF00), REPEAT (42601 — not a
// recognized function), and the qualified `TRIM(BOTH 'x' FROM s)` specification
// syntax (0AF00 — parses but does not plan; plain TRIM(s) works).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_StringFuncsBoundaryProbe(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_sfb")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_sfb")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sfb CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_sfb/s WITH TEMPLATE sfb")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_SFB?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	str := func(expr string) string {
		var v string
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	supported := func(name, expr, want string) {
		t.Run(name, func(t *testing.T) {
			if got := str(expr); got != want {
				t.Errorf("%s = %q, want %q", expr, got, want)
			}
		})
	}
	supported("trim", "TRIM('  hi  ')", "hi")
	supported("ltrim", "LTRIM('  hi')", "hi")
	supported("rtrim", "RTRIM('hi  ')", "hi")
	supported("replace", "REPLACE('aXbXc', 'X', '-')", "a-b-c")
	supported("nested_trim_in_replace", "REPLACE(TRIM('  ab  '), 'a', 'A')", "Ab")

	rejected := func(name, expr, code string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1")
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want %s", expr, err, code)
			}
		})
	}
	rejected("lpad_unsupported", "LPAD('5', 3, '0')", "0AF00")
	rejected("rpad_unsupported", "RPAD('5', 3, '0')", "0AF00")
	rejected("repeat_unrecognized", "REPEAT('ab', 3)", "42601")
	rejected("qualified_trim_unsupported", "TRIM(BOTH 'x' FROM 'xxhixx')", "0AF00")
}
