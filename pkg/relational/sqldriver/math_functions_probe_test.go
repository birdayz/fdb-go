package sqldriver_test

// Probes the scalar math function set: ABS, CEILING/CEIL, FLOOR, ROUND (incl.
// precision and half-up tie-breaking), POWER/POW, SQRT, MOD, EXP, LN. ROUND uses
// round-half-UP (3.5→4, 2.5→3), matching Java's CAST/Math.round rounding (NOT
// banker's round-half-to-even).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_MathFunctionsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mathfn")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mathfn")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mathfn CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mathfn/s WITH TEMPLATE mathfn")
	dsn := fmt.Sprintf("fdbsql:///testdb_mathfn?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")

	val := func(expr string) float64 {
		var v float64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	for _, c := range []struct {
		expr string
		want float64
	}{
		{"ABS(-5)", 5},
		{"ABS(-5.5)", 5.5},
		{"CEILING(3.2)", 4},
		{"CEIL(3.2)", 4},
		{"FLOOR(3.8)", 3},
		{"ROUND(3.5)", 4},
		{"ROUND(2.5)", 3},
		{"ROUND(3.14159, 2)", 3.14}, // half-up
		{"POWER(2, 3)", 8},
		{"POW(2, 3)", 8},
		{"SQRT(16)", 4},
		{"MOD(7, 3)", 1},
		{"EXP(0)", 1},
		{"LN(1)", 0},
	} {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			if got := val(c.expr); math.Abs(got-c.want) > 1e-9 {
				t.Errorf("%s = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}
