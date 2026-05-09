package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughFilterRule pushes a LogicalSort's ordering
// through a LogicalFilterExpression. Filter only removes rows — it
// does not reorder them — so sorting below the filter produces the
// same result as sorting above.
//
//	Sort([k1 ASC], Filter(pred, X))
//	  → Filter(pred, Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// filter case).
type PushOrderingThroughFilterRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughFilterRule() *PushOrderingThroughFilterRule {
	return &PushOrderingThroughFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_filter"),
	}
}

func (r *PushOrderingThroughFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughFilterRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	filter, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		return
	}

	// Build: Filter(preds, Sort(keys, filterChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), filter.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newFilter := expressions.NewLogicalFilterExpression(filter.GetPredicates(), pushedSortQ)
	call.Yield(newFilter)
}

var _ ExpressionRule = (*PushOrderingThroughFilterRule)(nil)
