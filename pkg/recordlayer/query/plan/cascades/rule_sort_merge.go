package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// SortMergeRule eliminates a wasted inner sort when its result is
// immediately re-sorted by an outer LogicalSortExpression.
//
//	Sort([k1, k2, ...]) over Sort([j1, j2, ...]) over X
//	→
//	Sort([k1, k2, ...]) over X
//
// The outer sort fully determines the final order — the inner sort's
// work is discarded the moment the outer re-orders. Eliminating the
// inner sort never changes the result rows OR their final order; it
// only saves the wasted intermediate ordering work.
//
// Edge cases:
//   - If the outer sort is unsorted (Sort([])), the rewrite would
//     destroy the inner's ordering. Decline in that case — the
//     UnsortedSortElim rule handles unsorted Sorts on its own pass
//     by eliminating them from outer-side inputs.
//   - If the inner sort is unsorted (Sort([])), the rule still fires:
//     dropping a no-op intermediate is cheap and structurally cleaner.
//     (Technically UnsortedSortElim would also have caught the inner
//     by itself — but having both rules cooperate is fine.)
//
// Java equivalent: emerges from cost preference for fewer operators.
// Seed implements directly so the optimiser's logical phase produces
// a concretely-cleaner tree before B4 cost lands.
type SortMergeRule struct {
	matcher matching.BindingMatcher
}

// NewSortMergeRule constructs the rule.
func NewSortMergeRule() *SortMergeRule {
	return &SortMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *SortMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over another
// LogicalSort AND the outer is non-empty (otherwise the rewrite
// would destroy the inner's ordering — see doc comment).
func (r *SortMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if outer.IsUnsorted() {
		return
	}
	innerExpr := outer.GetInner().GetRangesOver().Get()
	innerSort, ok := innerExpr.(*expressions.LogicalSortExpression)
	if !ok {
		return
	}
	// Yield: outer sort over inner-sort's inner.
	call.Yield(expressions.NewLogicalSortExpression(
		outer.GetSortKeys(),
		innerSort.GetInner(),
	))
}

var _ ExpressionRule = (*SortMergeRule)(nil)
