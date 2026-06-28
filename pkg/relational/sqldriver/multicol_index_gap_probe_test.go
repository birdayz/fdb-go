package sqldriver_test

// Probes multi-column index (a,b,c) SARG prefix matching with GAPS: an unbound
// middle column (a=1 AND c>5, b free) can only use the a=1 prefix with c>5 as a
// residual; a missing leading column (b=2 alone) cannot use the index prefix at
// all. These prefix/residual boundaries are a classic wrong-rows source.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_MultiColIndexGapProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mcgap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mcgap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mcgap "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_abc ON t (a, b, c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mcgap/s WITH TEMPLATE mcgap")
	dsn := fmt.Sprintf("fdbsql:///testdb_mcgap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id1 (1,2,10) id2 (1,2,20) id3 (1,3,5) id4 (2,2,10)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,c) VALUES (1,1,2,10),(2,1,2,20),(3,1,3,5),(4,2,2,10)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
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
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// a=1 with c>8 but NO constraint on b (gap): a=1 prefix, c>8 residual.
	// a=1 rows: id1(c10),id2(c20),id3(c5); c>8 → id1,id2.
	ck("prefix_gap_residual", "SELECT id FROM t WHERE a = 1 AND c > 8", []int64{1, 2})
	// full prefix a=1,b=2 + range c>15: id2(c20).
	ck("full_prefix_range", "SELECT id FROM t WHERE a = 1 AND b = 2 AND c > 15", []int64{2})
	// a=1, b>2 (range on 2nd col), c free: id3(b3).
	ck("prefix_then_range", "SELECT id FROM t WHERE a = 1 AND b > 2", []int64{3})
	// b=2 alone (no leading a): cannot use index prefix → full scan + residual.
	ck("missing_leading_col", "SELECT id FROM t WHERE b = 2", []int64{1, 2, 4})
	// a=1 AND b=2 (prefix only): id1,id2.
	ck("two_col_prefix", "SELECT id FROM t WHERE a = 1 AND b = 2", []int64{1, 2})
	// c>8 alone (no a,b): full scan + residual: id1(10),id2(20),id4(10).
	ck("only_trailing_col", "SELECT id FROM t WHERE c > 8", []int64{1, 2, 4})
	// a=1 AND c=20 (gap on b, equality on c): a=1 prefix + c=20 residual → id2.
	ck("prefix_gap_eq_residual", "SELECT id FROM t WHERE a = 1 AND c = 20", []int64{2})
}
