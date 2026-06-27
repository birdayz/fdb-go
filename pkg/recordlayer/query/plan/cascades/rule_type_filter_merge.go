package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// TypeFilterMergeRule consolidates two nested LogicalTypeFilter
// expressions into a single one with the INTERSECTION of their
// record-type sets.
//
//	TypeFilter([T_outer...]) over TypeFilter([T_inner...]) over X
//	→
//	TypeFilter([T_outer ∩ T_inner]) over X
//
// SQL-equivalent: nested type narrowing is the same as one narrowing
// to the intersection. If the intersection is empty the rule still
// fires — downstream rules will fold the empty-type-filter into a
// no-row-emission no-op.
//
// Java equivalent: the planner would naturally derive this via the
// type-narrowing rules. The seed implements it directly to exercise
// rule logic that touches node-information set arithmetic.
type TypeFilterMergeRule struct {
	matcher matching.BindingMatcher
}

// NewTypeFilterMergeRule constructs the rule.
func NewTypeFilterMergeRule() *TypeFilterMergeRule {
	return &TypeFilterMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalTypeFilterExpression]("logical_type_filter"),
	}
}

// Matcher returns the pattern.
func (r *TypeFilterMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner is also a LogicalTypeFilter; yields a
// single TypeFilter with the intersection of the type sets.
func (r *TypeFilterMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalTypeFilterExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	inner, ok := innerExpr.(*expressions.LogicalTypeFilterExpression)
	if !ok {
		return
	}
	intersected := intersectStringSlices(outer.GetRecordTypes(), inner.GetRecordTypes())
	rewritten := expressions.NewLogicalTypeFilterExpression(intersected, inner.GetInner())
	call.Yield(rewritten)
}

// intersectStringSlices returns the sorted intersection of two
// already-sorted+deduped string slices. Both inputs come from
// LogicalTypeFilterExpression.GetRecordTypes, which guarantees that
// canonical form. O(N+M) two-pointer walk.
func intersectStringSlices(a, b []string) []string {
	out := make([]string, 0, min(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

var _ ExpressionRule = (*TypeFilterMergeRule)(nil)
