package sqldriver_test

// Probes secondary-index maintenance on UPDATE/DELETE (wire-critical): changing an
// indexed column must REMOVE the old index entry and ADD the new one, so a query
// via the index sees the new value and NOT the stale old value. A missing
// old-entry delete would silently return wrong rows. Also covers DELETE removing
// the index entry, and updating a covering compound index.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_UpdateIndexMaintenanceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_updidx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_updidx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE updidx "+
			"CREATE TABLE t (id BIGINT NOT NULL, status STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_status ON t (status) CREATE INDEX t_amount ON t (amount)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_updidx/s WITH TEMPLATE updidx")
	dsn := fmt.Sprintf("fdbsql:///testdb_updidx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, status, amount) VALUES (1, 'pending', 100), (2, 'pending', 200), (3, 'shipped', 300)")

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

	// Baseline via the status index.
	if got := ids("SELECT id FROM t WHERE status = 'pending'"); !eq(got, []int64{1, 2}) {
		t.Fatalf("baseline status='pending' = %v, want [1 2]", got)
	}

	// UPDATE id=1 pending → shipped: the OLD index entry must be removed.
	mwjoMustExec(t, db, ctx, "UPDATE t SET status = 'shipped' WHERE id = 1")

	t.Run("old_index_entry_removed", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE status = 'pending'"); !eq(got, []int64{2}) {
			t.Errorf("after UPDATE, status='pending' = %v, want [2] (id=1's stale entry must be gone)", got)
		}
	})
	t.Run("new_index_entry_added", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE status = 'shipped'"); !eq(got, []int64{1, 3}) {
			t.Errorf("after UPDATE, status='shipped' = %v, want [1 3]", got)
		}
	})

	// UPDATE an indexed numeric column too (range-scan index).
	mwjoMustExec(t, db, ctx, "UPDATE t SET amount = 999 WHERE id = 2")
	t.Run("numeric_index_old_gone", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE amount = 200"); len(got) != 0 {
			t.Errorf("after UPDATE amount, amount=200 = %v, want [] (stale entry gone)", got)
		}
	})
	t.Run("numeric_index_range_sees_new", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE amount > 500"); !eq(got, []int64{2}) {
			t.Errorf("after UPDATE amount, amount>500 = %v, want [2] (new value 999)", got)
		}
	})

	// DELETE must remove index entries.
	mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id = 3")
	t.Run("delete_removes_index_entry", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE status = 'shipped'"); !eq(got, []int64{1}) {
			t.Errorf("after DELETE id=3, status='shipped' = %v, want [1] (id=3's entry gone)", got)
		}
	})
}
