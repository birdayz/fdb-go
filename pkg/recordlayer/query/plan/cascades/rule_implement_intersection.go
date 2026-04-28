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
	for _, q := range children {
		innerRef := q.GetRangesOver()
		if innerRef == nil {
			return
		}
		var innerPlan plans.RecordQueryPlan
		for _, m := range innerRef.Members() {
			switch w := m.(type) {
			case *physicalScanWrapper:
				innerPlan = w.GetPlan()
			case *physicalFilterWrapper:
				innerPlan = w.GetPlan()
			case *physicalSortWrapper:
				innerPlan = w.GetPlan()
			case *physicalDistinctWrapper:
				innerPlan = w.GetPlan()
			case *physicalTypeFilterWrapper:
				innerPlan = w.GetPlan()
			}
			if innerPlan != nil {
				break
			}
		}
		if innerPlan == nil {
			return // any child not physical → skip the whole rule fire
		}
		innerPlans = append(innerPlans, innerPlan)
	}

	intersectionPlan := plans.NewRecordQueryIntersectionPlan(innerPlans, intr.GetComparisonKeyValues())

	// Build wrapped child quantifiers — one per physical inner.
	childQs := make([]expressions.Quantifier, 0, len(innerPlans))
	for _, ip := range innerPlans {
		wrap := wrapPhysicalPlan(ip)
		if wrap == nil {
			return
		}
		childQs = append(childQs, expressions.ForEachQuantifier(expressions.InitialOf(wrap)))
	}

	call.Yield(NewPhysicalIntersectionWrapper(intersectionPlan, childQs))
}

var _ ExpressionRule = (*ImplementIntersectionRule)(nil)
