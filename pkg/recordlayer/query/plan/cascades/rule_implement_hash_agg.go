package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementHashAggregationRule implements a GroupByExpression as a
// physical RecordQueryHashAggregationPlan unconditionally — the hash
// aggregate doesn't require sorted input. The cost model ensures
// streaming aggregation wins when the inner IS ordered (streaming
// agg's CPU is lower because it avoids hashing), so both rules can
// coexist — the cheaper plan wins during cost-driven extraction.
//
//	GroupBy(keys, aggs, inner)  →  HashAggPlan(inner-physical)
//
// Java equivalent: the hash-aggregate implementation path from
// ImplementStreamingAggregationRule when no ordering satisfaction is
// found. The seed splits this into two distinct rules for clarity.
type ImplementHashAggregationRule struct {
	matcher matching.BindingMatcher
}

func NewImplementHashAggregationRule() *ImplementHashAggregationRule {
	return &ImplementHashAggregationRule{
		matcher: NewExpressionMatcher[*expressions.GroupByExpression]("group_by_hash"),
	}
}

func (r *ImplementHashAggregationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementHashAggregationRule) OnMatch(call *ExpressionRuleCall) {
	gb := matching.Get[*expressions.GroupByExpression](call.Bindings, r.matcher)

	innerRef := gb.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}

	aggPlan := plans.NewRecordQueryHashAggregationPlan(
		innerPlan, gb.GetGroupingKeys(), gb.GetAggregates(),
	)
	innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerExpr))
	call.Yield(newPhysicalHashAggWrapper(aggPlan, innerQ))
}

var _ ExpressionRule = (*ImplementHashAggregationRule)(nil)
