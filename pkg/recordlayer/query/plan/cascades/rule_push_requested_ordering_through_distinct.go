package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughDistinctRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through a LogicalDistinctExpression. Distinct preserves row order,
// so the requested ordering passes through unchanged to the child
// Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op — ImplementDistinctFinalRule
// handles the actual distinct implementation.
//
// Ports Java's PushRequestedOrderingThroughDistinctRule.
type PushRequestedOrderingThroughDistinctRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughDistinctRule() *PushRequestedOrderingThroughDistinctRule {
	return &PushRequestedOrderingThroughDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("push_requested_ordering_through_distinct"),
	}
}

func (r *PushRequestedOrderingThroughDistinctRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughDistinctRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	innerRef := d.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughDistinctRule)(nil)
