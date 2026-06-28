package sqldriver_test

// Probes arithmetic semantics + error paths: integer division truncates toward
// zero (incl. negatives), modulo sign, integer division/modulo by zero → 22012,
// and integer overflow → 22003 (Java uses Math.addExact/multiplyExact → throw).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ArithmeticEdgeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_arith")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_arith")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE arith "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_arith/s WITH TEMPLATE arith")
	dsn := fmt.Sprintf("fdbsql:///testdb_arith?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 7, 2), (2, 9223372036854775807, 1), (3, 7, 0), (4, -7, 2)")

	scalar := func(q string) (int64, error) {
		var v int64
		err := db.QueryRowContext(ctx, q).Scan(&v)
		return v, err
	}

	t.Run("int_div_truncates", func(t *testing.T) {
		if v, err := scalar("SELECT a / b FROM t WHERE id = 1"); err != nil || v != 3 {
			t.Errorf("7/2 = %d, err=%v, want 3", v, err)
		}
	})
	t.Run("int_div_negative_truncates_toward_zero", func(t *testing.T) {
		if v, err := scalar("SELECT a / b FROM t WHERE id = 4"); err != nil || v != -3 {
			t.Errorf("-7/2 = %d, err=%v, want -3 (toward zero)", v, err)
		}
	})
	t.Run("mod_positive", func(t *testing.T) {
		if v, err := scalar("SELECT a % b FROM t WHERE id = 1"); err != nil || v != 1 {
			t.Errorf("7%%2 = %d, err=%v, want 1", v, err)
		}
	})
	t.Run("mod_negative_sign", func(t *testing.T) {
		if v, err := scalar("SELECT a % b FROM t WHERE id = 4"); err != nil || v != -1 {
			t.Errorf("-7%%2 = %d, err=%v, want -1", v, err)
		}
	})
	t.Run("div_by_zero_22012", func(t *testing.T) {
		_, err := scalar("SELECT a / b FROM t WHERE id = 3")
		if err == nil {
			t.Fatal("7/0 succeeded; want division-by-zero error")
		}
		if !strings.Contains(err.Error(), "22012") {
			t.Errorf("7/0 error = %v, want SQLSTATE 22012", err)
		}
	})
	t.Run("mod_by_zero_22012", func(t *testing.T) {
		_, err := scalar("SELECT a % b FROM t WHERE id = 3")
		if err == nil {
			t.Fatal("7%0 succeeded; want division-by-zero error")
		}
		if !strings.Contains(err.Error(), "22012") {
			t.Errorf("7%%0 error = %v, want SQLSTATE 22012", err)
		}
	})
	t.Run("add_overflow_22003", func(t *testing.T) {
		_, err := scalar("SELECT a + b FROM t WHERE id = 2") // MAX + 1
		if err == nil {
			t.Fatal("MAX+1 succeeded; want overflow error")
		}
		if !strings.Contains(err.Error(), "22003") {
			t.Errorf("MAX+1 error = %v, want SQLSTATE 22003", err)
		}
	})
	t.Run("mul_overflow_22003", func(t *testing.T) {
		_, err := scalar("SELECT a * 2 FROM t WHERE id = 2") // MAX * 2
		if err == nil {
			t.Fatal("MAX*2 succeeded; want overflow error")
		}
		if !strings.Contains(err.Error(), "22003") {
			t.Errorf("MAX*2 error = %v, want SQLSTATE 22003", err)
		}
	})
	t.Run("float_division", func(t *testing.T) {
		var v float64
		if err := db.QueryRowContext(ctx, "SELECT CAST(a AS DOUBLE) / CAST(b AS DOUBLE) FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("7.0/2.0: %v", err)
		}
		if v != 3.5 {
			t.Errorf("7.0/2.0 = %v, want 3.5", v)
		}
	})
}
