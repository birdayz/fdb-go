package sqldriver_test

// Probes ORDER BY inside a subquery — a Go read-side EXTENSION over Java. Java
// rejects ORDER BY in any non-top-level context (ExpressionVisitor.visitOrderByClause:
// `!isTopLevel()` → UNSUPPORTED_OPERATION "order by is not supported in subquery"),
// so a derived-table / scalar-subquery ORDER BY ... LIMIT (the top-N pattern) is
// unavailable in Java. Go supports it AND honors the ordering — pinned here as a
// correct extension (CLAUDE.md: read-side reach may exceed Java with deep tests).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OrderBySubqueryExtension(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_obsubx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_obsubx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE obsubx CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_obsubx/s WITH TEMPLATE obsubx")
	dsn := fmt.Sprintf("fdbsql:///testdb_obsubx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (id,v): (1,30) (2,10) (3,20)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1,30),(2,10),(3,20)")

	ints := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var x int64
			if err := rows.Scan(&x); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, x)
		}
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
			if got := ints(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v (ORDER BY in subquery must be honored, not dropped)", name, got, want)
			}
		})
	}

	// derived-table ORDER BY v LIMIT 2 → the two smallest v (10,20) → ids 2,3.
	ck("derived_orderby_limit_picks_smallest", "SELECT x.id FROM (SELECT id, v FROM t ORDER BY v LIMIT 2) AS x", []int64{2, 3})
	// derived-table ORDER BY v DESC LIMIT 1 → largest v (30) → id 1.
	ck("derived_orderby_desc_limit", "SELECT x.id FROM (SELECT id, v FROM t ORDER BY v DESC LIMIT 1) AS x", []int64{1})
	// scalar subquery (ORDER BY v LIMIT 1) → min v = 10.
	ck("scalar_orderby_limit_min", "SELECT (SELECT v FROM t ORDER BY v LIMIT 1) FROM t WHERE id = 1", []int64{10})
	// scalar subquery (ORDER BY v DESC LIMIT 1) → max v = 30.
	ck("scalar_orderby_desc_limit_max", "SELECT (SELECT v FROM t ORDER BY v DESC LIMIT 1) FROM t WHERE id = 1", []int64{30})
}
