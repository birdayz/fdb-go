package sqldriver_test

// Probes ORDER BY over a GROUP BY result: by aggregate expression (SUM(v)), by
// aggregate alias, by grouping key, and by SELECT-list ORDINAL (ORDER BY 2 refers
// to the 2nd SELECT item = the aggregate, resolved by SELECT-list position — which
// is correct even though the output columns are emitted keys-first).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_GroupByOrderByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gob")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gob")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gob CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gob/s WITH TEMPLATE gob")
	dsn := fmt.Sprintf("fdbsql:///testdb_gob?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a=1 SUM=40, a=2 SUM=55
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, v) VALUES (1,1,30),(2,1,10),(3,2,50),(4,2,5)")

	keyOrder := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var k, s sql.NullInt64
			if err := rows.Scan(&k, &s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, k.Int64)
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
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := keyOrder(q); !eq(got, want) {
				t.Errorf("%s key-order = %v, want %v", name, got, want)
			}
		})
	}

	ck("by_aggregate_desc", "SELECT a, SUM(v) FROM t GROUP BY a ORDER BY SUM(v) DESC", []int64{2, 1})
	ck("by_aggregate_asc", "SELECT a, SUM(v) FROM t GROUP BY a ORDER BY SUM(v) ASC", []int64{1, 2})
	ck("by_ordinal_2_desc", "SELECT a, SUM(v) FROM t GROUP BY a ORDER BY 2 DESC", []int64{2, 1})
	ck("by_aggregate_alias", "SELECT a, SUM(v) AS s FROM t GROUP BY a ORDER BY s DESC", []int64{2, 1})
	ck("by_grouping_key_desc", "SELECT a, SUM(v) FROM t GROUP BY a ORDER BY a DESC", []int64{2, 1})
}
