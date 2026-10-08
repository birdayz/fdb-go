package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// real match. Each leg therefore ranges over EVERY stored-record member of its
// group that satisfies the comparison-key ordering, each with its ordering
// spine pinned, in one fresh final reference, and the rule declines when any
// leg has none. This is Java's ImplementIntersectionRule, which ranges each
// leg over its rolled-up stored-record partition
// (memoizeMemberPlansFromOther): no member is pre-selected, OptimizeGroup
// chooses (RFC-257 WS-F F-8). Java's legs are ordered by construction (the
// data-access rule memoizes them so); Go's filter and pins state it.
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

	legs := make([][]expressions.RelationalExpression, 0, len(children))
	var rowType values.Type
	for _, q := range children {
		var leg []expressions.RelationalExpression
		for _, candidate := range storedRecordDMLCandidates(q.GetRangesOver()) {
			if !memberSatisfiesOrdering(candidate.expr, requested) {
				continue
			}
			pinned := pinOrderedSpine(candidate.expr, requested, call.CostModel())
			physical, ok := pinned.(physicalPlanExpression)
			if !ok || physical.GetRecordQueryPlan() == nil {
				continue
			}
			// The comparison keys are baked against one row type, so every
			// member of every leg must present it.
			resultType := physical.GetRecordQueryPlan().GetResultType()
			if rowType == nil {
				rowType = resultType
			} else if !rowType.Equals(resultType) {
				continue
			}
			leg = append(leg, pinned)
		}
		if len(leg) == 0 {
			return
		}
		legs = append(legs, leg)
	}
	childPlans := make([]plans.RecordQueryPlan, len(legs))
	for i, leg := range legs {
		childPlans[i] = leg[0].(physicalPlanExpression).GetRecordQueryPlan()
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
	// Each ordering proof is tied to the exact executable spines pinned above.
	// A fresh final reference holding only them prevents a later generic child
	// relink from swapping in an unordered sibling.
	childQs := make([]expressions.Quantifier, 0, len(legs))
	for _, leg := range legs {
		legRef := call.MemoizeFinalExpression(leg[0])
		for _, member := range leg[1:] {
			legRef.InsertFinal(member)
		}
		childQs = append(childQs, expressions.NewPhysicalQuantifier(legRef))
	}

	intersection, err := plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(
		childQs,
		comparisonParts,
		false,
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(intersection)
}

var _ ExpressionRule = (*ImplementIntersectionRule)(nil)
