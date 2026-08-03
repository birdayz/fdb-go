package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// DistinctOverGroupByElimRule eliminates a LogicalDistinct that sits
// directly over a GroupByExpression when the grouping key's physical identity
// is congruent with logical DISTINCT equality. GROUP BY's streaming/runtime and
// maintained aggregate indexes preserve raw FLOAT/DOUBLE NaN encodings, while
// DISTINCT canonicalizes NaNs, so floating (including nested floating) or
// unknown keys must keep the outer DISTINCT.
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
	groupBy, ok := innerExpr.(*expressions.GroupByExpression)
	if !ok {
		return
	}
	for _, key := range groupBy.GetGroupingKeys() {
		if key == nil || !properties.ValueIdentityMatchesLogicalEquality(key.Type()) {
			return
		}
	}
	call.Yield(innerExpr)
}

var _ ExpressionRule = (*DistinctOverGroupByElimRule)(nil)
