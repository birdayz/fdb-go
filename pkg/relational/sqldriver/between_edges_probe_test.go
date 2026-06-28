package sqldriver_test

// Probes BETWEEN edge cases: normal inclusive range, REVERSED bounds (lo > hi →
// always empty, since BETWEEN x AND y ≡ v>=x AND v<=y), NOT BETWEEN (the
// complement), equal bounds (single value), and a wide range covering all.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_BetweenEdgesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_betw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_betw")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE betw CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_betw/s WITH TEMPLATE betw")
	dsn := fmt.Sprintf("fdbsql:///testdb_betw?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,5),(2,10),(3,15),(4,20),(5,25)")

	vs := func(q string) []int64 {
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
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
			if got := vs(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("between_normal", "SELECT v FROM t WHERE v BETWEEN 10 AND 20", []int64{10, 15, 20})
	ck("between_reversed_empty", "SELECT v FROM t WHERE v BETWEEN 20 AND 10", nil)
	ck("not_between", "SELECT v FROM t WHERE v NOT BETWEEN 10 AND 20", []int64{5, 25})
	ck("between_equal_bounds", "SELECT v FROM t WHERE v BETWEEN 15 AND 15", []int64{15})
	ck("between_covers_all", "SELECT v FROM t WHERE v BETWEEN -100 AND 100", []int64{5, 10, 15, 20, 25})
	ck("not_between_reversed_all", "SELECT v FROM t WHERE v NOT BETWEEN 20 AND 10", []int64{5, 10, 15, 20, 25})
}
