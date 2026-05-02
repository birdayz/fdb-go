package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PullFilterAboveDistinctRule pulls a LogicalFilter ABOVE a
// LogicalDistinct — the inverse direction of
// PushFilterThroughDistinctRule.
//
//	Distinct(Filter(P, X))  →  Filter(P, Distinct(X))
//
// Soundness: same row set either side. Filter narrows by P; Distinct
// dedupes. Both compositions admit "DISTINCT rows of X passing P".
//
// Why we keep BOTH this AND PushFilterThroughDistinct: the two
// shapes coexist in the memo as alternatives. Cost-model extraction
// (B4 follow-on) picks the cheaper. Without cost, both stay;
// FixpointApply terminates because Reference.Insert's SemanticEquals
// fallback absorbs structurally-equivalent re-yields after the first
// round.
//
// Optimization argument: filtering AFTER dedup means evaluating P on
// fewer rows (Distinct pre-shrinks the set) — the inverse trade-off
// from PushFilterThroughDistinct. Which is cheaper depends on
// dedup-vs-filter cost; the cost model picks.
type PullFilterAboveDistinctRule struct {
	matcher matching.BindingMatcher
}

// NewPullFilterAboveDistinctRule constructs the rule.
func NewPullFilterAboveDistinctRule() *PullFilterAboveDistinctRule {
	return &PullFilterAboveDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct"),
	}
}

// Matcher returns the pattern.
func (r *PullFilterAboveDistinctRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalFilterExpression.
func (r *PullFilterAboveDistinctRule) OnMatch(call *ExpressionRuleCall) {
	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerExpr := d.GetInner().GetRangesOver().Get()
	f, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		return
	}
	// Build Distinct(f.GetInner-source) — REUSE f's inner Quantifier.
	pulled := expressions.NewLogicalDistinctExpression(f.GetInner())
	pulledQ := expressions.ForEachQuantifier(call.MemoizeExpression(pulled))
	call.Yield(expressions.NewLogicalFilterExpression(f.GetPredicates(), pulledQ))
}

var _ ExpressionRule = (*PullFilterAboveDistinctRule)(nil)
