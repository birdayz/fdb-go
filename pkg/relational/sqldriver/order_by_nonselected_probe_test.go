package sqldriver_test

// Probes ORDER BY on columns/expressions NOT in the SELECT list (SQL allows it):
// SELECT a ORDER BY b, ORDER BY an expression of non-selected columns, ORDER BY
// the PK when only a non-PK column is projected, and DESC.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_OrderByNonSelectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_obns")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_obns")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE obns CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_obns/s WITH TEMPLATE obns")
	dsn := fmt.Sprintf("fdbsql:///testdb_obns?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// (id,a,b): (1,30,5) (2,10,15) (3,20,10)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,b) VALUES (1,30,5),(2,10,15),(3,20,10)")

	aList := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var a int64
			if err := rows.Scan(&a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, a)
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
			if got := aList(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// ORDER BY b (5,10,15 → ids 1,3,2 → a = 30,20,10).
	ck("order_by_nonselected_col", "SELECT a FROM t ORDER BY b", []int64{30, 20, 10})
	ck("order_by_nonselected_desc", "SELECT a FROM t ORDER BY b DESC", []int64{10, 20, 30})
	// ORDER BY id (PK, not selected): a = 30,10,20.
	ck("order_by_pk_not_selected", "SELECT a FROM t ORDER BY id", []int64{30, 10, 20})
	// ORDER BY an expression of non-selected columns: id+b → 6,17,13 → ids 1,3,2 → a=30,20,10.
	ck("order_by_expr_nonselected", "SELECT a FROM t ORDER BY id + b", []int64{30, 20, 10})
}
