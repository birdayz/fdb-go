package sqldriver_test

// Probes index maintenance across NULL transitions on UPDATE: setting an indexed
// column to NULL removes its old value entry and adds a NULL entry (so the value is
// no longer found by `= v` but is by IS NULL); setting it back to a value reverses
// that. Verified via index-backed lookups before/after.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_UpdateSetNullIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_usni")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_usni")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE usni CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_usni/s WITH TEMPLATE usni")
	dsn := fmt.Sprintf("fdbsql:///testdb_usni?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,10)")

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

	// initial state
	if !eq(ids("a = 10"), []int64{1, 3}) {
		t.Fatalf("initial a=10 = %v, want [1 3]", ids("a = 10"))
	}

	mwjoMustExec(t, db, ctx, "UPDATE t SET a = NULL WHERE id = 1")
	t.Run("value_to_null", func(t *testing.T) {
		if got := ids("a = 10"); !eq(got, []int64{3}) {
			t.Errorf("after SET NULL, a=10 = %v, want [3] (old {10} entry removed)", got)
		}
		if got := ids("a IS NULL"); !eq(got, []int64{1}) {
			t.Errorf("after SET NULL, a IS NULL = %v, want [1] (NULL entry added)", got)
		}
		if got := ids("a IS NOT NULL"); !eq(got, []int64{2, 3}) {
			t.Errorf("after SET NULL, a IS NOT NULL = %v, want [2 3]", got)
		}
	})

	mwjoMustExec(t, db, ctx, "UPDATE t SET a = 99 WHERE id = 1")
	t.Run("null_to_value", func(t *testing.T) {
		if got := ids("a = 99"); !eq(got, []int64{1}) {
			t.Errorf("after SET 99, a=99 = %v, want [1] (new entry)", got)
		}
		if got := ids("a IS NULL"); len(got) != 0 {
			t.Errorf("after SET 99, a IS NULL = %v, want empty (NULL entry removed)", got)
		}
	})
}
