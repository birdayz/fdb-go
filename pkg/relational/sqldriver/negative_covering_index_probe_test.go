package sqldriver_test

// Probes index ranges over NEGATIVE values (tuple sign-encoding ordering) and
// covering index-only scans. Negative numbers must sort below zero/positives in
// the index; a covering scan must return the right column values without a fetch.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NegativeCoveringIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_negidx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_negidx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE negidx "+
			"CREATE TABLE t (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_k ON t (k)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_negidx/s WITH TEMPLATE negidx")
	dsn := fmt.Sprintf("fdbsql:///testdb_negidx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id : k = -10, -5, -1, 0, 3, 7
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, k) VALUES (1, -10), (2, -5), (3, -1), (4, 0), (5, 3), (6, 7)")

	ids := func(q string, keepOrder bool) []int64 {
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
		if !keepOrder {
			for i := 1; i < len(out); i++ {
				for j := i; j > 0 && out[j-1] > out[j]; j-- {
					out[j-1], out[j] = out[j], out[j-1]
				}
			}
		}
		return out
	}
	eqi := func(g, w []int64) bool {
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
	check := func(name, q string, keepOrder bool, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q, keepOrder); !eqi(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	check("neg_range", "SELECT id FROM t WHERE k BETWEEN -10 AND -1", false, []int64{1, 2, 3})
	check("lt_zero", "SELECT id FROM t WHERE k < 0", false, []int64{1, 2, 3})
	check("gt_neg5", "SELECT id FROM t WHERE k > -5", false, []int64{3, 4, 5, 6})
	check("straddle_zero", "SELECT id FROM t WHERE k BETWEEN -5 AND 3", false, []int64{2, 3, 4, 5})
	check("eq_neg", "SELECT id FROM t WHERE k = -5", false, []int64{2})
	check("order_asc", "SELECT id FROM t ORDER BY k ASC", true, []int64{1, 2, 3, 4, 5, 6})
	check("order_desc", "SELECT id FROM t ORDER BY k DESC", true, []int64{6, 5, 4, 3, 2, 1})

	// Covering index-only scan: SELECT k WHERE k > 0 (k is in the index; no fetch).
	t.Run("covering_k_values", func(t *testing.T) {
		var plan string
		_ = db.QueryRowContext(ctx, "EXPLAIN SELECT k FROM t WHERE k > 0").Scan(&plan)
		t.Logf("plan: %s", plan)
		rows, err := db.QueryContext(ctx, "SELECT k FROM t WHERE k > 0 ORDER BY k")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var k int64
			if err := rows.Scan(&k); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, k)
		}
		if !eqi(got, []int64{3, 7}) {
			t.Errorf("covering k values = %v, want [3 7]", got)
		}
	})
}
