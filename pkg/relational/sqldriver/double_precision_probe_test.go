package sqldriver_test

// Probes DOUBLE (IEEE-754) semantics: exact round-trip of 0.1 and 1e308; arithmetic
// follows binary floating point (0.1 + 0.2 = 0.30000000000000004, so the predicate
// 0.1 + 0.2 = 0.3 matches nothing); and double overflow produces +Inf rather than
// an error — in contrast to INTEGER overflow, which raises 22003.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_DoublePrecisionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dpp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dpp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dpp CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dpp/s WITH TEMPLATE dpp")
	dsn := fmt.Sprintf("fdbsql:///testdb_dpp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d) VALUES (1, 0.1), (2, 1.5), (3, 1e308)")

	dval := func(expr string, id int) float64 {
		var v float64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM t WHERE id = %d", expr, id)).Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	count := func(where string) int {
		var c int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE "+where).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", where, err)
		}
		return c
	}

	t.Run("exact_roundtrip", func(t *testing.T) {
		if got := dval("d", 1); got != 0.1 {
			t.Errorf("0.1 round-trip = %v", got)
		}
		if got := dval("d", 3); got != 1e308 {
			t.Errorf("1e308 round-trip = %v", got)
		}
	})
	t.Run("ieee_addition", func(t *testing.T) {
		got := dval("d + 0.2", 1) // 0.1 + 0.2
		// Compare against a RUNTIME float64 add (NOT the Go constant 0.1+0.2, which
		// folds to exact 0.3 at compile time). The engine must match IEEE binary fp:
		// 0.30000000000000004, i.e. NOT exactly 0.3 but within rounding of it.
		var a, b float64 = 0.1, 0.2
		if got != a+b {
			t.Errorf("0.1 + 0.2 = %v, want %v (IEEE runtime add)", got, a+b)
		}
		if got == 0.3 {
			t.Errorf("0.1 + 0.2 unexpectedly exactly 0.3 (should be 0.30000000000000004)")
		}
		if math.Abs(got-0.3) > 1e-10 {
			t.Errorf("0.1 + 0.2 = %v, not approximately 0.3", got)
		}
	})
	t.Run("float_equality_predicate", func(t *testing.T) {
		if c := count("0.1 + 0.2 = 0.3"); c != 0 {
			t.Errorf("WHERE 0.1+0.2=0.3 matched %d, want 0 (float inequality)", c)
		}
		if c := count("d = 0.1"); c != 1 {
			t.Errorf("WHERE d=0.1 matched %d, want 1", c)
		}
		if c := count("d > 1.0"); c != 2 {
			t.Errorf("WHERE d>1.0 matched %d, want 2 (1.5, 1e308)", c)
		}
	})
	t.Run("double_overflow_is_inf_not_error", func(t *testing.T) {
		got := dval("d * 10", 3) // 1e308 * 10 = 1e309 → +Inf
		if !math.IsInf(got, 1) {
			t.Errorf("1e308 * 10 = %v, want +Inf (IEEE overflow, not 22003)", got)
		}
	})
}
