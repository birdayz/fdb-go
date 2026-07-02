package sqldriver_test

import (
	"context"
	"strings"
	"testing"
)

// TestFDB_CoveringIndexScan pins that a query whose projection is fully covered
// by a secondary index (its indexed columns + the primary key it carries)
// produces a COVERING IndexScan with NO Fetch, while a query that needs a
// non-indexed column fetches the base record.
//
// Regression sentinel for review PR-#256 finding P2: the data-access path emits
// Fetch(IndexScan) for every value-index candidate (wrapScanPlanWithCoverage),
// deferring the covering decision to MergeProjectionAndFetchRule — which does
// the precise projection-columns-vs-index-columns check (more precise than the
// coarse no-final-compensation `isCovering` signal at scan-wrap time). This test
// proves that deferral actually eliminates the fetch for covering projections,
// so the always-Fetch shape is not a covering regression.
func TestFDB_CoveringIndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := setupPlanShapeDB(t, "covidx",
		"CREATE TABLE items (id BIGINT NOT NULL, cat STRING, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cat_idx ON items (cat)")

	for _, c := range []struct {
		name        string
		query       string
		wantCover   bool // expect "COVERING" + no Fetch
		wantNoFetch bool
	}{
		{"project_indexed_col", "SELECT cat FROM items WHERE cat = 'c1'", true, true},
		{"project_indexed_plus_pk", "SELECT id, cat FROM items WHERE cat = 'c1'", true, true},
		{"project_noncovered_col", "SELECT price FROM items WHERE cat = 'c1'", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := planExplainVia(t, ctx, db, c.query)
			hasFetch := strings.Contains(plan, "Fetch(")
			hasCovering := strings.Contains(plan, "COVERING")
			if !strings.Contains(plan, "IndexScan(CAT_IDX") {
				t.Fatalf("%s: expected an IndexScan on CAT_IDX, got: %s", c.query, plan)
			}
			if c.wantCover && !hasCovering {
				t.Errorf("%s: expected a COVERING index scan (MergeProjectionAndFetch should eliminate the fetch), got: %s", c.query, plan)
			}
			if c.wantNoFetch && hasFetch {
				t.Errorf("%s: expected NO Fetch for a covering projection (codex P2 regression), got: %s", c.query, plan)
			}
			if !c.wantNoFetch && !hasFetch {
				t.Errorf("%s: expected a Fetch for a non-covered projection, got: %s", c.query, plan)
			}
		})
	}
}
