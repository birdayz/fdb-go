package sqldriver_test

// Probes IS NULL / IS NOT NULL on an indexed column (the null-index-entry seek
// path): IS NULL finds the null-keyed rows, IS NOT NULL the rest, and both combine
// correctly with ranges (OR / AND) and NOT. NULL index entries sort first; the
// SARG must seek/exclude them precisely.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_IsNullIndexedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inip")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inip")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inip CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inip/s WITH TEMPLATE inip")
	dsn := fmt.Sprintf("fdbsql:///testdb_inip?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(3,20),(5,30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (2),(4)") // a NULL

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
			if got := ids(where); !eq(got, want) {
				t.Errorf("WHERE %s = %v, want %v", where, got, want)
			}
		})
	}

	ck("is_null", "a IS NULL", []int64{2, 4})
	ck("is_not_null", "a IS NOT NULL", []int64{1, 3, 5})
	ck("is_null_or_range", "a IS NULL OR a > 25", []int64{2, 4, 5})
	ck("is_not_null_and_range", "a IS NOT NULL AND a < 25", []int64{1, 3})
	ck("not_is_null", "NOT (a IS NULL)", []int64{1, 3, 5})
}
