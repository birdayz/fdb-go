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
		"CREATE TABLE items (id BIGINT, cat STRING, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cat_idx ON items (cat)")

	// The question each case asks is whether the scan answers from the INDEX
	// ENTRY or reads the base record — not whether a `Fetch(` node renders.
	// A bare `IndexScan(…)` is itself a fetching scan, so `Fetch(` is a
	// rendering detail that RFC-220 changed without changing the behaviour;
	// asserting on it made this test fail on a plan that was entirely correct.
	for _, c := range []struct {
		name  string
		query string
		// wantCovered: every projected column lives in the index entry, so the
		// scan must answer from the entry alone.
		wantCovered bool
	}{
		{"project_indexed_col", "SELECT cat FROM items WHERE cat = 'c1'", true},
		{"project_indexed_plus_pk", "SELECT id, cat FROM items WHERE cat = 'c1'", true},
		// PRICE is outside CAT_IDX's entry, so this scan must read base records.
		{"project_noncovered_col", "SELECT price FROM items WHERE cat = 'c1'", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := planExplainVia(t, ctx, db, c.query)
			if !strings.Contains(plan, "IndexScan(CAT_IDX") {
				t.Fatalf("%s: expected an IndexScan on CAT_IDX, got: %s", c.query, plan)
			}
			if c.wantCovered {
				assertScanAnswersFromIndexEntry(t, plan, "IndexScan(CAT_IDX")
			} else {
				assertScanReadsBaseRecords(t, plan, "IndexScan(CAT_IDX")
			}
		})
	}
}
