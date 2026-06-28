package sqldriver_test

// Probes multiple CTEs and CTE chains: a second CTE referencing the first, and a
// join between two CTEs. (Single + recursive CTE are covered elsewhere; this pins
// chaining and multi-CTE joins.)

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_MultiCTEProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mcte")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mcte")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mcte CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mcte/s WITH TEMPLATE mcte")
	dsn := fmt.Sprintf("fdbsql:///testdb_mcte?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,5),(2,15),(3,25),(4,35)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
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

	// single CTE baseline: v>10 → ids 2,3,4.
	ck("single_cte", "WITH a AS (SELECT id, v FROM t WHERE v > 10) SELECT id FROM a", []int64{2, 3, 4})
	// CTE chain: b reads from a. a=v>10 (2,3,4); b filters v>20 → 3,4.
	ck("cte_chain", "WITH a AS (SELECT id, v FROM t WHERE v > 10), b AS (SELECT id FROM a WHERE v > 20) SELECT id FROM b", []int64{3, 4})
	// multi-CTE join: a=v>20 (3,4); b=v<30 (1,2,3); a JOIN b ON id → 3.
	ck("multi_cte_join",
		"WITH a AS (SELECT id FROM t WHERE v > 20), b AS (SELECT id FROM t WHERE v < 30) SELECT a.id FROM a JOIN b ON a.id = b.id",
		[]int64{3})
	// CTE with aggregate, then filter the CTE.
	ck("cte_aggregate_then_filter",
		"WITH s AS (SELECT id, v FROM t) SELECT id FROM s WHERE v = (SELECT MAX(v) FROM s)", []int64{4})
}
