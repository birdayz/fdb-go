package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctFinalRule is the PLANNING-phase counterpart of
// ImplementDistinctRule. It fires after the inner has been finalized
// (children are physical plans from ImplementationRules like
// ImplementInMemorySortRule), producing a physical distinct wrapper.
//
// Without this rule, DISTINCT + ORDER BY fails: ImplementDistinctRule
// fires during EXPLORE when the sort hasn't been physically implemented
// yet (ImplementInMemorySortRule runs during PLANNING), so Distinct
// never finds a physical inner to wrap.
type ImplementDistinctFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctFinalRule() *ImplementDistinctFinalRule {
	return &ImplementDistinctFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct_final"),
	}
}

func (r *ImplementDistinctFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctFinalRule) OnMatch(call *ImplementationRuleCall) {
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	qs := d.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	for _, m := range innerRef.FinalMembers() {
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		distPlan := plans.NewRecordQueryDistinctPlan(ph.GetRecordQueryPlan())
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		call.YieldFinalExpression(NewPhysicalDistinctWrapper(distPlan, innerQ))
		return
	}
}

var _ ImplementationRule = (*ImplementDistinctFinalRule)(nil)
