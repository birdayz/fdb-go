package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushLimitThroughProjectionRule pushes a LIMIT below a Projection.
// The Projection is a pure pass-through in the seed (row shape
// unchanged) so the LIMIT can safely move below it — reduces the
// number of rows the projection processes.
//
// Pattern:
//
//	LogicalLimit(limit, offset)
//	  inner → LogicalProjection(values)
//	    inner → X
//
// Rewrite:
//
//	LogicalProjection(values)
//	  inner → LogicalLimit(limit, offset)
//	    inner → X
type PushLimitThroughProjectionRule struct {
	matcher matching.BindingMatcher
}

func NewPushLimitThroughProjectionRule() *PushLimitThroughProjectionRule {
	return &PushLimitThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *PushLimitThroughProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushLimitThroughProjectionRule) OnMatch(call *ExpressionRuleCall) {
	limit := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	innerExpr := limit.GetInner().GetRangesOver().Get()
	proj, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return
	}

	newLimit := expressions.NewLogicalLimitExpression(
		limit.GetLimit(), limit.GetOffset(), proj.GetInner(),
	)
	limitRef := expressions.InitialOf(newLimit)
	limitQ := expressions.ForEachQuantifier(limitRef)

	newProj := expressions.NewLogicalProjectionExpression(proj.GetProjectedValues(), limitQ)
	call.Yield(newProj)
}

var _ ExpressionRule = (*PushLimitThroughProjectionRule)(nil)
