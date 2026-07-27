package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RFC-190 190.2: the three structural DEPTH tiebreaks (type-filter / fetch /
// distinct) and the map/filter-node-count tiebreak used to be gated to
// ABSTAIN when either side carried an in-memory sort. That gate made a gated
// rung decide a sort-free pair outright while a LATER, ungated rung (fetch
// count, inJoinCount, nljPredicateCount) decided any pair where one side had
// a sort — two different rungs governing "the same slot" depending on sort
// presence, which is exactly how a non-transitive 3-cycle arises
// (cost_transitivity_test.go pins the regression this produced).
//
// The fix has TWO parts, both required together:
//
//  1. concretePlanDepth is made SORT-INVARIANT (an InMemorySort is
//     transparent — it adds no depth level), matching Java's
//     ExpressionDepthProperty/UnmatchedFieldsCountProperty/countSimpleOps,
//     none of which gate on anything sort-related (Java has no in-memory
//     sort node at all; RemoveSortRule eliminates it structurally before
//     planning). The three depth rungs and the map/filter-count rung are
//     now UNGATED.
//  2. The inMemorySortCount rung is PROMOTED to fire immediately after
//     comparePrimaryScanVsIndexScan — BEFORE any of the now-ungated
//     structural rungs — so a redundant sort is rejected before a
//     sort-blind structural criterion ever gets a chance to prefer the
//     sorted candidate on an unrelated axis. This is the cost-time analog
//     of Java's RemoveSortRule: by the time any structural rung below runs,
//     opsA.inMemorySortCount == opsB.inMemorySortCount is GUARANTEED.
//
// These tests prove both halves: each rung fires the same way between two
// sort-free plans; adding a sort to ONLY the winning side flips the decision
// to the sort-free plan (via the promoted rung, not the structural one); and
// adding the SAME sort to BOTH sides re-ties sort count and falls through to
// the (sort-invariant) structural rung, which decides exactly like the
// sort-free case.

func scanT() plans.RecordQueryPlan {
	return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
}

func indexT() plans.RecordQueryPlan {
	return plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false)
}

// TestExpressionDepth_LogicalFallbackSortTransparent pins the logical memo
// fallback used while a parent is not physical yet. The physical path was
// already sort-invariant, but the fallback descends through physical child
// members and could therefore encounter an InMemorySort too. A sort must not
// add a structural depth level there either.
func TestExpressionDepth_LogicalFallbackSortTransparent(t *testing.T) {
	t.Parallel()

	fetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexT(), nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	logicalOver := func(child plans.RecordQueryPlan) expressions.RelationalExpression {
		return expressions.NewLogicalUnionExpression([]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.FinalOf(child)),
		})
	}

	plainDepth := costExprDepth(logicalOver(fetch), matchFetch)
	sortedDepth := costExprDepth(
		logicalOver(plans.NewRecordQueryInMemorySortPlan(fetch, nil)),
		matchFetch)
	if plainDepth != 1 {
		t.Fatalf("logical fallback fetch depth without sort = %d, want 1", plainDepth)
	}
	if sortedDepth != plainDepth {
		t.Fatalf("logical fallback fetch depth with transparent sort = %d, want %d", sortedDepth, plainDepth)
	}
}

// TestDepthRung_TypeFilterDepth_SortInvariant — the type-filter-depth rung
// prefers the DEEPER (pushed-below-the-limit) type filter between two
// sort-free plans. A sort added to ONLY the deeper side flips the winner to
// the sort-free shallower plan (promoted inMemorySortCount rung). The SAME
// sort added to BOTH sides re-ties sort count, and the deeper side wins
// again — sort-invariant depth measurement.
func TestDepthRung_TypeFilterDepth_SortInvariant(t *testing.T) {
	t.Parallel()
	// Same node multiset (1 limit, 1 type filter, 1 scan) — only the type-filter
	// depth differs. Deeper: Limit(TypeFilter(Scan)); shallower: TypeFilter(Limit(Scan)).
	deeper := plans.NewRecordQueryLimitPlan(plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scanT()), 10, 0)
	shallower := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, plans.NewRecordQueryLimitPlan(scanT(), 10, 0))

	if !PlanningCostModelLess(deeper, shallower) {
		t.Error("type-filter-depth rung must fire between sort-free plans: deeper type filter wins")
	}
	if PlanningCostModelLess(shallower, deeper) {
		t.Error("type-filter-depth rung asymmetry broken")
	}

	// Promoted sort-count rung: a redundant sort added to ONLY the
	// (structurally) winning deeper side makes the SORT-FREE shallower plan
	// win instead — the structural rung never gets a chance to fire.
	deeperSorted := plans.NewRecordQueryInMemorySortPlan(deeper, nil)
	if !PlanningCostModelLess(shallower, deeperSorted) {
		t.Error("promoted inMemorySortCount rung must reject the redundant sort before the type-filter-depth rung can fire")
	}
	if PlanningCostModelLess(deeperSorted, shallower) {
		t.Error("inMemorySortCount rung asymmetry broken")
	}

	// Sort-invariance: the SAME sort added to BOTH sides re-ties sort count,
	// and the (now sort-invariant) depth rung decides exactly like the
	// sort-free case — the deeper type filter wins again.
	shallowerSorted := plans.NewRecordQueryInMemorySortPlan(shallower, nil)
	if !PlanningCostModelLess(deeperSorted, shallowerSorted) {
		t.Error("type-filter-depth rung must be sort-invariant once both sides tie on sort count: the deeper type filter must win")
	}
	if PlanningCostModelLess(shallowerSorted, deeperSorted) {
		t.Error("type-filter-depth rung asymmetry broken with a sort on both sides")
	}
}

// TestDepthRung_FetchDepth_SortInvariant — the fetch-depth rung prefers the
// SHALLOWER (later, near-root) fetch, sort-free and once both sides tie on
// sort count. A sort added to ONLY the winning shallower side flips the
// decision to the sort-free deeper plan (promoted rung). Both sides carry
// an index scan so the fetch block runs.
func TestDepthRung_FetchDepth_SortInvariant(t *testing.T) {
	t.Parallel()
	fetch := func(inner plans.RecordQueryPlan) plans.RecordQueryPlan {
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	}
	// Same node multiset (1 limit, 1 fetch, 1 index) — only the fetch depth differs.
	shallower := fetch(plans.NewRecordQueryLimitPlan(indexT(), 10, 0)) // Fetch(Limit(Index)) — fetchDepth 0
	deeper := plans.NewRecordQueryLimitPlan(fetch(indexT()), 10, 0)    // Limit(Fetch(Index)) — fetchDepth 1

	if !PlanningCostModelLess(shallower, deeper) {
		t.Error("fetch-depth rung must fire between sort-free plans: shallower fetch wins")
	}
	if PlanningCostModelLess(deeper, shallower) {
		t.Error("fetch-depth rung asymmetry broken")
	}

	// Promoted sort-count rung: a redundant sort added to ONLY the winning
	// shallower side makes the sort-free deeper plan win instead.
	shallowerSorted := plans.NewRecordQueryInMemorySortPlan(shallower, nil)
	if !PlanningCostModelLess(deeper, shallowerSorted) {
		t.Error("promoted inMemorySortCount rung must reject the redundant sort before the fetch-depth rung can fire")
	}
	if PlanningCostModelLess(shallowerSorted, deeper) {
		t.Error("inMemorySortCount rung asymmetry broken")
	}

	// Sort-invariance: the SAME sort on both sides re-ties sort count, and
	// the shallower fetch wins again.
	deeperSorted := plans.NewRecordQueryInMemorySortPlan(deeper, nil)
	if !PlanningCostModelLess(shallowerSorted, deeperSorted) {
		t.Error("fetch-depth rung must be sort-invariant once both sides tie on sort count: the shallower fetch must win")
	}
	if PlanningCostModelLess(deeperSorted, shallowerSorted) {
		t.Error("fetch-depth rung asymmetry broken with a sort on both sides")
	}
}

// TestStructuralRung_MapFilterCount_SortInvariant — the map/filter-node-count
// rung (NON-depth) prefers FEWER filter nodes, sort-free and once both sides
// tie on sort count. A sort added to ONLY the winning fewer-node side flips
// the decision to the sort-free plan with more nodes (promoted rung).
func TestStructuralRung_MapFilterCount_SortInvariant(t *testing.T) {
	t.Parallel()
	pred := func(field string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			&values.FieldValue{Field: field, Typ: values.NullableLong},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	}
	// Same total residual (2 preds) but different filter-NODE count: one node vs a
	// pushdown-split two nodes. Only the map/filter count differs.
	oneNode := plans.NewRecordQueryPredicatesFilterPlan(scanT(), []predicates.QueryPredicate{pred("a"), pred("b")})
	twoNodes := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryPredicatesFilterPlan(scanT(), []predicates.QueryPredicate{pred("a")}),
		[]predicates.QueryPredicate{pred("b")})

	if !PlanningCostModelLess(oneNode, twoNodes) {
		t.Error("map/filter-count rung must fire between sort-free plans: fewer filter nodes wins")
	}
	if PlanningCostModelLess(twoNodes, oneNode) {
		t.Error("map/filter-count rung asymmetry broken")
	}

	// Promoted sort-count rung: a redundant sort added to ONLY the winning
	// fewer-node side makes the sort-free two-node plan win instead.
	oneNodeSorted := plans.NewRecordQueryInMemorySortPlan(oneNode, nil)
	if !PlanningCostModelLess(twoNodes, oneNodeSorted) {
		t.Error("promoted inMemorySortCount rung must reject the redundant sort before the map/filter-count rung can fire")
	}
	if PlanningCostModelLess(oneNodeSorted, twoNodes) {
		t.Error("inMemorySortCount rung asymmetry broken")
	}

	// Sort-invariance: the SAME sort on both sides re-ties sort count, and
	// the fewer-node plan wins again.
	twoNodesSorted := plans.NewRecordQueryInMemorySortPlan(twoNodes, nil)
	if !PlanningCostModelLess(oneNodeSorted, twoNodesSorted) {
		t.Error("map/filter-count rung must be sort-invariant once both sides tie on sort count: fewer filter nodes must win")
	}
	if PlanningCostModelLess(twoNodesSorted, oneNodeSorted) {
		t.Error("map/filter-count rung asymmetry broken with a sort on both sides")
	}
}

// TestDepthRung_DistinctDepth_SortInvariant — the distinct-depth rung
// prefers the DEEPER distinct, sort-free and once both sides tie on sort
// count. A sort added to ONLY the winning deeper side flips the decision to
// the sort-free shallower plan (promoted rung).
func TestDepthRung_DistinctDepth_SortInvariant(t *testing.T) {
	t.Parallel()
	// Same node multiset (1 limit, 1 distinct, 1 scan) — only the distinct depth differs.
	deeper := plans.NewRecordQueryLimitPlan(plans.NewRecordQueryDistinctPlan(scanT()), 10, 0)
	shallower := plans.NewRecordQueryDistinctPlan(plans.NewRecordQueryLimitPlan(scanT(), 10, 0))

	if !PlanningCostModelLess(deeper, shallower) {
		t.Error("distinct-depth rung must fire between sort-free plans: deeper distinct wins")
	}
	if PlanningCostModelLess(shallower, deeper) {
		t.Error("distinct-depth rung asymmetry broken")
	}

	// Promoted sort-count rung: a redundant sort added to ONLY the winning
	// deeper side makes the sort-free shallower plan win instead.
	deeperSorted := plans.NewRecordQueryInMemorySortPlan(deeper, nil)
	if !PlanningCostModelLess(shallower, deeperSorted) {
		t.Error("promoted inMemorySortCount rung must reject the redundant sort before the distinct-depth rung can fire")
	}
	if PlanningCostModelLess(deeperSorted, shallower) {
		t.Error("inMemorySortCount rung asymmetry broken")
	}

	// Sort-invariance: the SAME sort on both sides re-ties sort count, and
	// the deeper distinct wins again.
	shallowerSorted := plans.NewRecordQueryInMemorySortPlan(shallower, nil)
	if !PlanningCostModelLess(deeperSorted, shallowerSorted) {
		t.Error("distinct-depth rung must be sort-invariant once both sides tie on sort count: the deeper distinct must win")
	}
	if PlanningCostModelLess(shallowerSorted, deeperSorted) {
		t.Error("distinct-depth rung asymmetry broken with a sort on both sides")
	}
}
