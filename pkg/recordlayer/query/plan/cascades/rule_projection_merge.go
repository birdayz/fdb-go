package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// ProjectionMergeRule collapses two nested LogicalProjections into one.
//
//	Projection(P1) over Projection(P2) over X
//	→
//	Projection(P1) over X
//
// The outer projection list P1 wins outright — it specifies exactly
// the columns the caller wants. The inner projection list P2 was
// already a "narrowing" pass; doing both ends up materialising
// columns the outer doesn't need only to throw them away.
//
// Soundness contract: Values inside P1 are FieldValue references
// against the inner projection's flow, and LogicalProjection's
// GetResultValue returns the inner quantifier's flowed object value
// (the projection list is a side channel; rows pass through). So a
// FieldValue inside P1 logically resolves against the same row shape
// whether the inner projection is present or not — collapsing is safe.
// If projection ever narrows the row shape, this rule needs a
// column-substitution rewrite (see the caveat on
// DefaultExpressionRules).
//
// This is more aggressive than Java's planner (which lets the cost
// model + cardinality estimates decide): no dedicated Java rule
// exists — the Memo's cost preference for fewer Projection operators
// emerges from physical plan choice (a fetch-only plan with the wider
// column list is cheaper than two stacked Projection operators). Go
// gets the same shape via this static rewrite.
type ProjectionMergeRule struct {
	matcher matching.BindingMatcher
}

// NewProjectionMergeRule constructs the rule.
func NewProjectionMergeRule() *ProjectionMergeRule {
	return &ProjectionMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("logical_projection"),
	}
}

// Matcher returns the pattern.
func (r *ProjectionMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over another
// LogicalProjection; yields a flat projection wrapping the inner's
// inner.
func (r *ProjectionMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalProjectionExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	innerProj, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return
	}
	// Take the inner projection's inner Quantifier and wrap with the
	// outer's projection list.
	flat := expressions.NewLogicalProjectionExpression(
		outer.GetProjectedValues(),
		innerProj.GetInner(),
	)
	call.Yield(flat)
}

var _ ExpressionRule = (*ProjectionMergeRule)(nil)
