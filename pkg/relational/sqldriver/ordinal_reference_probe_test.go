package sqldriver_test

// Pins positional (ordinal) column references: ORDER BY n and GROUP BY n refer to the
// n-th SELECT-list entry (1-based), and an out-of-range position is a clean 22023
// ("ORDER BY position N is out of range"), not a crash or silent ignore.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_OrdinalReferenceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ordr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ordr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ordr CREATE TABLE t (id BIGINT NOT NULL, grp BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ordr/s WITH TEMPLATE ordr")
	dsn := fmt.Sprintf("fdbsql:///testdb_ordr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, grp, v) VALUES (1,1,30),(2,1,10),(3,2,20)")

	col := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var a, b int64
			_ = rows.Scan(&a, &b)
			o = append(o, a)
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

	t.Run("order_by_ordinal_2_orders_by_v", func(t *testing.T) {
		// SELECT id, v ORDER BY 2 → by v asc (10,20,30) → ids 2,3,1
		if got := col("SELECT id, v FROM t ORDER BY 2"); !eq(got, []int64{2, 3, 1}) {
			t.Errorf("ORDER BY 2 ids = %v, want [2 3 1]", got)
		}
	})
	t.Run("order_by_ordinal_2_desc", func(t *testing.T) {
		if got := col("SELECT id, v FROM t ORDER BY 2 DESC"); !eq(got, []int64{1, 3, 2}) {
			t.Errorf("ORDER BY 2 DESC ids = %v, want [1 3 2]", got)
		}
	})
	t.Run("group_by_ordinal_1", func(t *testing.T) {
		// SELECT grp, COUNT(*) GROUP BY 1 → grp 1 → 2 rows, grp 2 → 1 row
		rows, err := db.QueryContext(ctx, "SELECT grp, COUNT(*) FROM t GROUP BY 1 ORDER BY 1")
		if err != nil {
			t.Fatalf("group by ordinal: %v", err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var g, c int64
			_ = rows.Scan(&g, &c)
			got[g] = c
		}
		if got[1] != 2 || got[2] != 1 {
			t.Errorf("GROUP BY 1 counts = %v, want {1:2, 2:1}", got)
		}
	})
	t.Run("order_by_ordinal_out_of_range_22023", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT id, v FROM t ORDER BY 3")
		if err == nil || !strings.Contains(err.Error(), "22023") {
			t.Errorf("ORDER BY 3 (out of range) error = %v, want 22023", err)
		}
	})
}
