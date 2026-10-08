package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// FilterToLogicalUnionRule adapts Go's shape-preserving filter to Java's
// Select-based union exploration. The adapter is ephemeral: publishing the
// equivalent Select in the memo would rematch every ordinary filter's access
// paths without introducing a new execution alternative.
type FilterToLogicalUnionRule struct{ matcher matching.BindingMatcher }

func NewFilterToLogicalUnionRule() *FilterToLogicalUnionRule {
	return &FilterToLogicalUnionRule{matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("filter_to_logical_union")}
}
func (r *FilterToLogicalUnionRule) Matcher() matching.BindingMatcher { return r.matcher }
func (r *FilterToLogicalUnionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	q := f.GetInner()
	// Compensation filters already range over physical plans. They do not
	// introduce a new logical access path and must not restart exploration.
	logicalInput := false
	for _, member := range q.GetRangesOver().Members() {
		if !isPhysical(member) {
			logicalInput = true
			break
		}
	}
	if !logicalInput {
		return
	}
	meaningful := false
	for _, pred := range f.GetPredicates() {
		if !predicates.IsTautology(pred) {
			meaningful = true
			break
		}
	}
	if !meaningful {
		return
	}
	result, err := q.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	conjuncts := predicates.FlattenConjunction(f.GetPredicates())
	sel, err := expressions.NewSelectExpression(result, []expressions.Quantifier{q}, conjuncts)
	if err != nil {
		call.Fail(err)
		return
	}
	explorePredicateUnion(call, sel, partiallyMatchedOrPredicates(call.Reference, f))
}
