package sqldriver_test

// Probes CTE (WITH) alias propagation, including the CTE analog of the nested-derived
// alias gap: a CTE that selects from another CTE with an alias. Unlike a 2-level
// nested INLINE derived table (which drops the alias — see
// nested_derived_table_probe_test.go), CTE chains propagate alias-introduced column
// names correctly, so a CTE is the workaround for that gap.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CteAliasPropagationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ctap")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ctap")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ctap CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ctap/s WITH TEMPLATE ctap")
	dsn := fmt.Sprintf("fdbsql:///testdb_ctap?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")

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

	ck("cte_alias", "WITH c AS (SELECT a AS x FROM t) SELECT x FROM c", []int64{10, 20, 30})
	ck("cte_alias_where", "WITH c AS (SELECT a AS x FROM t) SELECT x FROM c WHERE x > 15", []int64{20, 30})
	ck("cte_column_alias_clause", "WITH c(x) AS (SELECT a FROM t) SELECT x FROM c", []int64{10, 20, 30})
	// CTE referencing CTE with alias — the workaround for the nested-derived gap.
	ck("cte_ref_cte_alias", "WITH c1 AS (SELECT a AS x FROM t), c2 AS (SELECT x FROM c1) SELECT x FROM c2", []int64{10, 20, 30})
	ck("cte_ref_cte_realias", "WITH c1 AS (SELECT a AS x FROM t), c2 AS (SELECT x AS y FROM c1) SELECT y FROM c2", []int64{10, 20, 30})
	ck("cte_self_join", "WITH c AS (SELECT a AS x FROM t) SELECT c1.x FROM c c1 JOIN c c2 ON c1.x = c2.x WHERE c1.x = 20", []int64{20})
}
