package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PullFilterAboveProjectionRule pulls a LogicalFilter ABOVE a
// LogicalProjection — the inverse direction of
// PushFilterThroughProjectionRule.
//
//	Projection([cols], Filter(P, X))  →  Filter(P, Projection([cols], X))
//
// Soundness: LogicalProjection in the seed doesn't reshape rows
// (GetResultValue is the inner's flowed-object pass-through; the
// projection list is a side channel describing exposed columns).
// FieldValue references inside P resolve identically either side.
//
// Why we keep BOTH this AND PushFilterThroughProjection: the two
// shapes coexist as cost-model alternatives. Cost-driven extraction
// (B4 follow-on) picks the cheaper.
type PullFilterAboveProjectionRule struct {
	matcher matching.BindingMatcher
}

// NewPullFilterAboveProjectionRule constructs the rule.
func NewPullFilterAboveProjectionRule() *PullFilterAboveProjectionRule {
	return &PullFilterAboveProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *PullFilterAboveProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalFilterExpression.
func (r *PullFilterAboveProjectionRule) OnMatch(call *ExpressionRuleCall) {
	p := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	innerExpr := p.GetInner().GetRangesOver().Get()
	f, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		return
	}
	pulled := expressions.NewLogicalProjectionExpression(p.GetProjectedValues(), f.GetInner())
	pulledQ := expressions.ForEachQuantifier(call.MemoizeExpression(pulled))
	call.Yield(expressions.NewLogicalFilterExpression(f.GetPredicates(), pulledQ))
}

var _ ExpressionRule = (*PullFilterAboveProjectionRule)(nil)
