package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughFilterRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through a LogicalFilterExpression. Filtering removes rows but does
// not reorder them, so the requested ordering passes through unchanged
// to the child Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op — ImplementFilterRule /
// ImplementSimpleSelectRule handle the actual filter implementation.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// single-source filter case — Go's LogicalFilterExpression maps to
// Java's SelectExpression with a single ForEach quantifier and
// predicates only).
type PushRequestedOrderingThroughFilterRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughFilterRule() *PushRequestedOrderingThroughFilterRule {
	return &PushRequestedOrderingThroughFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("push_requested_ordering_through_filter"),
	}
}

func (r *PushRequestedOrderingThroughFilterRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughFilterRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	f := call.Bindings.Get(r.matcher).(*expressions.LogicalFilterExpression)
	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughFilterRule)(nil)
