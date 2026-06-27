package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PullCommonFilterAboveIntersectionRule combines an Intersection
// whose every child is a LogicalFilter with the SAME predicate list
// into a single Filter above the Intersection. The reverse direction
// of PushFilterThroughIntersection.
//
//	Intersection(Filter([P], A), Filter([P], B), ..., keys=K)
//	→
//	Filter([P], Intersection(A, B, ..., keys=K))
//
// Soundness: filter and bag-intersection commute under row admittance.
// `(A passing P) ∩ (B passing P)` = `(A ∩ B) passing P`. The
// comparison-key list K is preserved exactly.
//
// Optimization: collapsing N filters into 1 reduces operator count.
// Useful when downstream rules (e.g. predicate folding) can do more
// with one combined Filter than N independent ones.
type PullCommonFilterAboveIntersectionRule struct {
	matcher matching.BindingMatcher
}

// NewPullCommonFilterAboveIntersectionRule constructs the rule.
func NewPullCommonFilterAboveIntersectionRule() *PullCommonFilterAboveIntersectionRule {
	return &PullCommonFilterAboveIntersectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalIntersectionExpression]("logical_intersection"),
	}
}

// Matcher returns the pattern.
func (r *PullCommonFilterAboveIntersectionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

// OnMatch fires when every child Quantifier ranges over a Filter
// AND every Filter has the SAME predicate list (Explain-equal).
func (r *PullCommonFilterAboveIntersectionRule) OnMatch(call *ExpressionRuleCall) {
	x := matching.Get[*expressions.LogicalIntersectionExpression](call.Bindings, r.matcher)
	children := x.GetQuantifiers()
	if len(children) < 2 {
		return // single-child case: IntersectionSingletonElim handles it
	}
	commonPreds, allFilters := commonFilterPredicates(children)
	if !allFilters || commonPreds == nil {
		return
	}
	newQs := make([]expressions.Quantifier, 0, len(children))
	for _, q := range children {
		f := q.GetRangesOver().Get().(*expressions.LogicalFilterExpression)
		newQs = append(newQs, f.GetInner())
	}
	newX := expressions.NewLogicalIntersectionExpression(newQs, x.GetComparisonKeyValues())
	newXQ := expressions.ForEachQuantifier(call.MemoizeExpression(newX))
	call.Yield(expressions.NewLogicalFilterExpression(commonPreds, newXQ))
}

var _ ExpressionRule = (*PullCommonFilterAboveIntersectionRule)(nil)
