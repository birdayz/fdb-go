package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		projPlan := plans.NewRecordQueryProjectionPlanWithAliases(
			proj.GetProjectedValues(), proj.GetAliases(), ph.GetRecordQueryPlan())
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		call.YieldFinalExpression(newPhysicalProjectionFinalWrapper(projPlan, innerQ))
	}
}

func newPhysicalProjectionFinalWrapper(plan *plans.RecordQueryProjectionPlan, innerQuant expressions.Quantifier) *physicalProjectionWrapper {
	return NewPhysicalProjectionWrapper(plan, innerQuant)
}

var _ ImplementationRule = (*ImplementProjectionFinalRule)(nil)
