package sqldriver_test

// Probes WITH RECURSIVE hierarchy traversal correctness (transitive closure over
// a manager tree) — the recursive UNION ALL must reach every transitive child
// exactly once and terminate. Additive to the existing depth-limit coverage.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_RecursiveCTEHierarchy(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rctehier")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rctehier")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rctehier "+
			"CREATE TABLE emp (id BIGINT NOT NULL, mgr BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX emp_mgr ON emp (mgr)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rctehier/s WITH TEMPLATE rctehier")
	dsn := fmt.Sprintf("fdbsql:///testdb_rctehier?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// tree: 1 -> {2,3}; 2 -> {4}; 4 -> {5}; 3 is a leaf.
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, mgr) VALUES (2,1),(3,1),(4,2),(5,4)")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id) VALUES (1)")

	traverse := func(rootID int64) []int64 {
		q := fmt.Sprintf("WITH RECURSIVE reports AS ("+
			"SELECT id FROM emp WHERE id = %d "+
			"UNION ALL "+
			"SELECT e.id FROM emp e, reports r WHERE e.mgr = r.id) "+
			"SELECT id FROM reports", rootID)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("recursive CTE: %v", err)
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

	t.Run("full_tree_from_root", func(t *testing.T) {
		if got := traverse(1); !eq(got, []int64{1, 2, 3, 4, 5}) {
			t.Errorf("traverse(1) = %v, want [1 2 3 4 5]", got)
		}
	})
	t.Run("subtree_from_mid", func(t *testing.T) {
		// from id=2: 2 -> 4 -> 5.
		if got := traverse(2); !eq(got, []int64{2, 4, 5}) {
			t.Errorf("traverse(2) = %v, want [2 4 5]", got)
		}
	})
	t.Run("leaf_only", func(t *testing.T) {
		// from id=3 (leaf): just 3.
		if got := traverse(3); !eq(got, []int64{3}) {
			t.Errorf("traverse(3) = %v, want [3]", got)
		}
	})
}
