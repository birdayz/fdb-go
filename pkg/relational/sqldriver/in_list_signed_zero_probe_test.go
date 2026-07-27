package sqldriver_test

// An IN list must probe every element that packs to a DISTINCT index key.
//
// The IN-list dedup exists to avoid issuing the same probe twice, so its
// identity has to be PROBE identity — the tuple key an element packs to — not
// SQL value identity. Those coincide for every type except float: IEEE says
// -0.0 == +0.0, while the FDB tuple encoder preserves the sign bit and packs
// them to two distinct, adjacent keys.
//
// Deduping by value therefore dropped one of the two, and the survivor probed
// only its own key. Measured before the fix, over rows (-0.0,5) (5.0,5) (0.0,9):
//
//	v IN (-0.0, 0.0)  ->  [1]      one probe; the +0.0 row is lost
//	v IN (0.0, -0.0)  ->  [3]      the -0.0 row is lost instead
//	v IN (-0.0, 5.0)  ->  [1] [2]  two distinct values, always worked
//
// The answer depended on element ORDER, which is the tell: a set operation
// whose result depends on the order of the set is not deduping, it is losing.
//
// Java does not exhibit this, but only by accident: its `=` is bit identity
// (Comparisons.java:246), which is an upstream bug this port deliberately does
// NOT follow — it also makes `WHERE d = d` true for NaN. See DIVERGENCES.md.
// Go's `=` is correctly IEEE, and that is precisely what made a value-based
// probe dedup unsound here.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_InListSignedZeroProbesBothKeys(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_inzero")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_inzero")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE inzero "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_inzero/s WITH TEMPLATE inzero")
	dsn := fmt.Sprintf("fdbsql:///testdb_inzero?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 5), (2, 5.0, 5), (3, 0.0, 9)")

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

	for _, tc := range []struct {
		query string
		want  []int64
		why   string
	}{
		{
			"SELECT id FROM t WHERE v IN (-0.0, 0.0)",
			[]int64{1, 3},
			"both signed zeros pack to distinct keys, so both must be probed",
		},
		{
			// Same set, reversed. A set operation must not depend on order.
			"SELECT id FROM t WHERE v IN (0.0, -0.0)",
			[]int64{1, 3},
			"reversing the list must not change the answer",
		},
		{
			"SELECT id FROM t WHERE v IN (-0.0, 5.0)",
			[]int64{1, 2},
			"control: two ordinarily-distinct values",
		},
		{
			// A genuine duplicate must STILL collapse — the fix must not turn
			// the dedup off, only change what counts as identical.
			"SELECT id FROM t WHERE v IN (5.0, 5.0, 5.0)",
			[]int64{2},
			"a repeated literal must still dedup to one probe, not emit three rows",
		},
		{
			"SELECT id FROM t WHERE v IN (-0.0, 0.0, 5.0)",
			[]int64{1, 2, 3},
			"both zeros plus a third distinct value",
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := ids(t, tc.query); !eq(got, tc.want) {
				t.Fatalf("%s\n%s = %v, want %v", tc.why, tc.query, got, tc.want)
			}
		})
	}
}
