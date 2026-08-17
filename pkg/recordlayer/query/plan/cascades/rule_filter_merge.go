package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FilterMergeRule consolidates a LogicalFilter whose inner Quantifier
// ranges over another LogicalFilter into a single LogicalFilter with
// both predicate lists concatenated. SQL-equivalent: WHERE p1 AND p2
// AND p3 ... regardless of whether the parser nested the conjuncts.
//
// Pattern:
//
//	LogicalFilter([p_outer...])
//	  inner: ForEachQuantifier → Reference holding LogicalFilter([p_inner...])
//	    inner: ForEachQuantifier → Reference holding any RelationalExpression X
//
// Rewrite:
//
//	LogicalFilter([p_outer..., p_inner...])
//	  inner: ForEachQuantifier → Reference holding X
//
// Java equivalent: LogicalFilterMergeRule (or the equivalent
// SimplifyFilterRule) in `cascades/rules/`.
type FilterMergeRule struct {
	matcher matching.BindingMatcher
}

// NewFilterMergeRule constructs the rule with its pattern matcher
// pre-built (avoids per-call allocation in the planner driver).
func NewFilterMergeRule() *FilterMergeRule {
	return &FilterMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *FilterMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch examines the matched LogicalFilter; if its inner Quantifier's
// Reference holds another LogicalFilter, yields a single merged
// LogicalFilter. Otherwise, declines (zero yields).
func (r *FilterMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	inner, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		// Inner isn't a LogicalFilter — rule declines.
		return
	}

	// New filter ranges over what the INNER filter ranged over —
	// strip the redundant inner filter from the chain.
	newInnerQ := inner.GetInner()
	outerInput, err := outer.GetInner().RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	replacementInput, err := newInnerQ.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}

	// The outer predicates are declared against the intermediate filter edge.
	// Removing that edge without moving its exact declaration strands those
	// predicates on an alias no runtime operator binds. Translate only the
	// complete declared edge (correlation plus exact row type) onto the edge
	// that replaces it. Foreign correlations and same-correlation retained
	// windows of another exact type stay untouched and therefore remain loud.
	//
	// Concatenate outer first to preserve SQL textual ordering (the outer
	// filter reads first in the source query, applies first to the row stream).
	merged := make([]predicates.QueryPredicate, 0, len(outer.GetPredicates())+len(inner.GetPredicates()))
	for _, predicate := range outer.GetPredicates() {
		translated, translateErr := predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				return values.TranslateDeclaredEdgeRoot(value, outerInput, replacementInput)
			},
		)
		if translateErr != nil {
			call.Fail(translateErr)
			return
		}
		merged = append(merged, translated)
	}
	merged = append(merged, inner.GetPredicates()...)

	rewritten, err := expressions.NewLogicalFilterExpression(merged, newInnerQ)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(rewritten)
}

// Compile-time assertion: FilterMergeRule satisfies the ExpressionRule
// interface.
var _ ExpressionRule = (*FilterMergeRule)(nil)
