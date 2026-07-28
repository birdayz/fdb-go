package sqldriver_test

// A widened zero range spans TWO physical prefixes, so every column after it
// resets — the scan does not provide suffix ordering and the planner must not
// claim it does.
//
// The executor widens a zero-valued float equality across both signed zeros
// (-0.0 and +0.0 are IEEE-equal but pack to distinct adjacent keys). On a
// composite index (v, w) that means the scan walks (-0.0, w...) and then
// (+0.0, w...): w is ordered WITHIN each group and resets at the boundary.
//
// equalityPrefixLen counted the zero equality as pinning one value, so
// HintOrdering advertised w as globally ordered and the planner dropped the
// sort. Measured over rows (-0.0, 9) and (+0.0, 1):
//
//	SELECT w ... WHERE v = 0 ORDER BY w             -> [9 1]  UNSORTED
//	SELECT w ... WHERE v = 0 ORDER BY w LIMIT 1     -> [9]    wrong row
//	SELECT w ... WHERE v = 0 ORDER BY w DESC LIMIT 1 -> [1]   wrong row
//
// A zero-valued float equality is now treated like an INEQUALITY for ordering
// purposes — it binds a two-key range, not a point — so nothing after it is
// claimed as ordered and the required sort survives. The column itself stays in
// the ordering because it genuinely is ordered: -0.0 sorts immediately before
// +0.0.
//
// This was introduced by the widening fix and caught in review, not by the
// suite: the row-correctness tests all queried without ORDER BY, so they could
// not observe an ordering claim at all.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ZeroWidenBreaksSuffixOrdering(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_zwo")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_zwo")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE zwo "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_zwo/s WITH TEMPLATE zwo")
	dsn := fmt.Sprintf("fdbsql:///testdb_zwo?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// -0.0 sorts BEFORE +0.0, so the physical scan yields w=9 then w=1 —
	// descending across the prefix boundary. Any claim that w is ordered is
	// observably false on this data.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 9), (2, 0.0, 1)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	vals := func(t *testing.T, q string) []int64 {
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
		return o // ORDER is the assertion — deliberately NOT sorted here
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
	}{
		{"SELECT w FROM t WHERE v = 0 ORDER BY w", []int64{1, 9}},
		{"SELECT w FROM t WHERE v = 0 ORDER BY w LIMIT 1", []int64{1}},
		{"SELECT w FROM t WHERE v = 0 ORDER BY w DESC LIMIT 1", []int64{9}},
		{"SELECT id FROM t WHERE v = 0 ORDER BY w LIMIT 1", []int64{2}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			// The sort must be PRESENT. Without this the row assertions could
			// pass on a plan that happened to emit rows in the right order.
			if plan := explainOnConn(t, ctx, conn, tc.query); !strings.Contains(plan, "Sort") {
				t.Fatalf("%s\nplan = %s\nwant a sort node: a widened zero range spans two "+
					"prefixes, so the scan cannot supply w's ordering and the sort must not "+
					"be elided", tc.query, plan)
			}
			if got := vals(t, tc.query); !eq(got, tc.want) {
				t.Fatalf("%s = %v, want %v — w resets at the signed-zero prefix boundary, so "+
					"an elided sort returns index order instead of sorted order",
					tc.query, got, tc.want)
			}
		})
	}
}
