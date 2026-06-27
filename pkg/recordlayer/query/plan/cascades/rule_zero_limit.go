package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ZeroLimitRule replaces LIMIT 0 with an empty scan — the inner
// expression can never produce rows when limit is zero.
//
// Pattern:
//
//	LogicalLimit(limit=0, any offset)
//	  inner → X
//
// Rewrite:
//
//	FullUnorderedScan([]) with EmptyType (zero-row source)
//
// In practice, downstream execution short-circuits on the
// RecordQueryLimitPlan(limit=0) anyway, but removing the sub-tree
// lets the planner avoid costing the child entirely.
type ZeroLimitRule struct {
	matcher matching.BindingMatcher
}

func NewZeroLimitRule() *ZeroLimitRule {
	return &ZeroLimitRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *ZeroLimitRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ZeroLimitRule) OnMatch(call *ExpressionRuleCall) {
	lim := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	if lim.GetLimit() != 0 {
		return
	}
	empty := expressions.NewFullUnorderedScanExpression(nil, values.UnknownType)
	call.Yield(empty)
}

var _ ExpressionRule = (*ZeroLimitRule)(nil)
