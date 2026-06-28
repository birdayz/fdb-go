package sqldriver_test

// Probes negative-integer ordering and range scans through an index (wire-relevant:
// FDB tuple encodes negative integers with a distinct scheme so they sort below
// positives; a bad encoding would order -1 after 100 or break sign-crossing
// ranges). Covers ORDER BY across the sign boundary, one-sided ranges, BETWEEN
// crossing zero, and equality on a negative.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NegativeIntIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_negint")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_negint")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE negint "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_negint/s WITH TEMPLATE negint")
	dsn := fmt.Sprintf("fdbsql:///testdb_negint?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// v: -9223372036854775808 (MinInt64), -100, -1, 0, 1, 100
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,-100),(2,-1),(3,0),(4,1),(5,100),(6,-9223372036854775808)")

	vals := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := vals(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("order_across_sign_boundary", "SELECT v FROM t ORDER BY v",
		[]int64{-9223372036854775808, -100, -1, 0, 1, 100})
	ck("order_desc", "SELECT v FROM t ORDER BY v DESC",
		[]int64{100, 1, 0, -1, -100, -9223372036854775808})
	ck("range_gt_negative", "SELECT v FROM t WHERE v > -50 ORDER BY v", []int64{-1, 0, 1, 100})
	ck("range_lt_zero", "SELECT v FROM t WHERE v < 0 ORDER BY v", []int64{-9223372036854775808, -100, -1})
	ck("between_crossing_zero", "SELECT v FROM t WHERE v BETWEEN -10 AND 10 ORDER BY v", []int64{-1, 0, 1})
	ck("eq_negative", "SELECT v FROM t WHERE v = -100", []int64{-100})
	ck("eq_min_int64", "SELECT v FROM t WHERE v = -9223372036854775808", []int64{-9223372036854775808})
}
