package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushFilterThroughSortRule pushes a LogicalFilter under a
// LogicalSort.
//
//	Filter(P, Sort([k1, ...], X))  →  Sort([k1, ...], Filter(P, X))
//
// Soundness: Sort doesn't reshape rows (it changes order, not
// admittance). FieldValue references inside P resolve identically
// either side of the rewrite. SQL semantics: filter-then-sort and
// sort-then-filter produce the same final ordering — both apply
// the filter AND then sort the surviving rows.
//
// Optimization argument: filtering BEFORE sort means fewer rows for
// the (potentially expensive) sort to process.
type PushFilterThroughSortRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughSortRule constructs the rule.
func NewPushFilterThroughSortRule() *PushFilterThroughSortRule {
	return &PushFilterThroughSortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughSortRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalSortExpression.
func (r *PushFilterThroughSortRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	s, ok := innerExpr.(*expressions.LogicalSortExpression)
	if !ok {
		return
	}
	pushed := expressions.NewLogicalFilterExpression(f.GetPredicates(), s.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	call.Yield(expressions.NewLogicalSortExpression(s.GetSortKeys(), pushedQ))
}

var _ ExpressionRule = (*PushFilterThroughSortRule)(nil)
