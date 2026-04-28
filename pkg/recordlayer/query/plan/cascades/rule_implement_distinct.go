package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctRule implements a logical LogicalDistinctExpression
// as a physical RecordQueryDistinctPlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	Distinct(inner-with-physical-member)
//	  →  DistinctPlan(inner-physical)
//
// Same gating pattern as ImplementFilterRule / ImplementSortRule.
//
// Java's ImplementDistinctRule consults PlanPartition properties to
// pick the right distinct-plan flavor (ordered vs unordered, by-key
// vs by-row); the seed always emits the unordered-by-row plan.
type ImplementDistinctRule struct {
	matcher matching.BindingMatcher
}

// NewImplementDistinctRule constructs the rule.
func NewImplementDistinctRule() *ImplementDistinctRule {
	return &ImplementDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct"),
	}
}

// Matcher returns the pattern.
func (r *ImplementDistinctRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalDistinctExpression with a physical
// inner.
func (r *ImplementDistinctRule) OnMatch(call *ExpressionRuleCall) {
	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerRef := d.GetInner().GetRangesOver()
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
		}
		if innerPlan != nil {
			break
		}
	}
	if innerPlan == nil {
		return
	}

	distPlan := plans.NewRecordQueryDistinctPlan(innerPlan)

	innerWrap := wrapPhysicalPlan(innerPlan)
	if innerWrap == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerWrap))
	call.Yield(NewPhysicalDistinctWrapper(distPlan, innerQ))
}

var _ ExpressionRule = (*ImplementDistinctRule)(nil)
