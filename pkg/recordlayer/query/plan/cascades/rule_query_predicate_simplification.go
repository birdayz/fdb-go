package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// QueryPredicateSimplificationRule applies predicate simplification to
// a SelectExpression. Runs the predicate simplifier
// (predicates.SimplifyPredicateValues) on each predicate in the
// SelectExpression's predicate list and yields a new SelectExpression
// if any predicate changed.
//
// Go's simplifier engine (SimplifyPredicateValues) walks the predicate
// tree and constant-folds Value operands — e.g. `name = 1+2` becomes
// `name = 3`. This is the Go equivalent of Java's
// ConstantFoldingRuleSet applied through Simplification.optimize.
//
// Convergence: if no predicates changed (pointer-identity check), the
// rule does not yield. Constant-folding is idempotent, so repeated
// application converges in one step.
//
// Ports Java's QueryPredicateSimplificationRule (ExplorationCascadesRule, 128 LOC).
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

	// Simplify each predicate. Track whether anything changed via
	// pointer identity (SimplifyPredicateValues returns the same
	// pointer when nothing folds).
	anyChanged := false
	simplified := make([]predicates.QueryPredicate, len(originalPredicates))
	for i, p := range originalPredicates {
		simplified[i] = predicates.SimplifyPredicateValues(p)
		if simplified[i] != p {
			anyChanged = true
		}
	}

	if !anyChanged {
		return
	}

	newSel := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		sel.GetQuantifiers(),
		simplified,
		sel.GetSourceAliases(),
		sel.GetJoinType(),
	)
	call.Yield(newSel)
}

var _ ExpressionRule = (*QueryPredicateSimplificationRule)(nil)
