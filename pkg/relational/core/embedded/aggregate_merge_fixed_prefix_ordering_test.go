package embedded

// An aggregate index scan and the multi-aggregate intersection order their
// rows by the grouping key, and a grouping column every leg binds by equality
// is FIXED in that order, as it is in each leg's own scan: Java derives the
// intersection's ordering by merging its children's (Ordering.merge with the
// INTERSECTION operator, which keeps their common fixed bindings), so
// `WHERE a = 'x' GROUP BY a, b, c ORDER BY b, c` needs no sort over a SUM.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func multiIntersectionsIn(plan plans.RecordQueryPlan) int {
	return plansOfTypeIn[*plans.RecordQueryMultiIntersectionOnValuesPlan](plan)
}

func plansOfTypeIn[P plans.RecordQueryPlan](plan plans.RecordQueryPlan) int {
	n := 0
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if _, ok := p.(P); ok {
			n++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return n
}

func TestAggregateMergeKeepsFixedPrefixOrdering(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT, a STRING, b BIGINT, c STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX sum_abc AS SELECT SUM(v) FROM T GROUP BY a, b, c
CREATE INDEX cnt_abc AS SELECT COUNT(*) FROM T GROUP BY a, b, c
CREATE INDEX cntv_abc AS SELECT COUNT(v) FROM T GROUP BY a, b, c`
	cases := []struct {
		name      string
		sql       string
		wantMerge bool
		wantSort  bool
	}{
		{"sum_tail_after_bound_prefix", "SELECT a, b, c, SUM(v) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY b, c", false, false},
		{"sum_tail_descending", "SELECT a, b, c, SUM(v) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY b DESC, c DESC", false, false},
		{"sum_two_bound", "SELECT a, b, c, SUM(v) FROM T WHERE a = 'x' AND b = 2 GROUP BY a, b, c ORDER BY c", false, false},
		{"multi_aggregate_tail", "SELECT a, b, c, SUM(v), COUNT(*) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY b, c", true, false},
		{"sum_full_key", "SELECT a, b, c, SUM(v) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY a, b, c", false, false},
		// c after the unbound b is no order of the scan.
		{"sum_skipping_unbound", "SELECT a, b, c, SUM(v) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY c", false, true},
		{"multi_aggregate_skipping_unbound", "SELECT a, b, c, SUM(v), COUNT(*) FROM T WHERE a = 'x' GROUP BY a, b, c ORDER BY c", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plansOfTypeIn[*plans.RecordQueryAggregateIndexPlan](plan) == 0 {
				t.Fatalf("not served by an aggregate index, so its ordering is not under test\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			}
			if (multiIntersectionsIn(plan) > 0) != c.wantMerge {
				t.Fatalf("multi-aggregate merge = %v, want %v\n  sql:  %s\n  plan: %s", multiIntersectionsIn(plan) > 0, c.wantMerge, c.sql, plan.Explain())
			}
			if sorts := inMemorySortsIn(plan); (sorts > 0) != c.wantSort {
				t.Errorf("in-memory sorts = %d, want sort=%v\n  sql:  %s\n  plan: %s", sorts, c.wantSort, c.sql, plan.Explain())
			}
		})
	}
}
