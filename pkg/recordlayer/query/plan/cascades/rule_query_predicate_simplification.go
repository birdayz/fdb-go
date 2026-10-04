package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// QueryPredicateSimplificationRule simplifies the whole conjunction and tests
// semantic equality for convergence, like Java's rule of the same name.
type QueryPredicateSimplificationRule struct {
	matcher matching.BindingMatcher
}

func NewQueryPredicateSimplificationRule() *QueryPredicateSimplificationRule {
	return &QueryPredicateSimplificationRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("query_predicate_simplification"),
	}
}

func (r *QueryPredicateSimplificationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *QueryPredicateSimplificationRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)

	originalPredicates := sel.GetPredicates()
	if len(originalPredicates) == 0 {
		return
	}

	// Java simplifies the conjunction, not isolated value operands: a sibling
	// can absorb an OR or supply the identity that eliminates another factor.
	conjunction := buildAnd(originalPredicates)
	simplifiedConjunction, err := Simplify(predicates.SimplifyPredicateValues(conjunction), queryPredicateSimplificationRules())
	if err != nil {
		call.Fail(err)
		return
	}
	if predicates.PredicateEquals(conjunction, simplifiedConjunction) {
		return
	}
	simplified := andConjuncts(simplifiedConjunction)

	call.Yield(sel.WithPredicates(simplified))
}

var _ ExpressionRule = (*QueryPredicateSimplificationRule)(nil)
