package sqldriver_test

// Probes derived tables (subquery in FROM) in their common shapes, all of which work
// at one level (the 2-level-nested-alias gap is pinned separately in
// nested_derived_table_probe_test.go): a derived table as a JOIN leg (with and
// without a column alias), a derived table over a GROUP BY aggregate, filtering on a
// derived aggregate alias, and joining a derived-aggregate to a base table.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_DerivedTableShapesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dts")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dts")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE dts "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT NOT NULL, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dts/s WITH TEMPLATE dts")
	dsn := fmt.Sprintf("fdbsql:///testdb_dts?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO u (id, b) VALUES (10,1),(20,2)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
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
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("derived_join_leg", "SELECT t.id FROM t JOIN (SELECT id FROM u) d ON t.a = d.id", []int64{1, 2, 3})
	ck("derived_join_leg_alias", "SELECT t.id FROM t JOIN (SELECT id AS uid FROM u) d ON t.a = d.uid", []int64{1, 2, 3})
	ck("derived_over_aggregate", "SELECT cnt FROM (SELECT a, COUNT(*) AS cnt FROM t GROUP BY a) d", []int64{1, 2})
	ck("filter_on_derived_agg_alias", "SELECT a FROM (SELECT a, COUNT(*) AS cnt FROM t GROUP BY a) d WHERE cnt > 1", []int64{20})
	ck("derived_aggregate_joined", "SELECT d.a FROM (SELECT a, COUNT(*) AS cnt FROM t GROUP BY a) d JOIN u ON d.a = u.id", []int64{10, 20})
}
