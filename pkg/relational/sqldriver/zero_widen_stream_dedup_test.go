package sqldriver_test

// Streaming DISTINCT and streaming aggregation dedup against the PREVIOUS row
// only, which is sound solely when equal keys are ADJACENT. A widened zero
// range spans two physical prefixes, so a suffix value can appear in the -0.0
// group and again in the +0.0 group, non-adjacently — and an adjacency-based
// operator would emit it twice.
//
// This is the second half of the ordering defect: the first half was the
// planner eliding a required sort (zero_widen_ordering_test.go), and both come
// from the same false claim that a zero equality pins a single key and leaves
// the suffix globally ordered.
//
// The fix is shared. Because a zero-valued float equality no longer counts
// toward the equality prefix, the suffix carries no ordering claim, so the
// planner either sorts first or picks the hash Distinct — never adjacency over
// the raw scan. The plan assertions below pin that, because the row counts
// alone would also pass on a plan that happened to order conveniently.
//
// Data is arranged so adjacency genuinely fails: the physical scan yields
// (-0.0,5) (-0.0,9) (+0.0,5), with w=5 appearing twice, straddling the boundary.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ZeroWidenStreamingDedup(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_zwsd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_zwsd")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE zwsd "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_zwsd/s WITH TEMPLATE zwsd")
	dsn := fmt.Sprintf("fdbsql:///testdb_zwsd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 5), (2, -0.0, 9), (3, 0.0, 5)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	scalar := func(t *testing.T, q string) int64 {
		t.Helper()
		var n int64
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return n
	}

	for _, tc := range []struct {
		query string
		want  int64
		why   string
	}{
		{
			"SELECT COUNT(*) FROM (SELECT DISTINCT w FROM t WHERE v = 0) AS s", 2,
			"w takes two values {5, 9}; the repeated 5 straddles the signed-zero prefix " +
				"boundary, so an adjacency-based dedup would report 3",
		},
		{
			"SELECT COUNT(*) FROM (SELECT w FROM t WHERE v = 0 GROUP BY w) AS s", 2,
			"same through the aggregate path — a streaming group-break on non-adjacent " +
				"equal keys would split w=5 into two groups",
		},
		{
			"SELECT COUNT(*) FROM (SELECT DISTINCT v, w FROM t WHERE v = 0) AS s", 3,
			"all three tuples are distinct under tuple identity: (-0.0,5) (-0.0,9) (+0.0,5) — " +
				"the two signed zeros are different keys, which is the settled DISTINCT rule",
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			// The plan must NOT rely on adjacency over the raw widened scan: it
			// either sorts first or uses the hash Distinct.
			plan := explainOnConn(t, ctx, conn, tc.query)
			if strings.Contains(plan, "IndexScan(T_VW") &&
				!strings.Contains(plan, "Sort") && !strings.Contains(plan, "Distinct(") {
				t.Fatalf("%s\nplan = %s\nthis dedups by adjacency directly over a widened zero "+
					"scan, whose suffix resets at the prefix boundary", tc.query, plan)
			}
			if got := scalar(t, tc.query); got != tc.want {
				t.Fatalf("%s\n%s = %d, want %d", tc.why, tc.query, got, tc.want)
			}
		})
	}

	// Per-group counts, which localise the failure if a group ever splits.
	rows, err := conn.QueryContext(ctx, "SELECT w, COUNT(*) FROM t WHERE v = 0 GROUP BY w ORDER BY w")
	if err != nil {
		t.Fatalf("grouped query: %v", err)
	}
	defer rows.Close()
	type pair struct{ w, n int64 }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.w, &p.n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []pair{{5, 2}, {9, 1}}
	if len(got) != len(want) {
		t.Fatalf("GROUP BY w = %v, want %v — w=5 spans the signed-zero boundary and must "+
			"still form ONE group of 2", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GROUP BY w = %v, want %v", got, want)
		}
	}
}
