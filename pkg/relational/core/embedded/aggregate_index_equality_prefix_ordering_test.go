package embedded

// An aggregate index stores one entry per group in group-key order. When the
// scan binds a leading grouping column by equality, every emitted group shares
// that value and the groups arrive ordered by the columns AFTER it — so
// `WHERE b = 1 GROUP BY b, a ORDER BY a` needs no sort. The plan used to carry
// one: RecordQueryAggregateIndexPlan claimed all grouping columns as sorted keys
// and never treated the bound prefix as fixed, unlike the value index scan and
// unlike Java's AggregateIndexMatchCandidate.computeOrderingFromScanComparisons.
//
// Plan-shape pins over the typed tree (never EXPLAIN text): the fixed-prefix
// shapes plan without an in-memory sort; the controls that must KEEP one — a
// signed-zero equality on a DOUBLE grouping column, a DOUBLE in the sorted
// tail, an inequality on the prefix, a fully unbound scan — still carry it.
// Row correctness rides on the sqldriver twin
// (TestFDB_AggregateIndexEqualityPrefixOrdering).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func inMemorySortsIn(plan plans.RecordQueryPlan) int {
	n := 0
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if _, ok := p.(*plans.RecordQueryInMemorySortPlan); ok {
			n++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return n
}

func aggregateIndexesIn(plan plans.RecordQueryPlan) int {
	n := 0
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if _, ok := p.(*plans.RecordQueryAggregateIndexPlan); ok {
			n++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return n
}

func TestAggregateIndexEqualityPrefixElidesSort(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE t (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, d DOUBLE, PRIMARY KEY (pk1, pk2))
CREATE INDEX t_cnt_pk1_a AS SELECT COUNT(*) FROM t GROUP BY pk1, a
CREATE INDEX t_cnt_b_a AS SELECT COUNT(*) FROM t GROUP BY b, a
CREATE INDEX t_max_b_a AS SELECT MAX(pk2) FROM t GROUP BY b, a
CREATE INDEX t_cnt_d_a AS SELECT COUNT(*) FROM t GROUP BY d, a
CREATE INDEX t_cnt_b_d AS SELECT COUNT(*) FROM t GROUP BY b, d`

	cases := []struct {
		name     string
		sql      string
		wantSort bool
	}{
		{"count_prefix_fixed", "SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a", false},
		{"count_prefix_fixed_full_order", "SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY b, a", false},
		{"count_pk_prefix_fixed", "SELECT pk1, a, COUNT(*) FROM t WHERE pk1 = 1 GROUP BY pk1, a ORDER BY a", false},
		{"max_prefix_fixed", "SELECT b, a, MAX(pk2) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a", false},
		{"count_prefix_fixed_limit", "SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a LIMIT 2", false},
		{"double_prefix_pinned", "SELECT d, a, COUNT(*) FROM t WHERE d = 1.0 GROUP BY d, a ORDER BY a", false},
		// Controls: the scan does not deliver the requested order here.
		{"unbound_needs_sort", "SELECT b, a, COUNT(*) FROM t GROUP BY b, a ORDER BY a", true},
		{"double_prefix_signed_zero", "SELECT d, a, COUNT(*) FROM t WHERE d = 0.0 GROUP BY d, a ORDER BY a", true},
		{"double_tail", "SELECT b, d, COUNT(*) FROM t WHERE b = 1 GROUP BY b, d ORDER BY d", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if aggregateIndexesIn(plan) == 0 {
				t.Fatalf("the query no longer reaches the aggregate index, so the sort claim is about a different plan\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			}
			sorts := inMemorySortsIn(plan)
			switch {
			case c.wantSort && sorts == 0:
				t.Errorf("the sort was elided although the aggregate scan does not deliver the requested order\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			case !c.wantSort && sorts > 0:
				t.Errorf("redundant in-memory sort: the bound grouping prefix leaves the scan ordered by the requested key\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			}
		})
	}
}
