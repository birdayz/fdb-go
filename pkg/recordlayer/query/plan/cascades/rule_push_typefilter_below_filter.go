package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushTypeFilterBelowFilterRule pushes a LogicalTypeFilter under a
// LogicalFilter — the inverse of PushFilterThroughTypeFilterRule.
//
//	TypeFilter([T], Filter(P, X))  →  Filter(P, TypeFilter([T], X))
//
// Soundness: filter and type-filter commute under row admittance.
// Either order yields "rows of type-set T that satisfy P".
//
// Optimization argument: TypeFilter is cheap (record-type dispatch).
// Pushing it CLOSER to the leaf means Filter operates on fewer rows
// (only T-typed rows reach the Filter). Wins when Filter's predicate
// is expensive.
//
// Why we keep BOTH this AND PushFilterThroughTypeFilterRule: the two
// shapes coexist in the memo as alternatives. Cost-model extraction
// (B4 follow-on) picks the cheaper one. Without cost, both shapes
// stay; FixpointApply terminates because Reference.Insert's
// SemanticEquals fallback absorbs structurally-equivalent re-yields
// after the first round of rule firing.
//
// Java equivalent: PushTypeFilterBelowFilterRule.
type PushTypeFilterBelowFilterRule struct {
	matcher matching.BindingMatcher
}

// NewPushTypeFilterBelowFilterRule constructs the rule.
func NewPushTypeFilterBelowFilterRule() *PushTypeFilterBelowFilterRule {
	return &PushTypeFilterBelowFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalTypeFilterExpression]("logical_type_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushTypeFilterBelowFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalFilterExpression.
func (r *PushTypeFilterBelowFilterRule) OnMatch(call *ExpressionRuleCall) {
	tf := matching.Get[*expressions.LogicalTypeFilterExpression](call.Bindings, r.matcher)
	innerExpr := tf.GetInner().GetRangesOver().Get()
	f, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		return
	}
	// Build TypeFilter([T], f.GetInner-source). REUSE f's inner
	// Quantifier so the Filter wrapping has the same Reference
	// pointer as the inner of f.
	pushed := expressions.NewLogicalTypeFilterExpression(tf.GetRecordTypes(), f.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	call.Yield(expressions.NewLogicalFilterExpression(f.GetPredicates(), pushedQ))
}

var _ ExpressionRule = (*PushTypeFilterBelowFilterRule)(nil)
