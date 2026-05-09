package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughDistinctRule pushes a LogicalSort's ordering
// through a LogicalDistinctExpression. Distinct (duplicate elimination)
// preserves row order when its input is sorted, so sorting below the
// distinct produces the same result as sorting above.
//
//	Sort([k1 ASC], Distinct(X))
//	  → Distinct(Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// distinct case).
type PushOrderingThroughDistinctRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughDistinctRule() *PushOrderingThroughDistinctRule {
	return &PushOrderingThroughDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_distinct"),
	}
}

func (r *PushOrderingThroughDistinctRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughDistinctRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	distinct, ok := innerExpr.(*expressions.LogicalDistinctExpression)
	if !ok {
		return
	}

	// Build: Distinct(Sort(keys, distinctChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), distinct.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newDistinct := expressions.NewLogicalDistinctExpression(pushedSortQ)
	call.Yield(newDistinct)
}

var _ ExpressionRule = (*PushOrderingThroughDistinctRule)(nil)
