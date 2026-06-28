package sqldriver_test

// Probes integer-overflow safety: SUM over values exceeding int64 and arithmetic
// (MaxInt64 + 1) both raise 22003 (overflow) rather than silently wrapping. A
// non-overflowing SUM works, and MaxInt64 itself round-trips.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_OverflowProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ovf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ovf")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ovf CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ovf/s WITH TEMPLATE ovf")
	dsn := fmt.Sprintf("fdbsql:///testdb_ovf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 9223372036854775807), (2, 1)")

	overflows := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "22003") {
				t.Errorf("%s error = %v, want 22003 (overflow, not silent wrap)", name, err)
			}
		})
	}
	overflows("sum_overflow", "SELECT SUM(v) FROM t")
	overflows("arith_overflow", "SELECT 9223372036854775807 + 1 FROM t WHERE id = 1")

	t.Run("non_overflow_sum_ok", func(t *testing.T) {
		var s int64
		if err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM t WHERE id = 2").Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s != 1 {
			t.Errorf("SUM over {1} = %d, want 1", s)
		}
	})
	t.Run("maxint_roundtrips", func(t *testing.T) {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != math.MaxInt64 {
			t.Errorf("MaxInt64 = %d, want %d", v, int64(math.MaxInt64))
		}
	})
}
