package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughUpdateRule pushes a LogicalSort's ordering
// through an UpdateExpression. Update passes through rows unchanged
// (it applies transforms and emits them for downstream
// counting/projection), so sorting below the update produces the
// same result as sorting above.
//
//	Sort([k1 ASC], Update(target, transforms, X))
//	  → Update(target, transforms, Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughUpdateRule.
type PushOrderingThroughUpdateRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughUpdateRule() *PushOrderingThroughUpdateRule {
	return &PushOrderingThroughUpdateRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_update"),
	}
}

func (r *PushOrderingThroughUpdateRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughUpdateRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	upd, ok := innerExpr.(*expressions.UpdateExpression)
	if !ok {
		return
	}

	// Build: Update(target, transforms, Sort(keys, updateChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), upd.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newUpd := expressions.NewUpdateExpression(pushedSortQ, upd.GetTargetRecordType(), upd.GetTransforms())
	call.Yield(newUpd)
}

var _ ExpressionRule = (*PushOrderingThroughUpdateRule)(nil)
