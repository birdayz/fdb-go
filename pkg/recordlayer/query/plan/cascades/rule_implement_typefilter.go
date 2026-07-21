package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
// covering-index partitions); Go always emits the simple
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
	winner := getWinnerForOrdering(innerRef, properties.PreserveOrdering(), call.CostModel())
	if winner == nil {
		return
	}
	if _, ok := winner.(physicalPlanExpression); !ok {
		return
	}
	// The type filter is its own cascades expression now (RFC-184 W2) — it carries
	// the live child edge directly, no physicalTypeFilterWrapper.
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
	tfPlan := plans.NewRecordQueryTypeFilterPlanFromQuantifier(tf.GetRecordTypes(), innerQ)
	call.Yield(tfPlan)
}

var _ ExpressionRule = (*ImplementTypeFilterRule)(nil)
