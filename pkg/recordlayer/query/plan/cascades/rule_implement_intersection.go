package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementIntersectionRule implements a logical
// LogicalIntersectionExpression as a physical
// RecordQueryIntersectionPlan, gated on EVERY child Reference
// having at least one physical-plan member.
//
//	Intersection(child0-with-physical, child1-with-physical, ...)
//	  →  IntersectionPlan(child0-physical, child1-physical, ...)
//
// Per-child gating: same as ImplementUnionRule — partial physical-
// implementation produces an invalid mixed-hierarchy plan tree.
//
// The comparisonKeyValues from the logical Intersection carry
// through unchanged into the physical plan — the row-equality key
// is independent of which physical operator emits the rows.
//
// Java has multiple Intersection variants (ordered, unordered,
// primary-key-based, value-based); the seed always emits the
// generic RecordQueryIntersectionPlan. Specialised flavors land
// when their consumers do.
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
// with at least one physical-plan member.
func (r *ImplementIntersectionRule) OnMatch(call *ExpressionRuleCall) {
	intr := matching.Get[*expressions.LogicalIntersectionExpression](call.Bindings, r.matcher)
	children := intr.GetQuantifiers()
	if len(children) == 0 {
		return
	}

	innerPlans := make([]plans.RecordQueryPlan, 0, len(children))
	winners := make([]expressions.RelationalExpression, 0, len(children))
	for _, q := range children {
		innerRef := q.GetRangesOver()
		if innerRef == nil {
			return
		}
		winner := getWinnerForOrdering(innerRef, PreserveOrdering())
		if winner == nil {
			return // any child not physical → skip the whole rule fire
		}
		ph, ok := winner.(physicalPlanExpression)
		if !ok {
			return
		}
		innerPlans = append(innerPlans, ph.GetRecordQueryPlan())
		winners = append(winners, winner)
	}

	intersectionPlan := plans.NewRecordQueryIntersectionPlan(innerPlans, intr.GetComparisonKeyValues())

	// Reuse the existing physical wrapper expressions from each child
	// Reference rather than re-wrapping from scratch.
	childQs := make([]expressions.Quantifier, 0, len(winners))
	for _, winner := range winners {
		childQs = append(childQs, expressions.ForEachQuantifier(call.MemoizeExpression(winner)))
	}

	call.Yield(NewPhysicalIntersectionWrapper(intersectionPlan, childQs))
}

var _ ExpressionRule = (*ImplementIntersectionRule)(nil)
