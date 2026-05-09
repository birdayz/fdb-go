package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughUnionRule pushes a LogicalSort's ordering into
// each branch of a LogicalUnionExpression. Each union branch gets
// its own copy of the sort so downstream rules (SortOverOrderedElim,
// ImplementInMemorySortRule) can independently satisfy or eliminate
// the ordering per branch.
//
//	Sort([k1 ASC], Union(A, B, ...))
//	  → Union(Sort([k1 ASC], A), Sort([k1 ASC], B), ...)
//
// This does NOT produce a globally sorted result on its own — a
// merge-sort union physical plan is needed above (handled by
// ImplementDistinctUnionRule / ImplementUnionRule when they detect
// ordered branches). The rule's job is purely to push the ordering
// requirement down so that each branch can be independently ordered.
//
// Ports Java's PushRequestedOrderingThroughUnionRule.
type PushOrderingThroughUnionRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughUnionRule() *PushOrderingThroughUnionRule {
	return &PushOrderingThroughUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_union"),
	}
}

func (r *PushOrderingThroughUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughUnionRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	union, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		return
	}

	children := union.GetQuantifiers()
	if len(children) == 0 {
		return
	}

	// Push Sort into each union branch.
	sortKeys := s.GetSortKeys()
	newChildren := make([]expressions.Quantifier, len(children))
	for i, child := range children {
		pushedSort := expressions.NewLogicalSortExpression(sortKeys, child)
		newChildren[i] = expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	}

	newUnion := expressions.NewLogicalUnionExpression(newChildren)
	call.Yield(newUnion)
}

var _ ExpressionRule = (*PushOrderingThroughUnionRule)(nil)
