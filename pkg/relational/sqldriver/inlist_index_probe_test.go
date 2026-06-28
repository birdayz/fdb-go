package sqldriver_test

// Probes IN-list SARG over an indexed column (IN → a union of equality seeks):
// multi-value IN (with a duplicate-value match), single-value, no-match (empty),
// NOT IN, IN combined with an AND residual, and an IN covering all values.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_InListIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ilip")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ilip")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ilip CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ilip/s WITH TEMPLATE ilip")
	dsn := fmt.Sprintf("fdbsql:///testdb_ilip?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30),(4,40),(5,20)")

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
			got := ids(where)
			if len(want) == 0 {
				if len(got) != 0 {
					t.Errorf("WHERE %s = %v, want empty", where, got)
				}
				return
			}
			if !eq(got, want) {
				t.Errorf("WHERE %s = %v, want %v", where, got, want)
			}
		})
	}

	ck("in_multi_with_dup_match", "a IN (20, 40)", []int64{2, 4, 5})
	ck("in_single", "a IN (10)", []int64{1})
	ck("in_no_match", "a IN (99)", nil)
	ck("not_in", "a NOT IN (20, 40)", []int64{1, 3})
	ck("in_and_residual", "a IN (20, 40) AND id < 4", []int64{2})
	ck("in_covers_all", "a IN (10, 20, 30, 40)", []int64{1, 2, 3, 4, 5})
}
