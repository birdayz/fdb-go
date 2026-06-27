package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushFilterThroughIntersectionRule distributes a LogicalFilter
// over a LogicalIntersection's children.
//
//	Filter(P, Intersection(A, B, ..., [keys=K]))
//	→
//	Intersection(Filter(P, A), Filter(P, B), ..., [keys=K])
//
// Soundness: row-set equivalence — filter commutes with intersection.
// `rows in (A ∩ B) passing P` equals `(A passing P) ∩ (B passing P)`.
// The comparison-key list K is preserved exactly; the keys are about
// HOW the intersection compares rows, not which rows it admits.
//
// Optimization argument: same as the Union variant — distributing
// the filter into each operand gives downstream pushdown rules a
// chance to push the predicate into each operand's scan / index.
//
// Note: produces a structurally LARGER tree (N filters instead of 1).
// The benefit comes from follow-on rules. Same termination contract
// as PushFilterThroughDistinct: Reference.Insert's SemanticEquals
// fallback (commit 680e664a) absorbs the structurally-equivalent
// re-yield on subsequent fires.
type PushFilterThroughIntersectionRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughIntersectionRule constructs the rule.
func NewPushFilterThroughIntersectionRule() *PushFilterThroughIntersectionRule {
	return &PushFilterThroughIntersectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughIntersectionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

// OnMatch fires when the inner Quantifier ranges over a
// LogicalIntersectionExpression with at least one child.
func (r *PushFilterThroughIntersectionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	x, ok := innerExpr.(*expressions.LogicalIntersectionExpression)
	if !ok {
		return
	}
	children := x.GetQuantifiers()
	if len(children) == 0 {
		return
	}
	pushed := make([]expressions.Quantifier, 0, len(children))
	for _, child := range children {
		fc := expressions.NewLogicalFilterExpression(f.GetPredicates(), child)
		pushed = append(pushed, expressions.ForEachQuantifier(call.MemoizeExpression(fc)))
	}
	call.Yield(expressions.NewLogicalIntersectionExpression(pushed, x.GetComparisonKeyValues()))
}

var _ ExpressionRule = (*PushFilterThroughIntersectionRule)(nil)
