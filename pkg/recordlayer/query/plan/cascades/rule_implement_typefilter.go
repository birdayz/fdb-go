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
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	tfPlan := plans.NewRecordQueryTypeFilterPlan(tf.GetRecordTypes(), innerPlan)

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(NewPhysicalTypeFilterWrapper(tfPlan, innerQ))
}

var _ ExpressionRule = (*ImplementTypeFilterRule)(nil)
