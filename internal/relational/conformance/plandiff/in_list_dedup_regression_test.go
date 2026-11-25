package plandiff

import (
	"context"
	"testing"
)

// TestGoSQLRunner_InListDedup is the regression test for the IN-list
// duplicate-literal bug.
//
// When the IN column is the primary key (or otherwise indexed), the planner
// rewrites `col IN (v1, v2, ...)` into an Explode + InJoin (one iteration per
// list element). Java's InComparisonToExplodeRule wraps the comparand in
// ArrayDistinctValue, so duplicate literals collapse to a single iteration.
// Go skipped that dedup, so `a IN (1, 1, 1)` on a PK explodes three times and
// the InJoin emitted three copies of the same row.
//
// Teeth: before the fix `a IN (1,1,1)` returned 3 rows and
// `a IN (2,1,2,3,1)` returned 5; both are now correctly deduped.
func TestGoSQLRunner_InListDedup(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	const schema = "CREATE TABLE ta (a BIGINT, b BIGINT, PRIMARY KEY (a))"
	setup := []string{"INSERT INTO ta VALUES (1, 8), (2, 7), (3, 6), (4, 5), (5, 4), (6, 3), (7, 2), (8, 1)"}

	cases := []struct {
		name string
		sql  string
		want [][]float64
	}{
		// PK IN with all-duplicate literals → single row (not three).
		{"pk_all_dup", "SELECT a, b FROM ta WHERE a IN (1, 1, 1) ORDER BY a", [][]float64{{1, 8}}},
		// PK IN with interleaved duplicates → distinct rows, sorted.
		{"pk_interleaved_dup", "SELECT a, b FROM ta WHERE a IN (2, 1, 2, 3, 1) ORDER BY a", [][]float64{{1, 8}, {2, 7}, {3, 6}}},
		// Single distinct value repeated → equality collapse.
		{"pk_two_dup", "SELECT a, b FROM ta WHERE a IN (5, 5) ORDER BY a", [][]float64{{5, 4}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := r.RunWithSetup(context.Background(), schema, setup, c.sql)
			if got.Err != nil {
				t.Fatalf("%s: %v", c.name, got.Err)
			}
			if len(got.Rows.Rows) != len(c.want) {
				t.Fatalf("%s: got %d rows %+v, want %d (IN-list duplicates not deduped)",
					c.name, len(got.Rows.Rows), got.Rows.Rows, len(c.want))
			}
			for i, wr := range c.want {
				if got.Rows.Rows[i][0] != wr[0] || got.Rows.Rows[i][1] != wr[1] {
					t.Fatalf("%s: row %d = %v, want %v", c.name, i, got.Rows.Rows[i], wr)
				}
			}
		})
	}
}
