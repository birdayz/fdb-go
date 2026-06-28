package sqldriver_test

// Probes constant / always-true / always-false predicates and their combination
// with column predicates (constant folding — the area that produced the LIMIT 0
// bug). `WHERE 1=0` → none, `WHERE 1=1` → all, and AND/OR folding with a real
// predicate must not drop or add rows.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ConstantPredicateProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_constpred")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_constpred")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE constpred "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_constpred/s WITH TEMPLATE constpred")
	dsn := fmt.Sprintf("fdbsql:///testdb_constpred?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,1),(2,2),(3,3),(4,4)")

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

	all := []int64{1, 2, 3, 4}
	ck("false_const_eq", "SELECT id FROM t WHERE 1 = 0", nil)
	ck("true_const_eq", "SELECT id FROM t WHERE 1 = 1", all)
	ck("false_const_cmp", "SELECT id FROM t WHERE 2 > 5", nil)
	ck("true_const_cmp", "SELECT id FROM t WHERE 5 > 2", all)
	ck("false_or_col", "SELECT id FROM t WHERE 1 = 0 OR a = 2", []int64{2})
	ck("true_and_col", "SELECT id FROM t WHERE 1 = 1 AND a = 2", []int64{2})
	ck("col_or_true", "SELECT id FROM t WHERE a = 2 OR 1 = 1", all)
	ck("col_and_false", "SELECT id FROM t WHERE a = 2 AND 1 = 0", nil)
	ck("false_and_col", "SELECT id FROM t WHERE 1 = 0 AND a = 2", nil)
	ck("true_or_col", "SELECT id FROM t WHERE 1 = 1 OR a = 99", all)
	// always-false on an indexed column predicate combined with a real range.
	ck("range_and_false", "SELECT id FROM t WHERE a > 1 AND 0 = 1", nil)
	ck("range_or_false", "SELECT id FROM t WHERE a > 2 OR 0 = 1", []int64{3, 4})
}
