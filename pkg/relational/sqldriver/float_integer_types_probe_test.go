package sqldriver_test

// Pins the 32-bit numeric column types (distinct from DOUBLE/BIGINT): FLOAT is a
// 32-bit float (proto float) — 0.1 round-trips with float32 precision
// (0.10000000149011612), NOT the float64 0.1 — and INTEGER is a 32-bit int (proto
// int32) — max int32 round-trips, but 2^31 and beyond overflow with a clean 22003.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_FloatIntegerTypesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fit")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fit")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fit CREATE TABLE t (id BIGINT NOT NULL, f FLOAT, i INTEGER, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fit/s WITH TEMPLATE fit")
	dsn := fmt.Sprintf("fdbsql:///testdb_fit?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Run("float_is_32bit_precision", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, f) VALUES (1, 0.1)")
		var f float64
		if err := db.QueryRowContext(ctx, "SELECT f FROM t WHERE id = 1").Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if f != float64(float32(0.1)) {
			t.Errorf("FLOAT 0.1 = %.17g, want %.17g (float32 precision)", f, float64(float32(0.1)))
		}
		if f == 0.1 {
			t.Errorf("FLOAT 0.1 stored at full float64 precision — not 32-bit")
		}
	})
	t.Run("float_exact_value_roundtrips", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, f) VALUES (2, 1.5)") // exact in float32
		var f float64
		if err := db.QueryRowContext(ctx, "SELECT f FROM t WHERE id = 2").Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if f != 1.5 {
			t.Errorf("FLOAT 1.5 = %v, want 1.5", f)
		}
	})
	t.Run("integer_max_int32_roundtrips", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, i) VALUES (3, 2147483647)")
		var i int64
		if err := db.QueryRowContext(ctx, "SELECT i FROM t WHERE id = 3").Scan(&i); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i != 2147483647 {
			t.Errorf("INTEGER max int32 = %d, want 2147483647", i)
		}
	})
	overflow := func(name string, v int64) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, "INSERT INTO t (id, i) VALUES (?, ?)", v, v)
			if err == nil || !strings.Contains(err.Error(), "22003") {
				t.Errorf("INSERT INTEGER %d error = %v, want 22003 (int32 overflow)", v, err)
			}
		})
	}
	overflow("integer_overflow_2pow31", 2147483648)
	overflow("integer_overflow_5e9", 5000000000)
}
