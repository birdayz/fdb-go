package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// FilterDropTruePredicatesRule drops TriTrue ConstantPredicates from
// a LogicalFilter's predicate list (without eliminating the filter
// itself). Composes naturally with FilterMergeRule (which can leave
// trivial TRUE conjuncts after merging) and NoOpFilterRule (which
// then eliminates the filter if ALL predicates are TRUE).
//
// Pattern:
//
//	Filter([..., TriTrue-ConstantPredicate, ...]) over X
//	→
//	Filter([... without TriTrue ...]) over X
//
// Yields the new Filter only if at least one TriTrue predicate is
// dropped — otherwise declines.
//
// If dropping leaves the predicate list empty, the rule still fires;
// NoOpFilterRule will then eliminate the now-trivial Filter on a
// subsequent fixpoint iteration.
type FilterDropTruePredicatesRule struct {
	matcher matching.BindingMatcher
}

// NewFilterDropTruePredicatesRule constructs the rule.
func NewFilterDropTruePredicatesRule() *FilterDropTruePredicatesRule {
	return &FilterDropTruePredicatesRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *FilterDropTruePredicatesRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when at least one predicate is TriTrue.
func (r *FilterDropTruePredicatesRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	dropped := false
	kept := make([]predicates.QueryPredicate, 0, len(f.GetPredicates()))
	for _, p := range f.GetPredicates() {
		cp, ok := p.(*predicates.ConstantPredicate)
		if ok && cp.Value == predicates.TriTrue {
			dropped = true
			continue
		}
		kept = append(kept, p)
	}
	if !dropped {
		return
	}
	call.Yield(expressions.NewLogicalFilterExpression(kept, f.GetInner()))
}

var _ ExpressionRule = (*FilterDropTruePredicatesRule)(nil)
