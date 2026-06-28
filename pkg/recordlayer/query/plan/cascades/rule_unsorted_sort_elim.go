package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// UnsortedSortElimRule eliminates a LogicalSort whose sort-key list
// is empty (the no-op sort produced by UnsortedLogicalSortExpression
// or by query rewrites that drop all sort keys).
//
// Pattern:
//
//	LogicalSort([]) over X
//	→
//	X
//
// This is the sort analogue of NoOpFilterRule. A sort with no keys
// preserves order — equivalent to no sort at all.
//
// Java equivalent: not a dedicated rule, but the planner's cost model
// would naturally prefer the un-wrapped X over the no-op-Sort wrapper.
// The seed implements it directly.
type UnsortedSortElimRule struct {
	matcher matching.BindingMatcher
}

// NewUnsortedSortElimRule constructs the rule.
func NewUnsortedSortElimRule() *UnsortedSortElimRule {
	return &UnsortedSortElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *UnsortedSortElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the sort is unsorted (empty key list).
func (r *UnsortedSortElimRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if !s.IsUnsorted() {
		return
	}
	call.Yield(s.GetInner().GetRangesOver().Get())
}

var _ ExpressionRule = (*UnsortedSortElimRule)(nil)
