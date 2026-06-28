package sqldriver_test

// Probes for set operations, DISTINCT, and ORDER BY/LIMIT over joins — checking
// row-set correctness (dedup, union-all multiplicity, ordering + limit).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_SetOpsDistinctProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_setops")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_setops")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE setops "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_setops/s WITH TEMPLATE setops")
	dsn := fmt.Sprintf("fdbsql:///testdb_setops?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 10), (2, 20), (3, 30)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 1), (52, 2)")

	// ordered = preserve order (for ORDER BY tests); sorted = order-insensitive.
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
			sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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

	// UNION ALL: a.id (1,2,3) ++ c.a_id (1,1,2) → multiset {1,1,1,2,2,3} sorted.
	check("union_all", "SELECT id FROM a UNION ALL SELECT a_id FROM c", false,
		[]int64{1, 1, 1, 2, 2, 3})
	// UNION (distinct) is not implemented in Go (only UNION ALL) — a feature gap
	// vs Java, but cleanly rejected (not wrong rows).
	t.Run("union_distinct_rejected", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT id FROM a UNION SELECT a_id FROM c")
	})
	// DISTINCT a_id: {1,2}.
	check("distinct_col", "SELECT DISTINCT a_id FROM c", false, []int64{1, 2})
	// ORDER BY DESC + LIMIT over a join: a.id for matched rows {1,1,2}; DESC → 2,1,1; LIMIT 2 → 2,1.
	check("orderby_limit_join", "SELECT a.id FROM a JOIN c ON c.a_id = a.id ORDER BY a.id DESC LIMIT 2", true,
		[]int64{2, 1})
	// DISTINCT over a join: distinct a.id among matched → {1,2}.
	check("distinct_over_join", "SELECT DISTINCT a.id FROM a JOIN c ON c.a_id = a.id", false,
		[]int64{1, 2})
	// ORDER BY x ASC LIMIT with OFFSET-like: top-2 smallest x → 10,20 (ids 1,2).
	check("orderby_x_limit", "SELECT id FROM a ORDER BY x ASC LIMIT 2", true, []int64{1, 2})
}
