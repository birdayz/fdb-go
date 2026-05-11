package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughSelectExistentialRule is a PLANNING-phase
// ImplementationRule that pushes a preserve RequestedOrdering to the
// child of an existential quantifier within a SelectExpression.
//
// Existential quantifiers (EXISTS subqueries) don't need specific
// orderings — they only check for the existence of at least one row.
// This rule unconditionally pushes preserve ordering to their child
// Reference so downstream rules know the ordering is unconstrained.
//
// Ports Java's PushRequestedOrderingThroughSelectExistentialRule.
type PushRequestedOrderingThroughSelectExistentialRule struct {
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughSelectExistentialRule() *PushRequestedOrderingThroughSelectExistentialRule {
	return &PushRequestedOrderingThroughSelectExistentialRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("push_req_ord_select_existential"),
	}
}

func (r *PushRequestedOrderingThroughSelectExistentialRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughSelectExistentialRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	sel := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	// Find the first existential quantifier. Java's matcher structure
	// binds exactly one existential; we iterate the quantifiers.
	for _, q := range sel.GetQuantifiers() {
		if q.Kind() != expressions.QuantifierExistential {
			continue
		}
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		// Push preserve ordering — existential children are
		// unconstrained in ordering.
		call.PushConstraint(childRef, []*RequestedOrdering{PreserveOrdering()})
	}
}

var _ ImplementationRule = (*PushRequestedOrderingThroughSelectExistentialRule)(nil)
