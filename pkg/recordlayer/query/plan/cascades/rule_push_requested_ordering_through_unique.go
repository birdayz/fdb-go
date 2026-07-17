package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughUniqueRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through a LogicalUniqueExpression. Unique (PK deduplication)
// preserves row order, so the requested ordering passes through
// unchanged to the child Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op — ImplementUniqueRule handles
// the actual unique implementation.
//
// Ports Java's PushRequestedOrderingThroughUniqueRule.
type PushRequestedOrderingThroughUniqueRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughUniqueRule() *PushRequestedOrderingThroughUniqueRule {
	return &PushRequestedOrderingThroughUniqueRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUniqueExpression]("push_requested_ordering_through_unique"),
	}
}

func (r *PushRequestedOrderingThroughUniqueRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughUniqueRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	u := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)
	innerRef := u.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughUniqueRule)(nil)
