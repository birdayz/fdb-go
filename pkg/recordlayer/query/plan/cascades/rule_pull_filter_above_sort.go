package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PullFilterAboveSortRule pulls a LogicalFilter ABOVE a LogicalSort —
// the inverse direction of PushFilterThroughSortRule.
//
//	Sort([k], Filter(P, X))  →  Filter(P, Sort([k], X))
//
// Soundness: filter and sort commute under row admittance — both
// orders yield "rows of X passing P, ordered by k".
//
// Why we keep BOTH this AND PushFilterThroughSort: the two shapes
// coexist as cost-model alternatives. Cost-driven extraction picks
// the cheaper one. Without cost, both stay; exploration terminates
// because Reference.Insert's SemanticEquals fallback absorbs
// structurally-equivalent re-yields after the first round.
//
// Optimization argument: depending on costs, either form can win:
//   - Push (filter under sort): sort fewer rows
//   - Pull (filter above sort): potentially benefit from
//     index-ordered scan that's already sorted, so the sort can be
//     eliminated downstream by UnsortedSortElim or natural-ordering
//     rules
type PullFilterAboveSortRule struct {
	matcher matching.BindingMatcher
}

// NewPullFilterAboveSortRule constructs the rule.
func NewPullFilterAboveSortRule() *PullFilterAboveSortRule {
	return &PullFilterAboveSortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *PullFilterAboveSortRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalFilterExpression.
func (r *PullFilterAboveSortRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	innerExpr := s.GetInner().GetRangesOver().Get()
	f, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		return
	}
	pulled := expressions.NewLogicalSortExpression(s.GetSortKeys(), f.GetInner())
	pulledQ := expressions.ForEachQuantifier(call.MemoizeExpression(pulled))
	call.Yield(expressions.NewLogicalFilterExpression(f.GetPredicates(), pulledQ))
}

var _ ExpressionRule = (*PullFilterAboveSortRule)(nil)
