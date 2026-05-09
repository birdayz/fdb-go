package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughInsertRule pushes a LogicalSort's ordering
// through an InsertExpression. Insert passes through rows unchanged
// (it writes them to the store and emits them for downstream
// counting/projection), so sorting below the insert produces the
// same result as sorting above.
//
//	Sort([k1 ASC], Insert(target, X))
//	  → Insert(target, Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughInsertRule.
type PushOrderingThroughInsertRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughInsertRule() *PushOrderingThroughInsertRule {
	return &PushOrderingThroughInsertRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_insert"),
	}
}

func (r *PushOrderingThroughInsertRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughInsertRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	ins, ok := innerExpr.(*expressions.InsertExpression)
	if !ok {
		return
	}

	// Build: Insert(target, Sort(keys, insertChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), ins.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newIns := expressions.NewInsertExpression(pushedSortQ, ins.GetTargetRecordType(), ins.GetTargetType())
	call.Yield(newIns)
}

var _ ExpressionRule = (*PushOrderingThroughInsertRule)(nil)
