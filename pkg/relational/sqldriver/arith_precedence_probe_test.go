package sqldriver_test

// Pins a MAJOR fdb-relational dialect quirk: arithmetic has NO operator
// precedence — `* / % + -` are all one left-associative `mathOperator` alternative
// in the grammar, so expressions evaluate strictly LEFT-TO-RIGHT. `2 + 3 * 4` is
// (2+3)*4 = 20, NOT 14. This deviates from standard SQL but is CONFORMANT with
// Java (same grammar) — the cross-engine corpus pins it via
// `SELECT (x + y) * z, x + y * z FROM T_NEST` (plandiff/conformance green). Use
// parentheses to force a different grouping. Also: negative literals (`-5`, `-3`)
// are supported, but there is no unary-minus OPERATOR (`-(expr)` / `- -5` → 42601).
//
// (If precedence is ever added, this must change IN LOCKSTEP with Java — it is a
// shared wire-adjacent dialect property, not a Go-only choice.)

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestFDB_ArithPrecedenceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_arithprec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_arithprec")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE arithprec CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_arithprec/s WITH TEMPLATE arithprec")
	dsn := "fdbsql:///testdb_arithprec?cluster_file=" + clusterFilePath + "&schema=s"
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 3)")

	scalar := func(expr string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	for _, c := range []struct {
		expr string
		want int64 // left-to-right evaluation (no precedence)
	}{
		{"2 + 3 * 4", 20},     // (2+3)*4, NOT 14
		{"2 + (3 * 4)", 14},   // parens force mult-first
		{"2 * 3 + 4 * 5", 50}, // ((2*3)+4)*5
		{"20 - 4 * 3", 48},    // (20-4)*3
		{"2 + 3 * 4 - 1", 19}, // ((2+3)*4)-1
		{"10 - 3 - 2", 5},     // left-assoc subtraction
		{"100 / 10 / 2", 5},   // left-assoc division
		{"10 % 3 + 1", 2},     // (10%3)+1
		{"2 + 12 / 3 - 1", 3}, // ((2+12)/3)-1 = 4-1
		{"2 + a * 4", 20},     // a=3, column: (2+3)*4 — left-to-right at runtime too
		{"-5 + 3", -2},        // negative literal
		{"2 * -3", -6},        // negative literal as operand
		{"5 - -3", 8},         // subtract a negative literal
	} {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			if got := scalar(c.expr); got != c.want {
				t.Errorf("%s = %d, want %d (fdb-relational evaluates math left-to-right; matches Java)", c.expr, got, c.want)
			}
		})
	}

	// No unary-minus OPERATOR — only negative literals. `-(expr)` / `- -5` reject.
	rejected := func(name, expr string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1")
			if err == nil || !strings.Contains(err.Error(), "42601") {
				t.Errorf("%s error = %v, want 42601 (no unary-minus operator; only negative literals)", expr, err)
			}
		})
	}
	rejected("unary_minus_on_parens", "-(3 + 2)")
	rejected("double_unary_minus", "- -5")
}
