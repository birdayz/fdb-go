package sqldriver_test

// Probes DML with a subquery in WHERE: correlated EXISTS / NOT EXISTS work in
// DELETE and UPDATE (delete/update exactly the matching / non-matching rows), while
// an IN-subquery is cleanly rejected (0AF00) — consistent with the SELECT/JOIN-ON
// surface where IN-subqueries are unsupported. ref holds {1,3}; t holds {1,2,3}.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_DmlSubqueryWhereProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dswp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dswp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE dswp "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE ref (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dswp/s WITH TEMPLATE dswp")
	dsn := fmt.Sprintf("fdbsql:///testdb_dswp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	reset := func() {
		if _, err := db.ExecContext(ctx, "DELETE FROM t"); err != nil {
			t.Fatalf("reset t: %v", err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM ref"); err != nil {
			t.Fatalf("reset ref: %v", err)
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")
		mwjoMustExec(t, db, ctx, "INSERT INTO ref (id, flag) VALUES (1,1),(3,1)")
	}
	remainingIDs := func() []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t")
		if err != nil {
			t.Fatalf("scan ids: %v", err)
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

	t.Run("delete_where_exists_correlated", func(t *testing.T) {
		reset()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE EXISTS (SELECT 1 FROM ref WHERE ref.id = t.id)")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Errorf("RowsAffected = %d, want 2", n)
		}
		if got := remainingIDs(); !eq(got, []int64{2}) {
			t.Errorf("remaining = %v, want [2]", got)
		}
	})
	t.Run("delete_where_not_exists", func(t *testing.T) {
		reset()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE NOT EXISTS (SELECT 1 FROM ref WHERE ref.id = t.id)")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("RowsAffected = %d, want 1", n)
		}
		if got := remainingIDs(); !eq(got, []int64{1, 3}) {
			t.Errorf("remaining = %v, want [1 3]", got)
		}
	})
	t.Run("update_where_exists_correlated", func(t *testing.T) {
		reset()
		res, err := db.ExecContext(ctx, "UPDATE t SET a = 99 WHERE EXISTS (SELECT 1 FROM ref WHERE ref.id = t.id)")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Errorf("RowsAffected = %d, want 2", n)
		}
		// ids 1,3 → a=99; id 2 unchanged (20).
		var updated int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE a = 99").Scan(&updated); err != nil {
			t.Fatalf("count updated: %v", err)
		}
		if updated != 2 {
			t.Errorf("rows with a=99 = %d, want 2", updated)
		}
	})
	t.Run("delete_where_in_subquery_rejected", func(t *testing.T) {
		reset()
		_, err := db.ExecContext(ctx, "DELETE FROM t WHERE id IN (SELECT id FROM ref WHERE flag = 1)")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("DELETE … WHERE id IN (subquery) err = %v, want 0AF00 (unsupported)", err)
		}
	})
}
