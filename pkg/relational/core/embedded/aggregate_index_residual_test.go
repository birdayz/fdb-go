package embedded

// RFC-248: a predicate on grouping columns the aggregate index's scan cannot
// bind is a residual filter above the scan, not a reason to full-scan the
// base table. Plan-shape pins over the typed tree (never EXPLAIN text): what
// the scan binds (comparison arity), what the filter carries (predicate
// count), that the aggregate index is reached, and — for the ORDER BY arms —
// that the residual filter passes the scan's rich ordering through so the
// sort stays elided. The rows ride on the sqldriver twin
// (TestFDB_AggregateIndexResidual).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// aggregateResidualShape reports the aggregate index scan's comparison arity
// and the predicate count of the filter directly above the aggregate plan
// (or the merge/intersection it feeds), -1 for "no filter".
func aggregateResidualShape(plan plans.RecordQueryPlan) (aggregates, scanComparisons, residuals int) {
	residuals = -1
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		switch n := p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			if len(n.GetChildren()) == 1 {
				switch n.GetChildren()[0].(type) {
				case *plans.RecordQueryAggregateIndexPlan, *plans.RecordQueryMultiIntersectionOnValuesPlan:
					residuals = len(n.GetPredicates())
				}
			}
		case *plans.RecordQueryAggregateIndexPlan:
			aggregates++
			if idx := n.GetIndexPlan(); idx != nil {
				scanComparisons = len(idx.GetScanComparisons())
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return aggregates, scanComparisons, residuals
}

func TestAggregateIndexResidualOverGroupingColumns(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT, a STRING, b STRING, c STRING, v BIGINT, d DOUBLE, PRIMARY KEY (id))
CREATE INDEX sum_abc AS SELECT SUM(v) FROM T GROUP BY a, b, c
CREATE INDEX cnt_abc AS SELECT COUNT(*) FROM T GROUP BY a, b, c
CREATE INDEX cnt_d_a AS SELECT COUNT(*) FROM T GROUP BY d, a
CREATE TABLE CUST (id BIGINT, b STRING, region STRING, PRIMARY KEY (id))`

	cases := []struct {
		name      string
		sql       string
		wantAgg   bool // served by an aggregate index at all
		wantScan  int  // scan comparison arity
		wantResid int  // predicates on the residual filter, -1 = none
		wantSort  bool
	}{
		{"non_leading_equality_sum", "SELECT a, b, c, SUM(v) FROM t WHERE b = 'x' GROUP BY a, b, c", true, 0, 1, false},
		{"non_leading_equality_count", "SELECT a, b, c, COUNT(*) FROM t WHERE b = 'x' GROUP BY a, b, c", true, 0, 1, false},
		{"gap_in_prefix", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c", true, 1, 1, false},
		{"leading_inequality_string", "SELECT a, b, c, COUNT(*) FROM t WHERE a > 'm' GROUP BY a, b, c", true, 1, -1, false},
		{"leading_inequality_sum_companion", "SELECT a, b, c, SUM(v) FROM t WHERE a > 'm' GROUP BY a, b, c", true, 1, -1, false},
		{"range_then_equality", "SELECT a, b, c, COUNT(*) FROM t WHERE a > 'm' AND b = 'y' GROUP BY a, b, c", true, 1, 1, false},
		{"two_bounds_fold_to_one_range", "SELECT a, b, c, COUNT(*) FROM t WHERE a > 'm' AND a < 'z' GROUP BY a, b, c", true, 1, -1, false},
		// The first equality binds the scan and the second is re-applied above
		// it (Java's merge keeps the range and residualises the incoming
		// equality); the residual reads no group and the query is empty.
		{"contradictory_equalities_bind_the_first", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a = 'y' GROUP BY a, b, c", true, 1, 1, false},
		{"equality_beside_inequality_binds_the_equality", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a > 'm' GROUP BY a, b, c", true, 1, 1, false},
		{"is_null_residual", "SELECT a, b, c, COUNT(*) FROM t WHERE b IS NULL GROUP BY a, b, c", true, 0, 1, false},
		{"in_list_residual", "SELECT a, b, c, COUNT(*) FROM t WHERE b IN ('x', 'y') GROUP BY a, b, c", true, 0, 1, false},
		{"column_to_column_residual", "SELECT a, b, c, COUNT(*) FROM t WHERE a = b GROUP BY a, b, c", true, 0, 1, false},
		{"or_residual", "SELECT a, b, c, SUM(v) FROM t WHERE b = 'x' OR c = 'z' GROUP BY a, b, c", true, 0, 1, false},
		// A one-sided DOUBLE range cannot be a scan bound (tuple order is not
		// the comparator's), so it is peeled into the residual, not declined.
		{"double_range_demotes_to_residual", "SELECT d, a, COUNT(*) FROM t WHERE d > 1.5 GROUP BY d, a", true, 0, 1, false},
		// A LEADING IS NULL binds as an equality range on the [null] key (Java's
		// ScanComparisons classifies IS NULL as EQUALITY) — new reach, since the
		// old builder bound `=` only.
		{"leading_is_null_binds", "SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL GROUP BY a, b, c", true, 1, -1, false},
		{"leading_is_null_then_gap", "SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL AND c = 'z' GROUP BY a, b, c", true, 1, 1, false},
		// The third scan site: two aggregates over two indexes intersect on the
		// grouping key, with the residual above the intersection.
		{"multi_aggregate_intersection_residual", "SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE b = 'x' GROUP BY a, b, c", true, 0, 1, false},
		{"multi_aggregate_intersection_bound_and_residual", "SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c", true, 1, 1, false},
		// The filter passes the scan's rich ordering through: a FIXED and b
		// sorted, so both requests are served with no sort.
		{"residual_keeps_fixed_binding_desc", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY a DESC, b", true, 1, 1, false},
		{"residual_keeps_sorted_tail", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true, 1, 1, false},
		// The grouping-column leaf under a function / CAST wrapper: the walk
		// descends to the leaf, the rewrite replaces only the leaf and keeps
		// the wrapper.
		{"wrapped_leaf_function", "SELECT a, b, c, COUNT(*) FROM t WHERE UPPER(b) = 'P' GROUP BY a, b, c", true, 0, 1, false},
		{"wrapped_leaf_cast", "SELECT a, b, c, COUNT(*) FROM t WHERE CAST(b AS STRING) = 'p' AND a = 'x' GROUP BY a, b, c", true, 1, 1, false},
		// A correlated residual: `t.b = c.b` reads the OUTER row's b — the same
		// NAME as the grouping column — and must stay correlated to it; the
		// input-rooted read moves onto the aggregate row, the outer one does
		// not. Two residuals (the correlated one and c = 'z'), a bound on a.
		{"correlated_outer_field_same_name", "SELECT c.id, (SELECT COUNT(*) FROM t WHERE t.b = c.b AND t.a = 'x' AND t.c = 'z' GROUP BY t.a, t.b, t.c) FROM cust c", true, 1, 2, false},
		// Decline: a leaf on the aggregation input.
		{"non_grouping_leaf_declines", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND v > 0 GROUP BY a, b, c", false, 0, -1, true},
		{"column_to_column_with_input_declines", "SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND v = id GROUP BY a, b, c", false, 0, -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			aggs, scan, resid := aggregateResidualShape(plan)
			if (aggs > 0) != c.wantAgg {
				t.Fatalf("aggregate index reached = %v, want %v\n  sql:  %s\n  plan: %s", aggs > 0, c.wantAgg, c.sql, plan.Explain())
			}
			if !c.wantAgg {
				if inMemorySortsIn(plan) == 0 && c.wantSort {
					t.Fatalf("declined shape lost its sort\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
				}
				return
			}
			if scan != c.wantScan {
				t.Errorf("scan binds %d comparison(s), want %d\n  sql:  %s\n  plan: %s", scan, c.wantScan, c.sql, plan.Explain())
			}
			if resid != c.wantResid {
				t.Errorf("residual filter carries %d predicate(s), want %d (-1 = no filter)\n  sql:  %s\n  plan: %s", resid, c.wantResid, c.sql, plan.Explain())
			}
			if sorts := inMemorySortsIn(plan); (sorts > 0) != c.wantSort {
				t.Errorf("in-memory sorts = %d, want sort=%v: the residual filter must pass the scan's rich ordering through\n  sql:  %s\n  plan: %s", sorts, c.wantSort, c.sql, plan.Explain())
			}
		})
	}
}

// TestAggregateIndexResidual_RecordTypedGroupingKeyIsUnreachable is a NEGATIVE
// result that guards an ordinal argument. RFC-248 places the residual BELOW
// projectAggregateResultToGroupBy because on the leaf row grouping column i
// is ordinal i of the candidate's groupCols, whereas the GroupBy row's ordinals
// differ when a grouping key is RECORD-typed (MatchesGroupBy expands such a
// key to its primitive leaves, so the GroupBy row carries fewer, wider
// columns than the candidate). That rewrite has no rows pin over such a shape,
// so it must stay unreachable until it gets one.
//
// Java cannot plan a RECORD-typed grouping key at all (UnableToPlanException,
// measured by the "WS-J nested-grouping aggregate index plan oracle"): its
// grouping subsumption matches the leaves, and the query's result value cannot
// be pulled up through them. Go answers it through Scan + Aggregate, a read
// extension, and keeps it OFF the aggregate index by the same pull-up
// (AggregateIndexMatchCandidate.groupingKeysMatch). Nested LEAF grouping
// columns over the same index ARE served (TestAggregateIndexNestedLeafGrouping
// Columns), which is what makes this population reachable at all.
func TestAggregateIndexResidual_RecordTypedGroupingKeyIsUnreachable(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT)
CREATE TYPE AS STRUCT ONE (city STRING)
CREATE TABLE T_S (id BIGINT, home ADDR, solo ONE, cat STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX cnt_home_cat AS SELECT COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat
CREATE INDEX sum_home_cat AS SELECT SUM(v) FROM T_S GROUP BY home.city, home.zip, cat
CREATE INDEX cnt_solo AS SELECT COUNT(*) FROM T_S GROUP BY solo.city
CREATE INDEX cnt_cat AS SELECT COUNT(*) FROM T_S GROUP BY cat`
	served := func(q string) bool {
		t.Helper()
		plan, err := PlanPhysicalForTest(q, schema, nil)
		if err != nil {
			t.Fatalf("the schema (accepted by Java 4.14.2.0) or the read must plan: %v\n  sql: %s", err, q)
		}
		aggs, _, _ := aggregateResidualShape(plan)
		return aggs > 0
	}
	// Positive controls, same schema: a flat grouping and the nested leaves the
	// RECORD keys below expand to ARE served, so a refusal below is the pull-up
	// declining, not a detector that never sees an aggregate index plan.
	for _, q := range []string{
		"SELECT cat, COUNT(*) FROM T_S GROUP BY cat",
		"SELECT home.city, home.zip, cat, COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat",
		"SELECT solo.city, COUNT(*) FROM T_S GROUP BY solo.city",
	} {
		if !served(q) {
			t.Fatalf("control: not served by its aggregate index\n  sql: %s", q)
		}
	}
	for _, q := range []string{
		"SELECT home, cat, COUNT(*) FROM T_S WHERE cat = 'x' GROUP BY home, cat",
		"SELECT home, cat, SUM(v) FROM T_S WHERE cat = 'x' GROUP BY home, cat",
		"SELECT home, cat, COUNT(*) FROM T_S GROUP BY home, cat",
		// One leaf: the widths agree, the RECORD-typed slot does not.
		"SELECT solo, COUNT(*) FROM T_S GROUP BY solo",
	} {
		if served(q) {
			t.Fatalf("a RECORD-typed grouping key is served by an aggregate index over nested struct fields: "+
				"RFC-248's residual rewrite (grouping column i = leaf-row ordinal i, below the projection) "+
				"needs a rows pin over this shape first, and Java cannot plan this query at all\n  sql: %s", q)
		}
	}
}

// TestAggregateIndexNestedLeafGroupingColumns: a GROUP BY over nested LEAF
// fields is served by the aggregate index that groups by them, the leaves one
// to one with the candidate's columns by full path (RFC-257 WS-J 3.3b). The
// first three are the measured Java shapes ("WS-J nested-grouping aggregate
// index plan oracle"): unbound, a residual on the third key, the leading
// equality bound. Rows ride on TestFDB_AggregateIndexNestedLeafGrouping. A
// nested AGGREGATED column is not creatable through SQL in either engine (the
// "sum_grouped_by_nested_and_top" WS-J shape), so it is pinned at the rule
// (TestAggregateDataAccessRule_NestedColumnPaths).
func TestAggregateIndexNestedLeafGroupingColumns(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT)
CREATE TABLE T_S (id BIGINT, home ADDR, office ADDR, city STRING, cat STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX cnt_home_cat AS SELECT COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat
CREATE INDEX sum_home_cat AS SELECT SUM(v) FROM T_S GROUP BY home.city, home.zip, cat
CREATE INDEX cnt_home_office AS SELECT COUNT(*) FROM T_S GROUP BY home.city, office.city
CREATE INDEX cnt_cat AS SELECT COUNT(*) FROM T_S GROUP BY cat`
	cases := []struct {
		name      string
		sql       string
		wantAgg   bool
		wantScan  int
		wantResid int
		wantSort  bool
	}{
		{"unbound", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat", true, 0, -1, false},
		{"residual_on_third_key", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE cat = 'x' GROUP BY home.city, home.zip, cat", true, 0, 1, false},
		{"leading_equality_bound", "SELECT home.city, home.zip, cat, SUM(v) FROM T_S WHERE home.city = 'a' GROUP BY home.city, home.zip, cat", true, 1, -1, false},
		{"equality_then_range", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE home.city = 'a' AND home.zip > 3 GROUP BY home.city, home.zip, cat", true, 2, -1, false},
		{"bound_and_residual", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE home.city = 'a' AND cat = 'x' GROUP BY home.city, home.zip, cat", true, 1, 1, false},
		{"residual_on_nested_key", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE home.zip = 3 GROUP BY home.city, home.zip, cat", true, 0, 1, false},
		{"order_by_nested_tail_after_binding", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE home.city = 'a' GROUP BY home.city, home.zip, cat ORDER BY home.zip, cat", true, 1, -1, false},
		// The plan's row carries the GroupBy's names (T_S.HOME.CITY), not the
		// index's labels (CITY): the ordering key is the row's slot under the
		// row's own name.
		{"order_by_nested_unbound", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat ORDER BY home.city, home.zip", true, 0, -1, false},
		{"same_leaf_name_two_parents", "SELECT home.city, office.city, COUNT(*) FROM T_S WHERE office.city = 'b' GROUP BY home.city, office.city", true, 0, 1, false},
		// The top-level CITY is not HOME.CITY: no index groups by it.
		{"top_level_same_leaf_declines", "SELECT city, home.zip, cat, COUNT(*) FROM T_S GROUP BY city, home.zip, cat", false, 0, -1, true},
		{"swapped_parents_decline", "SELECT office.city, home.city, COUNT(*) FROM T_S GROUP BY office.city, home.city", false, 0, -1, true},
		// A residual reading a nested field that is not a grouping column reads
		// the aggregation input and declines.
		{"input_leaf_declines", "SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE office.zip = 1 GROUP BY home.city, home.zip, cat", false, 0, -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			aggs, scan, resid := aggregateResidualShape(plan)
			if (aggs > 0) != c.wantAgg {
				t.Fatalf("aggregate index reached = %v, want %v\n  sql:  %s\n  plan: %s", aggs > 0, c.wantAgg, c.sql, plan.Explain())
			}
			if sorts := inMemorySortsIn(plan); (sorts > 0) != c.wantSort {
				t.Errorf("in-memory sorts = %d, want sort=%v\n  sql:  %s\n  plan: %s", sorts, c.wantSort, c.sql, plan.Explain())
			}
			if !c.wantAgg {
				return
			}
			if scan != c.wantScan {
				t.Errorf("scan binds %d comparison(s), want %d\n  sql:  %s\n  plan: %s", scan, c.wantScan, c.sql, plan.Explain())
			}
			if resid != c.wantResid {
				t.Errorf("residual filter carries %d predicate(s), want %d (-1 = no filter)\n  sql:  %s\n  plan: %s", resid, c.wantResid, c.sql, plan.Explain())
			}
		})
	}
}
