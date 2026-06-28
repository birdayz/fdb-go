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
// Soundness in the seed model: Values inside P1 are FieldValue
// references against the inner projection's flow. In the
// LogicalProjection seed, GetResultValue returns the inner
// quantifier's flowed object value (the projection list is a side
// channel; rows pass through). So a FieldValue inside P1 logically
// resolves against the same row shape whether the inner projection
// is present or not. Collapsing is safe.
//
// This is more aggressive than Java's planner (which lets the cost
// model + cardinality estimates decide). The seed implements it
// directly because it produces a concretely-simpler tree, which is
// useful for plan-text equivalence tests until B4 (cost) lands.
//
// Java equivalent: no dedicated rule — the Memo's cost preference
// for fewer Projection operators emerges from physical plan choice
// (a fetch-only plan with the wider column list is cheaper than two
// stacked Projection operators). The seed gets the same shape via
// this static rewrite.
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
