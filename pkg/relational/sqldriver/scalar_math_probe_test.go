package sqldriver_test

// Probes scalar math functions: ABS, MOD (sign + by-zero), ROUND/FLOOR/CEIL,
// POWER, SQRT (positive + SQL §6.27 negative-arg error). Complements the
// arithmetic-operator probe.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_ScalarMathProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_smathp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_smathp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE smathp "+
			"CREATE TABLE t (id BIGINT NOT NULL, n BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_smathp/s WITH TEMPLATE smathp")
	dsn := fmt.Sprintf("fdbsql:///testdb_smathp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, n, d) VALUES (1, -7, 2.5)")

	f := func(expr string) float64 {
		var v float64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	ckf := func(name, expr string, want float64) {
		t.Run(name, func(t *testing.T) {
			if got := f(expr); math.Abs(got-want) > 1e-9 {
				t.Errorf("%s = %v, want %v", expr, got, want)
			}
		})
	}

	ckf("abs_int", "ABS(n)", 7)
	ckf("abs_double", "ABS(d)", 2.5)
	ckf("mod_negative_sign", "MOD(n, 3)", -1) // -7 % 3 = -1 (truncated)
	ckf("round_half_up", "ROUND(d)", 3)       // 2.5 → 3 (matches CAST/Math.round)
	ckf("floor", "FLOOR(d)", 2)
	ckf("ceil", "CEIL(d)", 3)
	ckf("power", "POWER(2, 10)", 1024)
	ckf("sqrt", "SQRT(16)", 4)

	t.Run("mod_by_zero_22012", func(t *testing.T) {
		var v float64
		err := db.QueryRowContext(ctx, "SELECT MOD(n, 0) FROM t WHERE id = 1").Scan(&v)
		if err == nil || !strings.Contains(err.Error(), "22012") {
			t.Errorf("MOD(n,0) error = %v, want 22012 division-by-zero", err)
		}
	})
	t.Run("sqrt_negative_22023", func(t *testing.T) {
		// SQL §6.27: SQRT of a negative argument raises 22023 (Go extension; Java
		// has no SQL SQRT function).
		var v float64
		err := db.QueryRowContext(ctx, "SELECT SQRT(-1) FROM t WHERE id = 1").Scan(&v)
		if err == nil || !strings.Contains(err.Error(), "22023") {
			t.Errorf("SQRT(-1) error = %v, want 22023 invalid-argument", err)
		}
	})
}
