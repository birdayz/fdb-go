package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementExplodeRule converts an ExplodeExpression into a physical
// RecordQueryExplodePlan. Direct translation — the collection Value
// passes through unchanged.
//
// Mirrors Java's ImplementExplodeRule.
type ImplementExplodeRule struct {
	matcher matching.BindingMatcher
}

func NewImplementExplodeRule() *ImplementExplodeRule {
	return &ImplementExplodeRule{
		matcher: NewExpressionMatcher[*expressions.ExplodeExpression]("explode"),
	}
}

func (r *ImplementExplodeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementExplodeRule) OnMatch(call *ExpressionRuleCall) {
	explode := matching.Get[*expressions.ExplodeExpression](call.Bindings, r.matcher)
	plan := plans.NewRecordQueryExplodePlanWithOrdinality(
		explode.GetCollectionValue(), explode.GetWithOrdinality())
	call.Yield(newPhysicalExplodeWrapper(plan))
}

var _ ExpressionRule = (*ImplementExplodeRule)(nil)
