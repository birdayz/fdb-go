package sqldriver_test

// Probes DISTINCT composed with ORDER BY and LIMIT: dedup then order then limit,
// including DESC and multi-column DISTINCT. A common combination where dedup and
// ordering must compose correctly (the LIMIT applies to the distinct, ordered set).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_DistinctOrderByLimitProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dol")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dol")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dol CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dol/s WITH TEMPLATE dol")
	dsn := fmt.Sprintf("fdbsql:///testdb_dol?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// v duplicated: distinct v = {10,20,30}
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a,v) VALUES (1,1,30),(2,1,10),(3,2,30),(4,2,20),(5,1,10)")

	vlist := func(q string) []int64 {
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
			if got := vlist(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	ck("distinct_order_asc", "SELECT DISTINCT v FROM t ORDER BY v", []int64{10, 20, 30})
	ck("distinct_order_desc", "SELECT DISTINCT v FROM t ORDER BY v DESC", []int64{30, 20, 10})
	ck("distinct_order_limit", "SELECT DISTINCT v FROM t ORDER BY v LIMIT 2", []int64{10, 20})
	ck("distinct_order_desc_limit", "SELECT DISTINCT v FROM t ORDER BY v DESC LIMIT 2", []int64{30, 20})
	ck("distinct_order_offset", "SELECT DISTINCT v FROM t ORDER BY v LIMIT 1 OFFSET 1", []int64{20})

	t.Run("distinct_multicol_count", func(t *testing.T) {
		// distinct (a,v): (1,30),(1,10),(2,30),(2,20) = 4 distinct pairs (one dup (1,10)).
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT DISTINCT a, v FROM t) AS d").Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if c != 4 {
			t.Errorf("COUNT of DISTINCT a,v = %d, want 4", c)
		}
	})
}
