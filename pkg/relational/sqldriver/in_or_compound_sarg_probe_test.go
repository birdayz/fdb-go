package sqldriver_test

// Probes specialized SARG plans: IN-lists (InJoin/InUnion), NOT IN, OR-to-union
// of ranges, and compound multi-column-index predicates (equality prefix + range
// on the next column). These are distinct planner paths from a single equality
// probe and are a common source of wrong-rows bugs.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_InOrCompoundSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_insarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_insarg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE insarg "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_insarg/s WITH TEMPLATE insarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_insarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id : (a,b) = (1,100) (2,200) (3,300) (4,400) (5,500) (3,310) (1,110)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,1,100),(2,2,200),(3,3,300),(4,4,400),(5,5,500),(6,3,310),(7,1,110)")

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

	// a IN (1,3,5): a=1 → id1,id7; a=3 → id3,id6; a=5 → id5.
	ck("in_list", "SELECT id FROM t WHERE a IN (1, 3, 5)", []int64{1, 3, 5, 6, 7})
	// IN with duplicate values must not duplicate rows.
	ck("in_list_dups", "SELECT id FROM t WHERE a IN (1, 1, 3)", []int64{1, 3, 6, 7})
	// NOT IN (2,4): everything except a=2 (id2), a=4 (id4).
	ck("not_in", "SELECT id FROM t WHERE a NOT IN (2, 4)", []int64{1, 3, 5, 6, 7})
	// a = 1 OR a = 5: id1,id7 (a=1), id5 (a=5).
	ck("or_equals", "SELECT id FROM t WHERE a = 1 OR a = 5", []int64{1, 5, 7})
	// a > 4 OR a < 2: a=5 (id5); a=1 (id1,id7).
	ck("or_ranges", "SELECT id FROM t WHERE a > 4 OR a < 2", []int64{1, 5, 7})
	// Compound: a = 3 AND b > 305 → id6 (a=3,b=310); id3 (b=300) excluded.
	ck("compound_eq_range", "SELECT id FROM t WHERE a = 3 AND b > 305", []int64{6})
	// Compound: a = 1 AND b = 110 → id7.
	ck("compound_eq_eq", "SELECT id FROM t WHERE a = 1 AND b = 110", []int64{7})
	// a IN (1,3) AND b >= 300 → a=3,b=300(id3),b=310(id6); a=1 has b=100,110 <300 → none.
	ck("in_and_range", "SELECT id FROM t WHERE a IN (1, 3) AND b >= 300", []int64{3, 6})
	// Empty IN-equivalent via impossible: a IN (99, 98) → none.
	ck("in_no_match", "SELECT id FROM t WHERE a IN (98, 99)", nil)
}
