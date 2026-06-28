package sqldriver_test

// Probes correctness under multi-index selection: when several indexes apply to
// a query, the planner must return the SAME correct rows regardless of which it
// picks (index + residual filter on the rest). Also AND/OR across columns with
// different indexes.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_MultiIndexSelectionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_multiidx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_multiidx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE multiidx "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a) CREATE INDEX t_b ON t (b) CREATE INDEX t_ac ON t (a, c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_multiidx/s WITH TEMPLATE multiidx")
	dsn := fmt.Sprintf("fdbsql:///testdb_multiidx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id : (a,b,c)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b,c) VALUES "+
		"(1,1,2,3),(2,1,2,9),(3,1,7,3),(4,5,2,3),(5,5,5,5),(6,1,2,3)")

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

	// a=1 AND b=2: candidates t_a, t_b. rows with a=1&b=2: id1,id2,id6.
	ck("and_two_indexed_cols", "SELECT id FROM t WHERE a = 1 AND b = 2", []int64{1, 2, 6})
	// a=1 AND c=3: t_ac covers both. rows: id1,id3,id6 (a=1,c=3).
	ck("and_composite_index", "SELECT id FROM t WHERE a = 1 AND c = 3", []int64{1, 3, 6})
	// b=2 AND c=3: t_b + residual c (no c-only index). rows a-any,b=2,c=3: id1,id4,id6.
	ck("indexed_plus_residual", "SELECT id FROM t WHERE b = 2 AND c = 3", []int64{1, 4, 6})
	// a=1 OR b=5: union of t_a(a=1: 1,2,3,6) and t_b(b=5: 5). → 1,2,3,5,6.
	ck("or_two_indexed_cols", "SELECT id FROM t WHERE a = 1 OR b = 5", []int64{1, 2, 3, 5, 6})
	// a=1 AND b=2 AND c=3: id1,id6.
	ck("three_col_and", "SELECT id FROM t WHERE a = 1 AND b = 2 AND c = 3", []int64{1, 6})
	// a=5 AND c=3 (composite, a=5): id4.
	ck("composite_other_value", "SELECT id FROM t WHERE a = 5 AND c = 3", []int64{4})
}
