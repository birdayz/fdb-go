package embedded

// An aggregate index is scanned in reverse when a requested ordering wants its
// groups descending, as Java's data-access rule does for every aggregate
// candidate (aggregate-index-tests.yamsql:146, `select col1, sum(col2) from T1
// group by col1 order by col1 desc` is `AISCAN(MV1 <,> BY_GROUP REVERSE ...)`).
// The multi-aggregate intersection runs its legs and its merge in the same
// direction. With no descending request the
// scan stays forward. Rows ride on TestFDB_AggregateIndexReverseScan.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// aggregateScanDirections reports how many aggregate index scans (and merges
// over them) run forward and in reverse.
func aggregateScanDirections(plan plans.RecordQueryPlan) (forward, reverse int) {
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		switch n := p.(type) {
		case *plans.RecordQueryAggregateIndexPlan:
			if n.IsReverse() {
				reverse++
			} else {
				forward++
			}
		case *plans.RecordQueryMultiIntersectionOnValuesPlan:
			if n.IsReverse() {
				reverse++
			} else {
				forward++
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return forward, reverse
}

func TestAggregateIndexReverseScan(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T1 (id BIGINT, col1 BIGINT, col2 BIGINT, col3 BIGINT, PRIMARY KEY (id))
CREATE INDEX mv_sum AS SELECT SUM(col2) FROM T1 GROUP BY col1
CREATE INDEX mv_cnt AS SELECT COUNT(*) FROM T1 GROUP BY col1
CREATE INDEX mv_cntv AS SELECT COUNT(col2) FROM T1 GROUP BY col1
CREATE INDEX mv_cnt13 AS SELECT COUNT(*) FROM T1 GROUP BY col1, col3`
	cases := []struct {
		name             string
		sql              string
		forward, reverse int
		wantSort         bool
	}{
		{"count_desc", "SELECT col1, COUNT(*) FROM T1 GROUP BY col1 ORDER BY col1 DESC", 0, 1, false},
		{"count_asc_stays_forward", "SELECT col1, COUNT(*) FROM T1 GROUP BY col1 ORDER BY col1", 1, 0, false},
		{"count_unordered_stays_forward", "SELECT col1, COUNT(*) FROM T1 GROUP BY col1", 1, 0, false},
		{"sum_desc", "SELECT col1, SUM(col2) FROM T1 GROUP BY col1 ORDER BY col1 DESC", 0, 1, false},
		{"sum_unordered_stays_forward", "SELECT col1, SUM(col2) FROM T1 GROUP BY col1", 1, 0, false},
		// The multi-aggregate intersection: its SUM and COUNT(*) legs and the
		// merge reversed.
		{"sum_and_count_desc", "SELECT col1, SUM(col2), COUNT(*) FROM T1 GROUP BY col1 ORDER BY col1 DESC", 0, 3, false},
		// The bound prefix is fixed; the tail is served descending.
		{"bound_prefix_tail_desc", "SELECT col1, col3, COUNT(*) FROM T1 WHERE col1 = 2 GROUP BY col1, col3 ORDER BY col3 DESC", 0, 1, false},
		{"full_key_desc", "SELECT col1, col3, COUNT(*) FROM T1 GROUP BY col1, col3 ORDER BY col1 DESC, col3 DESC", 0, 1, false},
		// Mixed directions are no scan order: the sort stays.
		{"mixed_directions_sort", "SELECT col1, col3, COUNT(*) FROM T1 GROUP BY col1, col3 ORDER BY col1 DESC, col3", 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			forward, reverse := aggregateScanDirections(plan)
			if forward != c.forward || reverse != c.reverse {
				t.Errorf("aggregate scans forward=%d reverse=%d, want %d/%d\n  sql:  %s\n  plan: %s",
					forward, reverse, c.forward, c.reverse, c.sql, plan.Explain())
			}
			if sorts := inMemorySortsIn(plan); (sorts > 0) != c.wantSort {
				t.Errorf("in-memory sorts = %d, want sort=%v\n  sql:  %s\n  plan: %s", sorts, c.wantSort, c.sql, plan.Explain())
			}
		})
	}
}
