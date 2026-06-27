package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushFilterThroughTypeFilterRule pushes a LogicalFilter under a
// LogicalTypeFilter.
//
//	Filter(P, TypeFilter([T], X))  →  TypeFilter([T], Filter(P, X))
//
// Soundness: TypeFilter doesn't reshape rows (its GetResultValue is
// the inner's flowed object value). FieldValue references inside P
// resolve against the same row shape on both sides. The TypeFilter
// admits only rows of the listed types; the Filter admits only rows
// satisfying P. Composing the two predicates is commutative for
// row admittance — same output rows.
//
// Optimization argument: TypeFilter is typically cheap (record-type
// dispatch). Filter is potentially expensive (predicate evaluation).
// Putting Filter "lower" in the tree gives downstream physical-plan
// rules (B5 Batch A) a chance to push the predicate INTO the scan
// itself (e.g. into a covering-index range scan).
type PushFilterThroughTypeFilterRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughTypeFilterRule constructs the rule.
func NewPushFilterThroughTypeFilterRule() *PushFilterThroughTypeFilterRule {
	return &PushFilterThroughTypeFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughTypeFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalTypeFilterExpression.
func (r *PushFilterThroughTypeFilterRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	tf, ok := innerExpr.(*expressions.LogicalTypeFilterExpression)
	if !ok {
		return
	}
	pushed := expressions.NewLogicalFilterExpression(f.GetPredicates(), tf.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	call.Yield(expressions.NewLogicalTypeFilterExpression(tf.GetRecordTypes(), pushedQ))
}

var _ ExpressionRule = (*PushFilterThroughTypeFilterRule)(nil)
