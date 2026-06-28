package sqldriver_test

// Probes expression-based ORDER BY: a CASE bucket (conditional ordering) as the
// primary sort key with a tiebreaker, the same CASE bucket DESC, and an arithmetic
// (a % 2) sort expression. All order correctly.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CaseOrderByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cobp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cobp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE cobp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cobp/s WITH TEMPLATE cobp")
	dsn := fmt.Sprintf("fdbsql:///testdb_cobp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30),(4,5)")

	ids := func(orderBy string) []int64 {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY "+orderBy)
		if err != nil {
			t.Fatalf("ORDER BY %s: %v", orderBy, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			o = append(o, v)
		}
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
	ck := func(name, orderBy string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(orderBy); !eq(got, want) {
				t.Errorf("ORDER BY %s = %v, want %v", orderBy, got, want)
			}
		})
	}

	// bucket 0 = a>15 (20→id2, 30→id3), bucket 1 = a<=15 (5→id4, 10→id1); tiebreak by a.
	ck("case_bucket_asc", "CASE WHEN a > 15 THEN 0 ELSE 1 END, a", []int64{2, 3, 4, 1})
	ck("case_bucket_desc", "CASE WHEN a > 15 THEN 0 ELSE 1 END DESC, a", []int64{4, 1, 2, 3})
	// a%2: even (10,20,30 → ids 1,2,3) then odd (5 → id4), tiebreak by a.
	ck("modulo_expr", "a % 2, a", []int64{1, 2, 3, 4})
}
