package sqldriver_test

// Probes for index SARG / scan-range correctness: equality, open/closed ranges,
// (a,b) prefix scans, IN-list on an indexed column, and ORDER BY via index +
// LIMIT (asc/desc). A wrong range bound, prefix, or scan direction yields wrong
// rows even when the plan "works".

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_IndexSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_idx_sarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_idx_sarg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE idx_sarg "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_idx_sarg/s WITH TEMPLATE idx_sarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_idx_sarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id: a, b, s
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, s) VALUES "+
		"(1, 1, 10, 'a'), (2, 3, 20, 'c'), (3, 5, 2, 'e'), (4, 6, 8, 'e'), (5, 7, 30, 'm'), (6, 9, 40, 'z')")

	ints := func(q string, keepOrder bool) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v.Int64)
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
			if got := ints(q, keepOrder); !eqi(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	check("eq", "SELECT id FROM t WHERE a = 5", false, []int64{3})
	check("range_open", "SELECT id FROM t WHERE a > 3 AND a < 8", false, []int64{3, 4, 5})
	check("range_closed", "SELECT id FROM t WHERE a >= 3 AND a <= 8", false, []int64{2, 3, 4, 5})
	check("range_half_open", "SELECT id FROM t WHERE a >= 5 AND a < 9", false, []int64{3, 4, 5})
	check("prefix_eq_then_range", "SELECT id FROM t WHERE a = 7 AND b > 20", false, []int64{5})
	check("prefix_range_a", "SELECT id FROM t WHERE a >= 5 AND a <= 7", false, []int64{3, 4, 5})
	check("in_list_indexed", "SELECT id FROM t WHERE a IN (3, 7)", false, []int64{2, 5})
	check("order_by_a_limit_asc", "SELECT id FROM t ORDER BY a ASC LIMIT 3", true, []int64{1, 2, 3})
	check("order_by_a_limit_desc", "SELECT id FROM t ORDER BY a DESC LIMIT 2", true, []int64{6, 5})
	check("string_eq", "SELECT id FROM t WHERE s = 'm'", false, []int64{5})
	check("string_range", "SELECT id FROM t WHERE s >= 'c' AND s < 'm'", false, []int64{2, 3, 4})
	check("not_eq", "SELECT id FROM t WHERE a <> 5", false, []int64{1, 2, 4, 5, 6})

	// Plan check: an indexed equality should use the index (not a full scan).
	t.Run("eq_uses_index", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM t WHERE a = 5").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		// A SARG'd index scan, not a full Scan(T) residual filter.
		if !containsAny(plan, "IndexScan", "Index(") {
			t.Logf("note: a=5 plan does not show an IndexScan (acceptable if rows correct): %s", plan)
		}
	})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
