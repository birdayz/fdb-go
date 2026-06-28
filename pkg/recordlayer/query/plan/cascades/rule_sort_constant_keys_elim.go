package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SortConstantKeysElimRule eliminates a LogicalSort whose every key
// is a row-independent constant.
//
//	Sort([ConstantValue(42), ConstantValue('x')], X)  →  X
//
// SQL semantic: ORDER BY 42 sorts rows by a value that's the same
// for every row — the resulting order is undefined / arbitrary.
// Same for ORDER BY 'x', BY TRUE, BY NULL. Stripping the sort
// doesn't change result-set semantics; it just removes wasted work.
//
// Soundness: IsConstantValue answers "this Value's Evaluate is
// independent of the row context" — exactly the property we need
// for "all rows would sort to the same key". When every sort key
// has IsConstantValue=true, the sort produces no ordering refinement.
//
// Termination: yields the inner expression directly (no new wrapper).
// Pointer-identity dedup on second fire — the inner is the same
// expression object as before.
//
// Edge case — empty sort: Sort([]) is the Unsorted form, which
// UnsortedSortElim handles independently. Letting both rules fire
// is harmless (UnsortedSortElim hits first; this rule's emptiness
// check declines).
type SortConstantKeysElimRule struct {
	matcher matching.BindingMatcher
}

// NewSortConstantKeysElimRule constructs the rule.
func NewSortConstantKeysElimRule() *SortConstantKeysElimRule {
	return &SortConstantKeysElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("logical_sort"),
	}
}

// Matcher returns the pattern.
func (r *SortConstantKeysElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the sort has at least one key AND every key's
// Value is row-context-independent (IsConstantValue=true).
func (r *SortConstantKeysElimRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	keys := s.GetSortKeys()
	if len(keys) == 0 {
		return // UnsortedSortElim's territory
	}
	for _, k := range keys {
		if !values.IsConstantValue(k.Value) {
			return
		}
	}
	// All keys constant — sort is a no-op. Yield inner directly.
	call.Yield(s.GetInner().GetRangesOver().Get())
}

var _ ExpressionRule = (*SortConstantKeysElimRule)(nil)
