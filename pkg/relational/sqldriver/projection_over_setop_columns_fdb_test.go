package sqldriver_test

// Column-derivation pin: a set operation BELOW a projection must not supply
// the result's column list (found by the RFC-182 rowdiff harness once its
// generator learned DISTINCT + LIMIT).
//
// findUnionPlan descends through unary wrappers to reach a top-level set
// operation wearing a Fetch/Limit hat. It also descended straight through the
// PROJECTION, so `SELECT DISTINCT id … WHERE a=? AND b=? LIMIT k` over two
// indexes — planned as `Limit(Project([ID], Intersection(…)))` — derived its
// columns from an intersection leg's full record. The result set then claimed
// every table column while the plan emitted the projection's single slot, and
// each read failed the positional-alignment guard with XX000.
//
// The bug needed all three of DISTINCT (to elide into a bare projection over
// the intersection), LIMIT (to put a unary wrapper on top so the projection
// was no longer the root), and two indexed equalities (to make the
// intersection win) — the kind of three-way interaction hand-written cases
// rarely reach.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ProjectionOverSetOp_ColumnDerivation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_projsetop"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE projsetop CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE projsetop")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1,2,5),(2,2,5),(3,3,5),(4,2,6)")

	for _, tc := range []struct {
		name     string
		query    string
		wantCols []string
		wantRows int
	}{
		{
			// The failing shape: DISTINCT elides to a bare projection, LIMIT
			// puts a wrapper above it, two equalities make the intersection win.
			name:     "distinct_limit_over_intersection",
			query:    "SELECT DISTINCT id FROM t WHERE b = 5 AND a = 2 LIMIT 27",
			wantCols: []string{"ID"},
			wantRows: 2,
		},
		{
			name:     "distinct_over_intersection_no_limit",
			query:    "SELECT DISTINCT id FROM t WHERE b = 5 AND a = 2",
			wantCols: []string{"ID"},
			wantRows: 2,
		},
		{
			name:     "distinct_nonpk_column",
			query:    "SELECT DISTINCT a FROM t WHERE b = 5 AND a = 2 LIMIT 27",
			wantCols: []string{"A"},
			wantRows: 1,
		},
		{
			// Control: a genuine star over the same intersection still reports
			// every column — the fix must not over-narrow.
			name:     "star_over_intersection_keeps_all_columns",
			query:    "SELECT * FROM t WHERE b = 5 AND a = 2 LIMIT 27",
			wantCols: []string{"ID", "A", "B"},
			wantRows: 2,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if len(cols) != len(tc.wantCols) {
				t.Fatalf("columns = %v, want %v", cols, tc.wantCols)
			}
			for i, want := range tc.wantCols {
				if cols[i] != want {
					t.Errorf("column %d = %q, want %q", i, cols[i], want)
				}
			}
			// Reading every column is what actually trips the positional
			// alignment guard — counting rows alone would not.
			n := 0
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if n != tc.wantRows {
				t.Errorf("row count = %d, want %d", n, tc.wantRows)
			}
		})
	}
}
