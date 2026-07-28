package sqldriver_test

// A zero-valued float equality must find BOTH signed zeros regardless of which
// index the planner picks.
//
// IEEE says -0.0 == +0.0 and this engine's `=` follows IEEE, but FDB tuple
// encoding preserves the sign bit (it is Java's encoder), so the two zeros are
// distinct adjacent index keys and a probe must span both. The executor widens
// the bound when the equality TERMINATES the scan range.
//
// "Terminal" was tested as `i == len(comparisons)-1`, i.e. last element of the
// slice. That is not the same thing. A candidate index contributes one
// ComparisonRange per indexed column, so an equality on the leading column of a
// composite index is followed by an EMPTY range for every column the query does
// not constrain. `v = 0` against index (v, w) produced [equality(v), empty(w)],
// the equality sat at i=0 with len-1 == 1, and the widening was skipped.
//
// The result was a PLAN-DEPENDENT wrong answer — the same query, same data,
// different rows depending on which index won:
//
//	v = 0  via single-column index (v)     -> [1 3]   correct
//	v = 0  via composite index (v, w)      -> [3]     the -0.0 row is lost
//	v IN (-0.0, 0.0) via composite (v, w)  -> [1]     the +0.0 row is lost
//
// Now the test is "nothing after this constrains the key", which is what
// terminal actually means. A trailing CONSTRAINING comparison is still excluded
// on purpose: with `v = 0 AND w = 5` the union of (-0.0,5) and (+0.0,5) is not
// a contiguous interval — the span between them also admits (-0.0, w>5) and
// (+0.0, w<5) — so widening there would return WRONG rows rather than missing
// ones. That case needs a two-probe union and is tracked as CQ-28.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_CompositeIndexZeroWidening(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_czw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_czw")
	// s: single-column index only. c: composite only. u: no index at all.
	// The same predicate must answer identically through all three.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE czw "+
		"CREATE TABLE s (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX s_v ON s (v) "+
		"CREATE TABLE c (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX c_vw ON c (v, w) "+
		"CREATE TABLE u (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_czw/s WITH TEMPLATE czw")
	dsn := fmt.Sprintf("fdbsql:///testdb_czw?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tbl := range []string{"s", "c", "u"} {
		mwjoMustExec(t, db, ctx, fmt.Sprintf(
			"INSERT INTO %s (id, v, w) VALUES (1, -0.0, 5), (2, 5.0, 5), (3, 0.0, 9)", tbl))
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
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

	// The access path must not change the answer. Running the SAME predicate
	// across single-column / composite / unindexed is the assertion.
	for _, pred := range []struct {
		where string
		want  []int64
		why   string
	}{
		{"v = 0", []int64{1, 3}, "both signed zeros satisfy an IEEE `= 0`"},
		{"v IN (-0.0, 0.0)", []int64{1, 3}, "an IN over both zeros must find both rows"},
		{"v IN (5.0, 5.0)", []int64{2}, "a repeated literal must still collapse to one probe"},
		{"v >= 0", []int64{1, 2, 3}, "inequality keeps its own range widening"},
		{"v = 5.0", []int64{2}, "ordinary nonzero equality is untouched"},
	} {
		for _, tbl := range []string{"s", "c", "u"} {
			q := fmt.Sprintf("SELECT id FROM %s WHERE %s", tbl, pred.where)
			t.Run(q, func(t *testing.T) {
				if got := ids(t, q); !eq(got, pred.want) {
					t.Fatalf("%s\n%s = %v, want %v — table s has a single-column index, c has "+
						"only a composite one, u has none; the access path must not change the "+
						"answer", pred.why, q, got, pred.want)
				}
			})
		}
	}
}
