package sqldriver_test

// Probes HAVING with GROUP BY: filters groups by an aggregate predicate, by an
// aggregate NOT in the SELECT list, by a grouped column, and by a combined
// AND of conditions. Confirms HAVING is applied (not dropped) post-aggregation.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_HavingGroupByProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_havgb")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_havgb")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE havgb CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_havgb/s WITH TEMPLATE havgb")
	dsn := fmt.Sprintf("fdbsql:///testdb_havgb?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// g1: v10,v20 (cnt2 sum30) ; g2: v30 (cnt1 sum30) ; g3: v1,v2,v3 (cnt3 sum6)
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

	// HAVING COUNT(*) > 1 → g1(2), g3(3); excludes g2(1).
	ck("having_count", "SELECT g FROM t GROUP BY g HAVING COUNT(*) > 1", []int64{1, 3})
	// HAVING SUM(v) > 25 (SUM not selected) → g1(30), g2(30); excludes g3(6).
	ck("having_unselected_agg", "SELECT g FROM t GROUP BY g HAVING SUM(v) > 25", []int64{1, 2})
	// HAVING on grouped column → g2, g3.
	ck("having_grouped_col", "SELECT g FROM t GROUP BY g HAVING g >= 2", []int64{2, 3})
	// combined: COUNT(*)>1 AND SUM(v)>25 → g1 only (g3 has cnt3 but sum6).
	ck("having_combined_and", "SELECT g FROM t GROUP BY g HAVING COUNT(*) > 1 AND SUM(v) > 25", []int64{1})
	// HAVING that excludes everything → empty.
	ck("having_excludes_all", "SELECT g FROM t GROUP BY g HAVING COUNT(*) > 100", nil)

	// HAVING with no matching groups still returns the right shape.
	t.Run("having_min_max", func(t *testing.T) {
		// HAVING MAX(v) >= 30 → g1(max20)? no; g2(max30) yes; g3(max3) no. → g2.
		if got := groups("SELECT g FROM t GROUP BY g HAVING MAX(v) >= 30"); !eq(got, []int64{2}) {
			t.Errorf("HAVING MAX(v)>=30 = %v, want [2]", got)
		}
	})
}
