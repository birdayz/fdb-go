package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// NoOpFilterRule eliminates a LogicalFilter whose predicate list is
// empty OR whose predicates all evaluate (under the constant fold)
// to a tri-state TRUE. The replacement is the inner Quantifier's
// expression — the filter is a row-by-row identity at that point.
//
// Two firing conditions, both equivalent SQL no-ops:
//
//  1. Empty predicate list — comes up after FilterMergeRule + a
//     follow-on rule that folds out tautologies, leaving an empty
//     conjunction.
//  2. All predicates are ConstantPredicate(TriTrue) — the trivial
//     `WHERE TRUE` shape.
//
// Yields the inner expression directly, NOT a fresh wrapper. The
// memo dedup absorbs duplicate inserts via Reference.Insert.
type NoOpFilterRule struct {
	matcher matching.BindingMatcher
}

// NewNoOpFilterRule constructs the rule.
func NewNoOpFilterRule() *NoOpFilterRule {
	return &NoOpFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *NoOpFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the filter is a no-op.
func (r *NoOpFilterRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	if !isNoOpPredicateList(f.GetPredicates()) {
		return
	}
	innerExpr := f.GetInner().GetRangesOver().Get()
	call.Yield(innerExpr)
}

// isNoOpPredicateList reports whether `preds` is empty or contains
// only TriTrue ConstantPredicates. Anything else (including TriFalse,
// TriUnknown, or non-constant predicates) is NOT a no-op.
func isNoOpPredicateList(preds []predicates.QueryPredicate) bool {
	for _, p := range preds {
		cp, ok := p.(*predicates.ConstantPredicate)
		if !ok {
			return false
		}
		if cp.Value != predicates.TriTrue {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*NoOpFilterRule)(nil)
