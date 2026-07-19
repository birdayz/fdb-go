package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushLimitThroughProjectionRule pushes a LIMIT below a Projection.
// LogicalProjection is a pure row pass-through (row shape unchanged;
// the projection list is a side channel) so the LIMIT can safely move
// below it — reduces the number of rows the projection processes.
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
	// A runtime cap (parameterized RFC-156 rank limit) carries a Value the
	// int64-only rebuild below would drop — leave it in place.
	if limit.GetLimitValue() != nil {
		return
	}
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

	// Carry the projection's output ALIASES across the rebuild. Dropping
	// them changed only the result METADATA, so rows stayed correct while
	// every aliased column reverted to its base name:
	// `SELECT l.id AS l_id, r.id AS r_id FROM t AS l JOIN t AS r ON …
	// LIMIT k` reported two columns both named ID. Same query without the
	// LIMIT — no push, no rebuild — kept l_id/r_id.
	newProj := expressions.NewLogicalProjectionExpressionWithAliases(
		proj.GetProjectedValues(), proj.GetAliases(), limitQ)
	call.Yield(newProj)
}

var _ ExpressionRule = (*PushLimitThroughProjectionRule)(nil)
