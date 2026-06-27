package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// DistinctOverGroupByElimRule eliminates a LogicalDistinct that sits
// directly over a GroupByExpression. GROUP BY already produces at most
// one row per unique combination of grouping keys — DISTINCT on top
// is a no-op.
//
//	Distinct(GroupBy(keys, aggs, X))  →  GroupBy(keys, aggs, X)
//
// Java equivalent: part of RemoveRedundantDistinctRule family.
type DistinctOverGroupByElimRule struct {
	matcher matching.BindingMatcher
}

func NewDistinctOverGroupByElimRule() *DistinctOverGroupByElimRule {
	return &DistinctOverGroupByElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("distinct_over_groupby"),
	}
}

func (r *DistinctOverGroupByElimRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *DistinctOverGroupByElimRule) OnMatch(call *ExpressionRuleCall) {
	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerRef := d.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	innerExpr := innerRef.Get()
	if _, ok := innerExpr.(*expressions.GroupByExpression); !ok {
		return
	}
	call.Yield(innerExpr)
}

var _ ExpressionRule = (*DistinctOverGroupByElimRule)(nil)
