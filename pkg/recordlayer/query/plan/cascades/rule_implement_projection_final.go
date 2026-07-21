package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementProjectionFinalRule is the PLANNING-phase counterpart of
// ImplementProjectionRule. It fires after the inner has been finalized
// (children are physical plans), producing a physical projection wrapper.
type ImplementProjectionFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementProjectionFinalRule() *ImplementProjectionFinalRule {
	return &ImplementProjectionFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection_final"),
	}
}

func (r *ImplementProjectionFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementProjectionFinalRule) OnMatch(call *ImplementationRuleCall) {
	proj := call.Bindings.Get(r.matcher).(*expressions.LogicalProjectionExpression)
	qs := proj.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	for _, m := range innerRef.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		// The projection is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2).
		call.YieldFinalExpression(plans.NewRecordQueryProjectionPlanFromQuantifier(
			proj.GetProjectedValues(), proj.GetAliases(), innerQ))
	}
}

var _ ImplementationRule = (*ImplementProjectionFinalRule)(nil)
