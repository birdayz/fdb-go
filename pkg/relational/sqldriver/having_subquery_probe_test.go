package sqldriver_test

// Probes a scalar subquery used as the comparand of a HAVING (and WHERE)
// predicate over aggregated data: HAVING SUM(v) > (SELECT AVG(v) FROM t) filters
// groups against the whole-table average; the same scalar subquery works in WHERE
// before grouping. Confirms scalar subqueries resolve correctly in the
// aggregate-filter position.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_HavingSubqueryProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_havsubp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_havsubp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE havsubp CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_havsubp/s WITH TEMPLATE havsubp")
	dsn := fmt.Sprintf("fdbsql:///testdb_havsubp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// AVG(v) over all = (10+20+30+1+2+3)/6 = 11 ; MAX(v) = 30
	// g1 sum30, g2 sum30, g3 sum6
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,g,v) VALUES (1,1,10),(2,1,20),(3,2,30),(4,3,1),(5,3,2),(6,3,3)")

	groups := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var g int64
			if err := rows.Scan(&g); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, g)
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
			if got := groups(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// SUM(v) > AVG(v)=11 → g1(30), g2(30); g3(6) excluded.
	ck("having_gt_scalar_avg", "SELECT g FROM t GROUP BY g HAVING SUM(v) > (SELECT AVG(v) FROM t)", []int64{1, 2})
	// SUM(v) > MAX(v)=30 → none (30 is not > 30).
	ck("having_gt_scalar_max_empty", "SELECT g FROM t GROUP BY g HAVING SUM(v) > (SELECT MAX(v) FROM t)", nil)
	// WHERE v > AVG(v)=11 then GROUP BY → v=20(g1),30(g2) → groups 1,2.
	ck("where_gt_scalar_then_group", "SELECT g FROM t WHERE v > (SELECT AVG(v) FROM t) GROUP BY g", []int64{1, 2})
}
