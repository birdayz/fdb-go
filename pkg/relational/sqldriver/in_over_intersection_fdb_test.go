package sqldriver_test

// Row-level pin for IN over a compensated pk-intersection. The plan-shape
// companion is TestInOverIntersection_RelinksResidual (embedded).
//
// Two failure modes this covers, both found by the RFC-182 rowdiff harness:
//   - pre-compensation: the IN residual was silently DROPPED and the query
//     returned extra rows (the wrong-rows class);
//   - post-compensation, pre-relink: the same query failed the XX000 plan
//     invariant with `PredicatesFilter(<nil>)`.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_InOverIntersection_ResidualApplied(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_inovix"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inovix CREATE TABLE t_rd (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id)) CREATE INDEX idx_b ON t_rd (b) CREATE INDEX idx_c ON t_rd (c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE inovix")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// b=1 AND c=9 selects ids {1,2,3}; only id 1 (a=3) and id 3 (a=7) are in
	// the IN list, so a dropped residual would leak id 2.
	mwjoMustExec(t, db, ctx, `INSERT INTO t_rd VALUES
		(1, 3, 1, 9, 'x'),
		(2, 4, 1, 9, 'y'),
		(3, 7, 1, 9, 'x'),
		(4, 3, 2, 9, 'x'),
		(5, 3, 1, 8, 'x')`)

	collect := func(q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		var got []int64
		for rows.Next() {
			var id, a, b, c int64
			var s string
			if len(cols) == 1 {
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
			} else if err := rows.Scan(&id, &a, &b, &c, &s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return got
	}
	same := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		seen := map[int64]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range want {
			if !seen[w] {
				return false
			}
		}
		return true
	}

	if got := collect("SELECT * FROM t_rd WHERE b = 1 AND c = 9 AND a IN (3, 7)"); !same(got, 1, 3) {
		t.Errorf("SELECT * with IN residual: got %v, want [1 3] (id 2 leaks when the residual is dropped)", got)
	}
	if got := collect("SELECT id FROM t_rd WHERE b = 1 AND c = 9 AND a IN (3, 7)"); !same(got, 1, 3) {
		t.Errorf("SELECT id with IN residual: got %v, want [1 3]", got)
	}
	// IN plus a second unindexed residual.
	if got := collect("SELECT * FROM t_rd WHERE b = 1 AND c = 9 AND s = 'x' AND a IN (3, 7)"); !same(got, 1, 3) {
		t.Errorf("IN + scalar residual: got %v, want [1 3]", got)
	}
	// Empty IN-list result: the intersection matches, the residual excludes all.
	if got := collect("SELECT * FROM t_rd WHERE b = 1 AND c = 9 AND a IN (99)"); len(got) != 0 {
		t.Errorf("non-matching IN residual: got %v, want []", got)
	}
}
