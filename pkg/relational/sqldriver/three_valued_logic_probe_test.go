package sqldriver_test

// Probes for 3-valued boolean logic with NULLs (AND/OR/NOT propagation),
// NULL grouping in GROUP BY, and string functions (UPPER/LIKE/LENGTH). 3VL is a
// classic correctness trap: NULL must make AND/OR/NOT UNKNOWN and drop the row.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ThreeValuedLogicProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_3vl")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_3vl")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tvl "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3vl/s WITH TEMPLATE tvl")
	dsn := fmt.Sprintf("fdbsql:///testdb_3vl?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id, a, b, s
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, s) VALUES (1, 5, 10, 'hello'), (3, 7, 30, 'foo'), (5, 3, 5, 'hello')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, b, s) VALUES (2, 20, 'world')") // a NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (4)")                    // a,b,s NULL
	// Fix: id3 b should be NULL to exercise AND with NULL on b.
	mwjoMustExec(t, db, ctx, "UPDATE t SET b = NULL WHERE id = 3")

	ints := func(q string) []int64 {
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
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
	check := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ints(q); !eqi(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// a: 1→5, 2→NULL, 3→7, 4→NULL, 5→3. b: 1→10, 2→20, 3→NULL, 4→NULL, 5→5.
	check("and_with_null", "SELECT id FROM t WHERE a > 4 AND b > 4", []int64{1})      // id3 b NULL drops; id5 a=3
	check("or_with_null", "SELECT id FROM t WHERE a > 4 OR b > 15", []int64{1, 2, 3}) // a>4:1,3; b>15:2
	check("not_with_null", "SELECT id FROM t WHERE NOT (a > 4)", []int64{5})          // a=3 only; NULLs UNKNOWN
	check("eq_or_isnull", "SELECT id FROM t WHERE a = 3 OR a IS NULL", []int64{2, 4, 5})
	check("both_null", "SELECT id FROM t WHERE a IS NULL AND b IS NULL", []int64{4})
	check("upper_eq", "SELECT id FROM t WHERE UPPER(s) = 'HELLO'", []int64{1, 5})
	check("like_prefix", "SELECT id FROM t WHERE s LIKE 'h%'", []int64{1, 5})
	check("length_eq", "SELECT id FROM t WHERE LENGTH(s) = 5", []int64{1, 2, 5}) // hello,world,hello

	// NULL groups together in GROUP BY.
	t.Run("group_by_with_null", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT s, COUNT(*) FROM t GROUP BY s")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[string]int64{}
		nullCount := int64(0)
		for rows.Next() {
			var s sql.NullString
			var n int64
			if err := rows.Scan(&s, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !s.Valid {
				nullCount = n
			} else {
				got[s.String] = n
			}
		}
		if got["hello"] != 2 || got["world"] != 1 || got["foo"] != 1 || nullCount != 1 {
			t.Errorf("GROUP BY s = %v nullCount=%d, want hello:2 world:1 foo:1 NULL:1", got, nullCount)
		}
	})
}
