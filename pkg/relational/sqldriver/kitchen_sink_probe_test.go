package sqldriver_test

// Probes full clause interaction in one query: WHERE + GROUP BY + HAVING +
// aggregates + ORDER BY (on an aggregate) + LIMIT together, and with OFFSET.
// Clause-combination bugs (filter applied at the wrong stage, HAVING vs WHERE,
// ORDER/LIMIT over grouped output) surface here.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_KitchenSinkProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_kitchen")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_kitchen")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE kitchen "+
			"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_kitchen/s WITH TEMPLATE kitchen")
	dsn := fmt.Sprintf("fdbsql:///testdb_kitchen?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// g=1: v 5,15,25 (after WHERE v>10: 15,25 → cnt2 sum40)
	// g=2: v 20,30      (WHERE v>10: 20,30 → cnt2 sum50)
	// g=3: v 1,2,100    (WHERE v>10: 100 → cnt1 sum100)
	// g=4: v 12,13,14,16 (WHERE v>10: all 4 → cnt4 sum55)
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,g,v) VALUES "+
		"(1,1,5),(2,1,15),(3,1,25),(4,2,20),(5,2,30),(6,3,1),(7,3,2),(8,3,100),(9,4,12),(10,4,13),(11,4,14),(12,4,16)")

	type gr struct {
		g   int64
		cnt int64
		sum int64
	}
	groups := func(q string) []gr {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []gr
		for rows.Next() {
			var x gr
			if err := rows.Scan(&x.g, &x.cnt, &x.sum); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, x)
		}
		return out
	}
	eqGr := func(g, w []gr) bool {
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

	t.Run("where_group_having_order_limit", func(t *testing.T) {
		// WHERE v>10 → groups: g1(cnt2,sum40) g2(cnt2,sum50) g3(cnt1,sum100) g4(cnt4,sum55)
		// HAVING COUNT(*) >= 2 → g1,g2,g4
		// ORDER BY SUM(v) DESC → g4(55), g2(50), g1(40)
		// LIMIT 2 → g4, g2
		q := "SELECT g, COUNT(*), SUM(v) FROM t WHERE v > 10 GROUP BY g HAVING COUNT(*) >= 2 ORDER BY SUM(v) DESC LIMIT 2"
		got := groups(q)
		want := []gr{{4, 4, 55}, {2, 2, 50}}
		if !eqGr(got, want) {
			t.Errorf("kitchen-sink = %v, want %v", got, want)
		}
	})

	t.Run("with_offset", func(t *testing.T) {
		// same but LIMIT 2 OFFSET 1 → skip g4, take g2, g1.
		q := "SELECT g, COUNT(*), SUM(v) FROM t WHERE v > 10 GROUP BY g HAVING COUNT(*) >= 2 ORDER BY SUM(v) DESC LIMIT 2 OFFSET 1"
		got := groups(q)
		want := []gr{{2, 2, 50}, {1, 2, 40}}
		if !eqGr(got, want) {
			t.Errorf("kitchen-sink+offset = %v, want %v", got, want)
		}
	})

	t.Run("having_sum_threshold_order_asc", func(t *testing.T) {
		// WHERE v>10, HAVING SUM(v) > 45 → g2(50), g3(100), g4(55). ORDER BY g ASC.
		q := "SELECT g, COUNT(*), SUM(v) FROM t WHERE v > 10 GROUP BY g HAVING SUM(v) > 45 ORDER BY g ASC"
		got := groups(q)
		want := []gr{{2, 2, 50}, {3, 1, 100}, {4, 4, 55}}
		if !eqGr(got, want) {
			t.Errorf("having-sum = %v, want %v", got, want)
		}
	})
}
