package sqldriver_test

// Probes CAST: numeric widening/narrowing, CAST in SELECT and WHERE, CAST of
// NULL → NULL, and an explicit cross-type CAST equality that the implicit form
// can't currently do (workaround for the documented cross-type-SARG gap).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_CastProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cast")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cast")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE casttbl "+
			"CREATE TABLE t (id BIGINT NOT NULL, n BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cast/s WITH TEMPLATE casttbl")
	dsn := fmt.Sprintf("fdbsql:///testdb_cast?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, n, d) VALUES (1, 5, 5.0), (2, 7, 7.5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3)") // n,d NULL

	t.Run("cast_bigint_to_double_select", func(t *testing.T) {
		var d float64
		if err := db.QueryRowContext(ctx, "SELECT CAST(n AS DOUBLE) FROM t WHERE id = 1").Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(d-5.0) > 1e-9 {
			t.Errorf("CAST(5 AS DOUBLE) = %v, want 5.0", d)
		}
	})
	t.Run("cast_null_is_null", func(t *testing.T) {
		var d sql.NullFloat64
		if err := db.QueryRowContext(ctx, "SELECT CAST(n AS DOUBLE) FROM t WHERE id = 3").Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if d.Valid {
			t.Errorf("CAST(NULL AS DOUBLE) = %v, want NULL", d.Float64)
		}
	})
	t.Run("cast_double_to_bigint_rounds_half_up", func(t *testing.T) {
		// CAST(double AS BIGINT) ROUNDS half-up toward +inf (floor(x+0.5)), matching
		// Java CastValue.java's Math.round — NOT truncation. id=2 has d=7.5 → 8.
		var id sql.NullInt64
		err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE CAST(d AS BIGINT) = 8").Scan(&id)
		if err == sql.ErrNoRows {
			t.Fatal("CAST(7.5 AS BIGINT)=8 matched nothing; expected id=2 (Math.round half-up)")
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !id.Valid || id.Int64 != 2 {
			t.Errorf("CAST(d AS BIGINT)=8 → id=%v, want 2 (7.5 rounds to 8)", id.Int64)
		}
		var id5 sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE CAST(d AS BIGINT) = 5").Scan(&id5); err != nil {
			t.Fatalf("scan id5: %v", err)
		}
		if !id5.Valid || id5.Int64 != 1 {
			t.Errorf("CAST(5.0 AS BIGINT)=5 → id=%v, want 1", id5.Int64)
		}
	})
	t.Run("cast_workaround_crosstype_eq", func(t *testing.T) {
		// Explicit CAST is the documented workaround for the cross-type-SARG gap:
		// CAST(n AS DOUBLE) = d matches where the numeric values are equal (id=1: 5=5.0).
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE CAST(n AS DOUBLE) = d")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			got = append(got, v)
		}
		if len(got) != 1 || got[0] != 1 {
			t.Errorf("CAST(n AS DOUBLE)=d → %v, want [1] (5=5.0; 7≠7.5)", got)
		}
	})
}
