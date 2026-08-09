package embedded_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/coveringleaf"
	"fdb.dev/pkg/relational/core/embedded"
)

// The schema is the SHARED one — the driver-side metadata test asserts against
// the same tables, index and queries, and the two were hand-duplicated until
// coveringleaf gave them one definition to import. See that package for why the
// duplication was load-bearing rather than cosmetic.
const rfc220ProbeDDL = coveringleaf.DDL

// TestCoveringLeafMetadataQueriesStillPlanAsCoveringLeaves pins the premise of
// the column-metadata test over in the sqldriver package
// (TestFDB_CoveringLeafKeepsColumnTypeMetadata). That test is only meaningful
// while its two covering queries actually plan with the projection sitting
// DIRECTLY over a covering scan — the shape in which the leaf walk has to
// unwrap the covering plan to find the index plan held inside it.
//
// If a future cost change makes either query plan a fetching or plain scan
// instead, that test keeps passing while silently no longer exercising the
// case, which is the failure mode where coverage evaporates without a signal.
// It is pinned here, next to the planner, because that is where it would move.
func TestCoveringLeafMetadataQueriesStillPlanAsCoveringLeaves(t *testing.T) {
	t.Parallel()
	// Driven from the shared probe table, so a query added or edited on the
	// driver side cannot silently go unpinned here.
	if len(coveringleaf.Probes) == 0 {
		t.Fatal("coveringleaf.Probes is empty — this pin would report PASS having asserted nothing")
	}
	for _, tc := range coveringleaf.Probes {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			p, err := embedded.PlanQueryForTest(tc.Query, rfc220ProbeDDL, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := strings.Contains(p, "COVERING"); got != tc.Covering {
				t.Errorf("covering leaf = %v, want %v\n  plan: %s\n"+
					"TestFDB_CoveringLeafKeepsColumnTypeMetadata depends on this shape; "+
					"update both together or it stops testing what it claims",
					got, tc.Covering, p)
			}
		})
	}
}

// TestStreamingAggEnumeratesEveryOrderedChildMember pins that a GROUP BY whose
// grouping key is served by an index streams the aggregate off that index
// instead of sorting the whole table in memory.
//
// The rule that builds the aggregate applies two admissibility filters to its
// ordered inner — a general admissibility check and a skip for full-range Fetch
// wrappers, which read every row via random primary-key lookups. It used to
// locate that inner with a single pick: the FIRST ordered physical member of
// the child group. When the first member was one the filters reject and a LATER
// member was admissible, the rule yielded no index path at all and the
// aggregate silently fell back to InMemorySort(FullScan) — reading and
// materializing the entire table with an ordered index scan sitting unused in
// the same memo group.
//
// That is a plan-quality defect a rows-only test cannot see: both plans return
// identical, correctly grouped rows. It is pinned here on the plan SHAPE, and
// on the specific shape that breaks — the filtered variants, where the index is
// legitimately usable.
//
// The nofilter case is the control and is NOT a bug: a value index stores no
// entry for a NULL key, so GROUP BY over a nullable column without an
// IS NOT NULL filter must not stream off the index or it would drop the NULL
// group entirely. It asserts the sort STAYS.
func TestStreamingAggEnumeratesEveryOrderedChildMember(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		sql        string
		wantIndex  bool
		wantReason string
	}{
		{
			name: "cte_group_by_over_index",
			sql: `WITH cat_totals AS (
				SELECT category, SUM(price) AS total FROM products
				WHERE category IS NOT NULL GROUP BY category
			) SELECT category, total FROM cat_totals ORDER BY category`,
			wantIndex:  true,
			wantReason: "IS NOT NULL excludes the NULL group, so idx_cat supplies the grouping order",
		},
		{
			name: "bare_group_by_over_index",
			sql: `SELECT category, SUM(price) AS total FROM products
				WHERE category IS NOT NULL GROUP BY category ORDER BY category`,
			wantIndex:  true,
			wantReason: "same shape without the CTE wrapper",
		},
		{
			name: "nullable_group_by_must_not_use_the_index",
			sql: `SELECT category, SUM(price) AS total FROM products
				GROUP BY category ORDER BY category`,
			wantIndex:  false,
			wantReason: "a value index has no entry for a NULL key, so streaming off idx_cat would drop the NULL group",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := embedded.PlanQueryForTest(tc.sql, rfc220ProbeDDL, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			usesIndex := strings.Contains(plan, "IDX_CAT")
			sorts := strings.Contains(plan, "InMemorySort")
			if usesIndex != tc.wantIndex {
				t.Errorf("uses IDX_CAT = %v, want %v (%s)\n  plan: %s",
					usesIndex, tc.wantIndex, tc.wantReason, plan)
			}
			// The two halves are asserted separately on purpose. "Uses the
			// index" alone would pass on a plan that scans the index AND then
			// sorts it, which is the regression wearing a disguise.
			if sorts == tc.wantIndex {
				t.Errorf("InMemorySort present = %v, want %v (%s)\n  plan: %s",
					sorts, !tc.wantIndex, tc.wantReason, plan)
			}
		})
	}
}
