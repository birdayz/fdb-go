package sqldriver_test

// Probes string-column index ranges: empty string, lexicographic range bounds,
// LIKE prefix, open ranges, and ORDER BY — exercising string tuple ordering in
// the index SARG.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_StringIndexRangeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_stridx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_stridx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE stridx "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_stridx/s WITH TEMPLATE stridx")
	dsn := fmt.Sprintf("fdbsql:///testdb_stridx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id : s = '', 'a', 'apple', 'b', 'banana', 'z'
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1, ''), (2, 'a'), (3, 'apple'), (4, 'b'), (5, 'banana'), (6, 'z')")

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

	check("empty_string_eq", "SELECT id FROM t WHERE s = ''", false, []int64{1})
	check("range_a_to_b", "SELECT id FROM t WHERE s >= 'a' AND s < 'b'", false, []int64{2, 3})
	check("like_a_prefix", "SELECT id FROM t WHERE s LIKE 'a%'", false, []int64{2, 3})
	check("gt_b", "SELECT id FROM t WHERE s > 'b'", false, []int64{5, 6})
	check("ge_empty_all", "SELECT id FROM t WHERE s >= ''", false, []int64{1, 2, 3, 4, 5, 6})
	check("order_asc", "SELECT id FROM t ORDER BY s ASC", true, []int64{1, 2, 3, 4, 5, 6})
	check("eq_multichar", "SELECT id FROM t WHERE s = 'banana'", false, []int64{5})
	// LIMIT edges. NOTE: a BARE `SELECT … FROM t LIMIT 0` correctly returns 0
	// rows (plan Limit(0, Scan)); but `LIMIT 0` over any non-bare inner (WHERE,
	// ORDER BY, index) is a KNOWN gap — the Limit(0) operator is dropped and all
	// rows come back. Tracked in TODO.md "Known gaps". Pin the working bare form.
	check("limit_zero_bare", "SELECT id FROM t LIMIT 0", false, nil)
	check("limit_over", "SELECT id FROM t ORDER BY s ASC LIMIT 100", true, []int64{1, 2, 3, 4, 5, 6})
}
