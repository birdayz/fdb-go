package sqldriver_test

// Probes UPDATE/DELETE correctness (wire-relevant — these mutate stored records
// and index entries): targeted updates leave other rows untouched, deletes
// remove only matching rows, an indexed column's index entry is maintained after
// UPDATE (so an index probe finds the new value and not the old), and no-match
// UPDATE/DELETE are no-ops.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_DMLUpdateDeleteProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dml")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dml")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dml "+
			"CREATE TABLE t (id BIGINT NOT NULL, grp BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dml/s WITH TEMPLATE dml")
	dsn := fmt.Sprintf("fdbsql:///testdb_dml?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	reset := func() {
		mwjoMustExec(t, db, ctx, "DELETE FROM t")
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, grp, v) VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30), (4, 2, 40), (5, 3, 50)")
	}
	idsWhere := func(q string) []int64 {
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

	t.Run("update_targeted_leaves_others", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = 999 WHERE id = 3")
		if got := idsWhere("SELECT id FROM t WHERE v = 999"); !eq(got, []int64{3}) {
			t.Errorf("after UPDATE id=3: v=999 rows = %v, want [3]", got)
		}
		// others unchanged
		if got := idsWhere("SELECT id FROM t WHERE v = 30"); len(got) != 0 {
			t.Errorf("old value v=30 still present in %v, want gone", got)
		}
		if got := idsWhere("SELECT id FROM t WHERE v IN (10, 20, 40, 50)"); !eq(got, []int64{1, 2, 4, 5}) {
			t.Errorf("other rows changed: %v, want [1 2 4 5]", got)
		}
	})

	t.Run("update_index_entry_maintained", func(t *testing.T) {
		reset()
		// Move id=1 from v=10 to v=35; the t_v index probe must find it at 35, not 10.
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = 35 WHERE id = 1")
		if got := idsWhere("SELECT id FROM t WHERE v = 35"); !eq(got, []int64{1}) {
			t.Errorf("index probe v=35 = %v, want [1] (stale index entry?)", got)
		}
		if got := idsWhere("SELECT id FROM t WHERE v = 10"); len(got) != 0 {
			t.Errorf("stale index entry v=10 = %v, want none", got)
		}
		// range probe spanning the moved value
		if got := idsWhere("SELECT id FROM t WHERE v BETWEEN 30 AND 40"); !eq(got, []int64{1, 3, 4}) {
			t.Errorf("range [30,40] = %v, want [1 3 4]", got)
		}
	})

	t.Run("update_by_group_multiple_rows", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = 0 WHERE grp = 2")
		if got := idsWhere("SELECT id FROM t WHERE v = 0"); !eq(got, []int64{3, 4}) {
			t.Errorf("UPDATE grp=2 SET v=0: v=0 rows = %v, want [3 4]", got)
		}
	})

	t.Run("delete_targeted", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE grp = 1")
		if got := idsWhere("SELECT id FROM t"); !eq(got, []int64{3, 4, 5}) {
			t.Errorf("after DELETE grp=1: remaining = %v, want [3 4 5]", got)
		}
		// index reflects deletion
		if got := idsWhere("SELECT id FROM t WHERE v < 25"); len(got) != 0 {
			t.Errorf("deleted rows still index-visible: %v", got)
		}
	})

	t.Run("update_no_match_noop", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = 1 WHERE id = 999")
		if got := idsWhere("SELECT id FROM t WHERE v = 1"); len(got) != 0 {
			t.Errorf("no-match UPDATE changed rows: %v", got)
		}
		if got := idsWhere("SELECT id FROM t"); !eq(got, []int64{1, 2, 3, 4, 5}) {
			t.Errorf("no-match UPDATE lost rows: %v", got)
		}
	})

	t.Run("delete_no_match_noop", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE v > 10000")
		if got := idsWhere("SELECT id FROM t"); !eq(got, []int64{1, 2, 3, 4, 5}) {
			t.Errorf("no-match DELETE removed rows: %v", got)
		}
	})

	t.Run("delete_all", func(t *testing.T) {
		reset()
		mwjoMustExec(t, db, ctx, "DELETE FROM t")
		if got := idsWhere("SELECT id FROM t"); len(got) != 0 {
			t.Errorf("DELETE all left %v", got)
		}
	})
}
