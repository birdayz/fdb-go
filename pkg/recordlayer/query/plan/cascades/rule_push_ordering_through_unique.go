package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughUniqueRule pushes a LogicalSort's ordering
// through a LogicalUniqueExpression. Unique (PK deduplication)
// preserves row order — it only drops duplicate-PK rows without
// reordering — so sorting below unique produces the same final order
// as sorting above.
//
//	Sort([k1 ASC], Unique(X))
//	  → Unique(Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughUniqueRule.
type PushOrderingThroughUniqueRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughUniqueRule() *PushOrderingThroughUniqueRule {
	return &PushOrderingThroughUniqueRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_unique"),
	}
}

func (r *PushOrderingThroughUniqueRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughUniqueRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	unique, ok := innerExpr.(*expressions.LogicalUniqueExpression)
	if !ok {
		return
	}

	// Build: Unique(Sort(keys, uniqueChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), unique.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newUnique := expressions.NewLogicalUniqueExpression(pushedSortQ)
	call.Yield(newUnique)
}

var _ ExpressionRule = (*PushOrderingThroughUniqueRule)(nil)
