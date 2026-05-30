package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementTypeFilterRule implements a logical LogicalTypeFilterExpression
// as a physical RecordQueryTypeFilterPlan, gated on the inner Reference
// having at least one physical-plan member.
//
//	TypeFilter([T1, T2], inner-with-physical-member)
//	  →  TypeFilterPlan([T1, T2], inner-physical)
//
// Same gating pattern as Implement{Filter,Sort,Distinct}.
//
// Java's ImplementTypeFilterRule consults PlanPartition properties
// to filter only over partitions producing stored records (not
// covering-index partitions); the seed always emits the simple
// type-filter.
type ImplementTypeFilterRule struct {
	matcher matching.BindingMatcher
}

// NewImplementTypeFilterRule constructs the rule.
func NewImplementTypeFilterRule() *ImplementTypeFilterRule {
	return &ImplementTypeFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalTypeFilterExpression]("logical_type_filter"),
	}
}

// Matcher returns the pattern.
func (r *ImplementTypeFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalTypeFilterExpression with a
// physical inner.
func (r *ImplementTypeFilterRule) OnMatch(call *ExpressionRuleCall) {
	tf := matching.Get[*expressions.LogicalTypeFilterExpression](call.Bindings, r.matcher)
	innerRef := tf.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	winner := getWinnerForOrdering(innerRef, PreserveOrdering(), call.CostModel())
	if winner == nil {
		return
	}
	ph, ok := winner.(physicalPlanExpression)
	if !ok {
		return
	}
	tfPlan := plans.NewRecordQueryTypeFilterPlan(tf.GetRecordTypes(), ph.GetRecordQueryPlan())
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	call.Yield(NewPhysicalTypeFilterWrapper(tfPlan, innerQ))
}

var _ ExpressionRule = (*ImplementTypeFilterRule)(nil)
