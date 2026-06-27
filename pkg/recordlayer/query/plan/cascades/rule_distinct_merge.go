package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// DistinctMergeRule collapses LogicalDistinct(LogicalDistinct(X)) into
// a single LogicalDistinct(X) — DISTINCT is idempotent.
//
// Java equivalent: there's no dedicated rule (the Cascades cost model
// would naturally prefer the single-Distinct shape), but the
// transformation is uncontroversial and lets the seed exercise a
// second RelationalExpression rule.
type DistinctMergeRule struct {
	matcher matching.BindingMatcher
}

// NewDistinctMergeRule constructs the rule with its pattern matcher
// pre-built.
func NewDistinctMergeRule() *DistinctMergeRule {
	return &DistinctMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct"),
	}
}

// Matcher returns the pattern.
func (r *DistinctMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch examines the matched LogicalDistinct; if its inner is also a
// LogicalDistinct, yields a single LogicalDistinct over the inner-of-
// inner. Otherwise, declines.
func (r *DistinctMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	inner, ok := innerExpr.(*expressions.LogicalDistinctExpression)
	if !ok {
		return
	}
	rewritten := expressions.NewLogicalDistinctExpression(inner.GetInner())
	call.Yield(rewritten)
}

var _ ExpressionRule = (*DistinctMergeRule)(nil)
