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

	newLimit, err := expressions.NewLogicalLimitExpression(
		limit.GetLimit(), limit.GetOffset(), proj.GetInner(),
	)
	if err != nil {
		call.Fail(err)
		return
	}
	limitRef := expressions.InitialOf(newLimit)
	// The rewritten Projection retains the original projection program. Keep
	// that program's declared input alias on the new LIMIT edge: LIMIT is a
	// row-preserving wrapper, so changing the edge identity without rebasing the
	// Values severs their only binding. This is observable when the child is a
	// materializing sort, whose output is deliberately current-only — the stale
	// source QOV cannot be recovered from a source window after materialization.
	limitQ := expressions.NamedForEachQuantifier(proj.GetInner().GetAlias(), limitRef)

	// Carry the projection's output ALIASES across the rebuild. Dropping
	// them changed only the result METADATA, so rows stayed correct while
	// every aliased column reverted to its base name:
	// `SELECT l.id AS l_id, r.id AS r_id FROM t AS l JOIN t AS r ON …
	// LIMIT k` reported two columns both named ID. Same query without the
	// LIMIT — no push, no rebuild — kept l_id/r_id.
	newProj, err := expressions.NewLogicalProjectionExpressionWithOutputSchema(
		proj.GetProjectedValues(), proj.GetAliases(), proj.GetAliasMinted(), proj.GetOutputNames(), limitQ)
	if err != nil {
		call.Fail(err)
		return
	}
	newProj, err = newProj.WithAliasSources(proj.GetAliasSources())
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(newProj.WithInheritedOutputIdentity(proj))
}

var _ ExpressionRule = (*PushLimitThroughProjectionRule)(nil)
