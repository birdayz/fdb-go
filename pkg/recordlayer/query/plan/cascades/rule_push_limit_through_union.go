package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushLimitThroughUnionRule pushes a LIMIT into each branch of a
// UNION ALL. Each branch needs at most (limit + offset) rows since
// the outer LIMIT will pick from the union output. The outer LIMIT
// remains in place — it still needs to enforce the final cardinality.
//
// Pattern:
//
//	LogicalLimit(limit, offset)
//	  inner → LogicalUnion(q1, q2, …)
//
// Rewrite:
//
//	LogicalLimit(limit, offset)
//	  inner → LogicalUnion(
//	    LIMIT(limit+offset, 0, q1.inner),
//	    LIMIT(limit+offset, 0, q2.inner),
//	    …)
//
// The branch limit is limit+offset because the outer LIMIT may skip
// 'offset' rows before taking 'limit' rows.
type PushLimitThroughUnionRule struct {
	matcher matching.BindingMatcher
}

func NewPushLimitThroughUnionRule() *PushLimitThroughUnionRule {
	return &PushLimitThroughUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *PushLimitThroughUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushLimitThroughUnionRule) OnMatch(call *ExpressionRuleCall) {
	limit := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	innerExpr := limit.GetInner().GetRangesOver().Get()
	union, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		return
	}

	branchLimit := limit.GetLimit() + limit.GetOffset()
	if branchLimit <= 0 {
		return
	}

	oldQs := union.GetQuantifiers()

	allHaveLimit := true
	for _, q := range oldQs {
		if _, ok := q.GetRangesOver().Get().(*expressions.LogicalLimitExpression); !ok {
			allHaveLimit = false
			break
		}
	}
	if allHaveLimit {
		return
	}

	newQs := make([]expressions.Quantifier, 0, len(oldQs))
	for _, q := range oldQs {
		branchLim := expressions.NewLogicalLimitExpression(branchLimit, 0, q)
		branchRef := expressions.InitialOf(branchLim)
		newQs = append(newQs, expressions.ForEachQuantifier(branchRef))
	}

	newUnion := expressions.NewLogicalUnionExpression(newQs)
	unionRef := expressions.InitialOf(newUnion)
	unionQ := expressions.ForEachQuantifier(unionRef)

	newOuter := expressions.NewLogicalLimitExpression(limit.GetLimit(), limit.GetOffset(), unionQ)
	call.Yield(newOuter)
}

var _ ExpressionRule = (*PushLimitThroughUnionRule)(nil)
