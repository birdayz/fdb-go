package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughDeleteRule pushes a LogicalSort's ordering
// through a DeleteExpression. Delete passes through rows unchanged
// (it only removes them from the store), so sorting below the delete
// produces the same result as sorting above.
//
//	Sort([k1 ASC], Delete(target, X))
//	  → Delete(target, Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughDeleteRule.
type PushOrderingThroughDeleteRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughDeleteRule() *PushOrderingThroughDeleteRule {
	return &PushOrderingThroughDeleteRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_delete"),
	}
}

func (r *PushOrderingThroughDeleteRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughDeleteRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	del, ok := innerExpr.(*expressions.DeleteExpression)
	if !ok {
		return
	}

	// Build: Delete(target, Sort(keys, deleteChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), del.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newDel := expressions.NewDeleteExpression(pushedSortQ, del.GetTargetRecordType())
	call.Yield(newDel)
}

var _ ExpressionRule = (*PushOrderingThroughDeleteRule)(nil)
