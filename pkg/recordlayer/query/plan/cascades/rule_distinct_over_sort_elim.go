package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// DistinctOverSortElimRule eliminates a LogicalSort that sits
// directly under a LogicalDistinct.
//
//	Distinct(Sort([k1, k2, ...], X))  →  Distinct(X)
//
// SQL semantic justification: DISTINCT operates on a row-set
// (unordered); the inner Sort's ordering is lost the moment Distinct
// dedupes. So the inner sort is wasted work.
//
// Edge case — Sort([]) (Unsorted): the inner is a no-op anyway.
// UnsortedSortElim handles it independently. This rule firing on
// that case is harmless (yields Distinct(unsorted's inner) = same
// thing UnsortedSortElim would have produced post-merge).
//
// Termination: yields Distinct(sort.GetInner()) — REUSES the inner
// Sort's Quantifier (and therefore its Reference pointer). Second
// fire of the rule on the same input member produces a structurally-
// identical Distinct (same children-Reference pointers), so
// Reference.Insert's sameChildReferences dedup absorbs it.
//
// Java equivalent: emerges from cost preference for cheaper plans.
// Seed implements directly so the optimiser produces the
// concretely-cheaper logical tree before B4 cost lands.
type DistinctOverSortElimRule struct {
	matcher matching.BindingMatcher
}

// NewDistinctOverSortElimRule constructs the rule.
func NewDistinctOverSortElimRule() *DistinctOverSortElimRule {
	return &DistinctOverSortElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct"),
	}
}

// Matcher returns the pattern.
func (r *DistinctOverSortElimRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalSortExpression. Yields Distinct over the sort's inner
// Quantifier directly (NOT a fresh wrap — see Termination note).
func (r *DistinctOverSortElimRule) OnMatch(call *ExpressionRuleCall) {
	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerExpr := d.GetInner().GetRangesOver().Get()
	sort, ok := innerExpr.(*expressions.LogicalSortExpression)
	if !ok {
		return
	}
	// Reuse the sort's inner Quantifier — preserves Reference pointer
	// across rule fires, so Reference.Insert dedupes second-fire.
	call.Yield(expressions.NewLogicalDistinctExpression(sort.GetInner()))
}

var _ ExpressionRule = (*DistinctOverSortElimRule)(nil)
