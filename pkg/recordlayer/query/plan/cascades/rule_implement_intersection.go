package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementIntersectionRule implements a logical
// LogicalIntersectionExpression as a physical
// RecordQueryIntersectionPlan, gated on EVERY child Reference
// having an ascending physical-plan member for the comparison key.
//
//	Intersection(child0-ordered-by-key, child1-ordered-by-key, ...)
//	  →  IntersectionPlan(child0-ordered-by-key, child1-ordered-by-key, ...)
//
// RecordQueryIntersectionPlan is a sorted merge: merely finding an arbitrary
// physical member for each leg is not sufficient. A leg that does not emit the
// comparison key monotonically can make the merge permanently advance past a
// real match. This rule therefore pins one ordering-satisfying physical spine
// per child and declines when any leg cannot provide it.
//
// LogicalIntersectionExpression currently carries comparison values but no
// direction, so this implementation is the natural forward/ascending variant.
// The data-access intersector builds directional/reverse variants directly.
//
// Java has multiple Intersection variants (ordered, unordered,
// primary-key-based, value-based); this rule always emits the
// generic RecordQueryIntersectionPlan (the primary-key-keyed
// cross-candidate intersection is built separately by the
// data-access path — WithPrimaryKeyIntersector, planner.go).
type ImplementIntersectionRule struct {
	matcher matching.BindingMatcher
}

// NewImplementIntersectionRule constructs the rule.
func NewImplementIntersectionRule() *ImplementIntersectionRule {
	return &ImplementIntersectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalIntersectionExpression]("logical_intersection"),
	}
}

// Matcher returns the pattern.
func (r *ImplementIntersectionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when EVERY child Quantifier ranges over a Reference
// with an ascending physical-plan member for the comparison key.
func (r *ImplementIntersectionRule) OnMatch(call *ExpressionRuleCall) {
	intr := matching.Get[*expressions.LogicalIntersectionExpression](call.Bindings, r.matcher)
	children := intr.GetQuantifiers()
	comparisonKeyValues := intr.GetComparisonKeyValues()
	if len(children) == 0 || len(comparisonKeyValues) == 0 {
		return
	}

	requestedParts := make([]properties.RequestedOrderingPart, len(comparisonKeyValues))
	for i, value := range comparisonKeyValues {
		requestedParts[i] = properties.RequestedOrderingPart{
			Value:     value,
			SortOrder: properties.RequestedSortOrderAscending,
		}
	}
	requested := properties.NewRequestedOrdering(
		requestedParts,
		properties.DistinctnessPreserveDistinctness,
		false,
	)

	winners := make([]expressions.RelationalExpression, 0, len(children))
	childPlans := make([]plans.RecordQueryPlan, 0, len(children))
	for _, q := range children {
		innerRef := q.GetRangesOver()
		if innerRef == nil {
			return
		}
		winner, satisfied := getWinnerForOrdering(innerRef, requested, call.CostModel())
		if winner == nil || !satisfied {
			return
		}
		pinned := pinOrderedSpine(winner, requested, call.CostModel())
		if pinned == nil {
			return
		}
		physical, ok := pinned.(physicalPlanExpression)
		if !ok || physical.GetRecordQueryPlan() == nil {
			return
		}
		winners = append(winners, pinned)
		childPlans = append(childPlans, physical.GetRecordQueryPlan())
	}

	bakedComparisonKeys := bakedIntersectionKeys(comparisonKeyValues, childPlans)
	if len(bakedComparisonKeys) != len(comparisonKeyValues) {
		return
	}
	comparisonParts := make([]properties.ProvidedOrderingPart, len(bakedComparisonKeys))
	for i, value := range bakedComparisonKeys {
		comparisonParts[i] = properties.ProvidedOrderingPart{
			Value:     value,
			SortOrder: properties.ProvidedSortOrderAscending,
		}
	}

	// Each ordering proof is tied to the exact executable spine selected above.
	// A final singleton reference prevents a later generic child relink from
	// swapping in an unordered sibling after the merge has dropped its sort.
	childQs := make([]expressions.Quantifier, 0, len(winners))
	for _, winner := range winners {
		childQs = append(childQs, expressions.NewPhysicalQuantifier(
			call.MemoizeFinalExpression(winner),
		))
	}

	intersection := plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(
		childQs,
		comparisonParts,
		false,
	)
	if intersection != nil {
		call.Yield(intersection)
	}
}

var _ ExpressionRule = (*ImplementIntersectionRule)(nil)
