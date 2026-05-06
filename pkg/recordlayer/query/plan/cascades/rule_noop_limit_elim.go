package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// NoOpLimitElimRule eliminates a LIMIT that has no effect.
// A LIMIT with limit < 0 (no cap) AND offset == 0 (no skip) is a
// pure pass-through — the inner expression is equivalent.
//
// Pattern:
//
//	LogicalLimit(limit<0, offset=0)
//	  inner → X
//
// Rewrite: X
type NoOpLimitElimRule struct {
	matcher matching.BindingMatcher
}

func NewNoOpLimitElimRule() *NoOpLimitElimRule {
	return &NoOpLimitElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *NoOpLimitElimRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *NoOpLimitElimRule) OnMatch(call *ExpressionRuleCall) {
	lim := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	if lim.GetLimit() >= 0 || lim.GetOffset() != 0 {
		return
	}
	innerExpr := lim.GetInner().GetRangesOver().Get()
	call.Yield(innerExpr)
}

var _ ExpressionRule = (*NoOpLimitElimRule)(nil)
