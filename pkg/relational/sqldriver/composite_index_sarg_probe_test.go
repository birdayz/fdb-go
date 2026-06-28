package sqldriver_test

// Probes SARG building over a composite index (a, b): prefix-equality, prefix-eq +
// range on the suffix, BETWEEN on the suffix, a range on the prefix with a residual
// equality on the suffix, and a suffix-only predicate (no usable prefix). All
// return the correct rows.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CompositeIndexSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cis")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cis")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cis CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cis/s WITH TEMPLATE cis")
	dsn := fmt.Sprintf("fdbsql:///testdb_cis?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,10),(5,2,20)")

	ids := func(where string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("WHERE %s: %v", where, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
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
	ck := func(name, where string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(where); !eq(got, want) {
				t.Errorf("WHERE %s = %v, want %v", where, got, want)
			}
		})
	}

	ck("prefix_eq", "a = 1", []int64{1, 2, 3})
	ck("prefix_eq_suffix_range", "a = 1 AND b > 15", []int64{2, 3})
	ck("prefix_eq_suffix_between", "a = 1 AND b BETWEEN 15 AND 25", []int64{2})
	ck("prefix_eq_suffix_lt", "a = 1 AND b < 25", []int64{1, 2})
	ck("prefix_eq2_suffix_gte", "a = 2 AND b >= 20", []int64{5})
	ck("prefix_range_suffix_residual_eq", "a >= 1 AND b = 20", []int64{2, 5})
	ck("suffix_only_no_prefix", "b = 10", []int64{1, 4})
}
