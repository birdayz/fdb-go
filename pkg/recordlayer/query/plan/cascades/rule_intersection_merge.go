package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// IntersectionMergeRule flattens nested LogicalIntersection
// expressions, mirroring UnionMergeRule for the intersection
// operator. The inner intersection's children are promoted into the
// outer's child list.
//
//	Intersection(A, Intersection(B, C), D) [keys=K]
//	→
//	Intersection(A, B, C, D) [keys=K]
//
// SQL-equivalent: bag-intersection is associative.
//
// Constraint: the inner Intersection's comparisonKeyValues MUST
// match the outer's. Different keys means the inner's intersection
// is computed under a different equality contract; flattening
// would silently change semantics. Different-keys case declines.
//
// 'Match' here is by Explain text equality of the corresponding
// keys — same bridge as the rest of the seed until Value gains a
// real SemanticEquals.
type IntersectionMergeRule struct {
	matcher matching.BindingMatcher
}

// NewIntersectionMergeRule constructs the rule.
func NewIntersectionMergeRule() *IntersectionMergeRule {
	return &IntersectionMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalIntersectionExpression]("logical_intersection"),
	}
}

// Matcher returns the pattern.
func (r *IntersectionMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when at least one child Quantifier ranges over
// another Intersection AND every such inner Intersection has
// matching comparisonKeyValues.
func (r *IntersectionMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalIntersectionExpression](call.Bindings, r.matcher)
	outerKeys := outer.GetComparisonKeyValues()
	flat, sawNested := flattenIntersectionChildren(outer.GetQuantifiers(), outerKeys)
	if !sawNested {
		return
	}
	call.Yield(expressions.NewLogicalIntersectionExpression(flat, outerKeys))
}

// flattenIntersectionChildren walks `qs` once. For each Quantifier
// whose Reference holds an Intersection with matching keys AND with
// at least one child, promote the inner's children. Returns the new
// slice + a boolean indicating whether any flattening occurred.
//
// An empty inner intersection (zero children) is left in place — its
// row-stream semantics are degenerate (no defined output, NOT
// "universe"), so absorbing it into the outer would silently change
// the outer's child count without adding equivalent rows.
func flattenIntersectionChildren(qs []expressions.Quantifier, outerKeys []values.Value) ([]expressions.Quantifier, bool) {
	out := make([]expressions.Quantifier, 0, len(qs))
	sawNested := false
	for _, q := range qs {
		inner := q.GetRangesOver().Get()
		x, ok := inner.(*expressions.LogicalIntersectionExpression)
		if !ok {
			out = append(out, q)
			continue
		}
		if !sameComparisonKeys(outerKeys, x.GetComparisonKeyValues()) {
			out = append(out, q) // keys differ — leave the Quantifier as-is
			continue
		}
		innerQs := x.GetQuantifiers()
		if len(innerQs) == 0 {
			out = append(out, q) // empty inner — see doc comment above
			continue
		}
		out = append(out, innerQs...)
		sawNested = true
	}
	return out, sawNested
}

// sameComparisonKeys returns true when two key lists have the same
// length AND every pair is Explain-equal.
func sameComparisonKeys(a, b []values.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if values.ExplainValue(a[i]) != values.ExplainValue(b[i]) {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*IntersectionMergeRule)(nil)
