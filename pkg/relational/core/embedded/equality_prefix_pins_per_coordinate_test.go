package embedded

// FIXED is a per-coordinate fact, not a prefix length. Under `d = 0.0 AND b = 1`
// over INDEX(d, b) the signed-zero equality on D spans two physical blocks — so
// D is ordered only in the scan's direction and nothing after the bound prefix
// is globally ordered — but every admitted row still carries b = 1, one
// physical key within each of D's blocks. B therefore binds FIXED, which is
// direction-free: `ORDER BY b DESC` is served by a forward scan with no sort.
// Reading the pinned coordinates as the LENGTH of the leading pinned run
// demoted B to SORTED and put that sort back (splitKeyOrder, plans/ordering.go);
// the same split serves the aggregate index's grouping key, so both plan types
// are pinned here, over the typed tree.
//
// D itself is SORTED in the scan's direction, never FIXED: `ORDER BY d` is
// served forward and `ORDER BY d DESC` by turning the scan around (reverse
// opens the +0.0 block before the -0.0 block, which IS descending under
// Double.compare). The control is the PK suffix after the widened D: it
// restarts at the block boundary, so `ORDER BY id` keeps its sort.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func indexScansIn(plan plans.RecordQueryPlan) int {
	n := 0
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		switch p.(type) {
		case *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan, *plans.RecordQueryAggregateIndexPlan:
			n++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return n
}

func TestPinnedCoordinateAfterWidenedOneElidesSort(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE t (id BIGINT, d DOUBLE, b BIGINT, PRIMARY KEY (id))
CREATE INDEX t_d_b ON t (d, b)
CREATE INDEX t_cnt_d_b AS SELECT COUNT(*) FROM t GROUP BY d, b`

	cases := []struct {
		name     string
		sql      string
		wantSort bool
	}{
		{"index_pinned_after_widened_desc", "SELECT id FROM t WHERE d = 0.0 AND b = 1 ORDER BY b DESC", false},
		{"index_pinned_after_widened_asc", "SELECT id FROM t WHERE d = 0.0 AND b = 1 ORDER BY b", false},
		{"index_widened_forward", "SELECT id FROM t WHERE d = 0.0 AND b = 1 ORDER BY d", false},
		{"index_widened_reverse_turns_scan", "SELECT id FROM t WHERE d = 0.0 AND b = 1 ORDER BY d DESC", false},
		{"aggregate_pinned_after_widened_desc", "SELECT d, b, COUNT(*) FROM t WHERE d = 0.0 AND b = 1 GROUP BY d, b ORDER BY b DESC", false},
		{"aggregate_pinned_after_widened_asc", "SELECT d, b, COUNT(*) FROM t WHERE d = 0.0 AND b = 1 GROUP BY d, b ORDER BY b", false},
		// Control.
		{"index_suffix_after_widened_needs_sort", "SELECT id FROM t WHERE d = 0.0 AND b = 1 ORDER BY id", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if indexScansIn(plan) == 0 {
				t.Fatalf("the query does not reach an index scan, so the sort claim is about a different plan\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			}
			sorts := inMemorySortsIn(plan)
			switch {
			case c.wantSort && sorts == 0:
				t.Errorf("the sort was elided although the scan does not deliver the requested order past the widened coordinate\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			case !c.wantSort && sorts > 0:
				t.Errorf("redundant in-memory sort: b = 1 is one physical key within each of d's blocks, so b is FIXED and any direction on it is served\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			}
		})
	}
}
