package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
// SimplifyFilterRule) in `cascades/rules/`. Our seed lands the
// transformation directly here; once the rules sub-package splits
// (RFC-025 Phase 2) this moves alongside its peers.
//
// First RelationalExpression rule in the codebase — proves the B1 +
// B3 seed plumbing (Reference / Quantifier / ExpressionRuleCall +
// ExpressionMatcher) holds together end-to-end.
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

	// Concatenate predicate lists. Outer first preserves the SQL
	// textual ordering (WHERE p_outer applied to rows from p_inner).
	merged := make([]predicates.QueryPredicate, 0, len(outer.GetPredicates())+len(inner.GetPredicates()))
	merged = append(merged, outer.GetPredicates()...)
	merged = append(merged, inner.GetPredicates()...)

	// New filter ranges over what the INNER filter ranged over —
	// strip the redundant inner filter from the chain.
	newInnerQ := inner.GetInner()
	rewritten := expressions.NewLogicalFilterExpression(merged, newInnerQ)
	call.Yield(rewritten)
}

// Compile-time assertion: FilterMergeRule satisfies the ExpressionRule
// interface.
var _ ExpressionRule = (*FilterMergeRule)(nil)
