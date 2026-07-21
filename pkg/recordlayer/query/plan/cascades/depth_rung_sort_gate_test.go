package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The three structural DEPTH tiebreaks (type-filter / fetch / distinct) are gated
// to abstain when either side carries an in-memory sort (RFC-184 W2): a redundant
// InMemorySort is an extra top node that inflates every descendant's depth by +1,
// so a depth tiebreak between a sorted and a sort-elided plan compares depths from
// different roots and always favours the taller (sorted) plan — the sort-vs-
// elision decision is owned by criterion #12 (fewer in-memory sorts). These tests
// prove the gate is NARROW: between two SORT-FREE plans each depth rung STILL
// fires and picks its preferred (pushed) variant exactly as before the gate; a
// sort on one side then flips the decision to the sort-free plan via criterion #12.

func scanT() plans.RecordQueryPlan {
	return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
}

func indexT() plans.RecordQueryPlan {
	return plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false)
}

// TestDepthRung_TypeFilterDepth_FiresWhenSortFree — the type-filter-depth rung
// prefers the DEEPER (pushed-below-the-limit) type filter, and still does so
// between two sort-free plans after the gate.
func TestDepthRung_TypeFilterDepth_FiresWhenSortFree(t *testing.T) {
	t.Parallel()
	// Same node multiset (1 limit, 1 type filter, 1 scan) — only the type-filter
	// depth differs. Deeper: Limit(TypeFilter(Scan)); shallower: TypeFilter(Limit(Scan)).
	deeper := plans.NewRecordQueryLimitPlan(plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scanT()), 10, 0)
	shallower := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, plans.NewRecordQueryLimitPlan(scanT(), 10, 0))

	if !PlanningCostModelLess(deeper, shallower) {
		t.Error("type-filter-depth rung must STILL fire between sort-free plans: deeper type filter wins")
	}
	if PlanningCostModelLess(shallower, deeper) {
		t.Error("type-filter-depth rung asymmetry broken")
	}

	// Gate: a sort on the otherwise-winning deeper side makes the depth rung
	// abstain, so criterion #12 (fewer sorts) picks the sort-free shallower plan.
	deeperSorted := plans.NewRecordQueryInMemorySortPlan(deeper, nil)
	if !PlanningCostModelLess(shallower, deeperSorted) {
		t.Error("with a sort present the type-filter-depth rung must abstain and the sort-free plan must win")
	}
}

// TestDepthRung_FetchDepth_FiresWhenSortFree — the fetch-depth rung prefers the
// SHALLOWER (later, near-root) fetch, and still does so between two sort-free
// plans after the gate. Both sides carry an index scan so the fetch block runs.
func TestDepthRung_FetchDepth_FiresWhenSortFree(t *testing.T) {
	t.Parallel()
	fetch := func(inner plans.RecordQueryPlan) plans.RecordQueryPlan {
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	}
	// Same node multiset (1 limit, 1 fetch, 1 index) — only the fetch depth differs.
	shallower := fetch(plans.NewRecordQueryLimitPlan(indexT(), 10, 0)) // Fetch(Limit(Index)) — fetchDepth 0
	deeper := plans.NewRecordQueryLimitPlan(fetch(indexT()), 10, 0)    // Limit(Fetch(Index)) — fetchDepth 1

	if !PlanningCostModelLess(shallower, deeper) {
		t.Error("fetch-depth rung must STILL fire between sort-free plans: shallower fetch wins")
	}
	if PlanningCostModelLess(deeper, shallower) {
		t.Error("fetch-depth rung asymmetry broken")
	}

	// Gate: a sort on the otherwise-winning shallower side makes the depth rung
	// abstain, so criterion #12 (fewer sorts) picks the sort-free deeper plan.
	shallowerSorted := plans.NewRecordQueryInMemorySortPlan(shallower, nil)
	if !PlanningCostModelLess(deeper, shallowerSorted) {
		t.Error("with a sort present the fetch-depth rung must abstain and the sort-free plan must win")
	}
}

// TestStructuralGate_MapFilterCount_FiresWhenSortFree — the map/filter-node-count
// rung (NON-depth) prefers FEWER filter nodes, and still does so between two
// sort-free plans after the gate. This is the rung that let a redundant sort over
// a clean single-filter inner beat a sort-elided sibling whose residual was
// pushdown-split into two filter nodes (rowdiff seeds 132/214).
func TestStructuralGate_MapFilterCount_FiresWhenSortFree(t *testing.T) {
	t.Parallel()
	pred := func(field string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			&values.FieldValue{Field: field, Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	}
	// Same total residual (2 preds) but different filter-NODE count: one node vs a
	// pushdown-split two nodes. Only the map/filter count differs.
	oneNode := plans.NewRecordQueryPredicatesFilterPlan(scanT(), []predicates.QueryPredicate{pred("a"), pred("b")})
	twoNodes := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryPredicatesFilterPlan(scanT(), []predicates.QueryPredicate{pred("a")}),
		[]predicates.QueryPredicate{pred("b")})

	if !PlanningCostModelLess(oneNode, twoNodes) {
		t.Error("map/filter-count rung must STILL fire between sort-free plans: fewer filter nodes wins")
	}
	if PlanningCostModelLess(twoNodes, oneNode) {
		t.Error("map/filter-count rung asymmetry broken")
	}

	// Gate: a sort on the otherwise-winning fewer-node side makes the count rung
	// abstain, so criterion #12 (fewer sorts) picks the sort-free two-node plan —
	// the fix for the redundant-sort-over-split-residual case (rowdiff 132/214).
	oneNodeSorted := plans.NewRecordQueryInMemorySortPlan(oneNode, nil)
	if !PlanningCostModelLess(twoNodes, oneNodeSorted) {
		t.Error("with a sort present the map/filter-count rung must abstain and the sort-free plan must win")
	}
}

// TestDepthRung_DistinctDepth_FiresWhenSortFree — the distinct-depth rung prefers
// the DEEPER distinct, and still does so between two sort-free plans after the gate.
func TestDepthRung_DistinctDepth_FiresWhenSortFree(t *testing.T) {
	t.Parallel()
	// Same node multiset (1 limit, 1 distinct, 1 scan) — only the distinct depth differs.
	deeper := plans.NewRecordQueryLimitPlan(plans.NewRecordQueryDistinctPlan(scanT()), 10, 0)
	shallower := plans.NewRecordQueryDistinctPlan(plans.NewRecordQueryLimitPlan(scanT(), 10, 0))

	if !PlanningCostModelLess(deeper, shallower) {
		t.Error("distinct-depth rung must STILL fire between sort-free plans: deeper distinct wins")
	}
	if PlanningCostModelLess(shallower, deeper) {
		t.Error("distinct-depth rung asymmetry broken")
	}

	// Gate: a sort on the otherwise-winning deeper side makes the depth rung
	// abstain, so criterion #12 (fewer sorts) picks the sort-free shallower plan.
	deeperSorted := plans.NewRecordQueryInMemorySortPlan(deeper, nil)
	if !PlanningCostModelLess(shallower, deeperSorted) {
		t.Error("with a sort present the distinct-depth rung must abstain and the sort-free plan must win")
	}
}
