// Portions derived from FoundationDB Record Layer (
// PushRequestedOrderingThroughDeleteRule.java),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughDeleteRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through a DeleteExpression. Delete passes through rows unchanged
// (it only removes them from the store), so the requested ordering
// passes through unchanged to the child Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op — ImplementDeleteRule handles
// the actual delete implementation.
//
// Ports Java's PushRequestedOrderingThroughDeleteRule.
type PushRequestedOrderingThroughDeleteRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughDeleteRule() *PushRequestedOrderingThroughDeleteRule {
	return &PushRequestedOrderingThroughDeleteRule{
		matcher: NewExpressionMatcher[*expressions.DeleteExpression]("push_requested_ordering_through_delete"),
	}
}

func (r *PushRequestedOrderingThroughDeleteRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

// ConstraintDependencies is Java's ImmutableSet.of(REQUESTED_ORDERING).
func (r *PushRequestedOrderingThroughDeleteRule) ConstraintDependencies() []any {
	return []any{RequestedOrderingConstraintKey}
}

func (r *PushRequestedOrderingThroughDeleteRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	d := call.Bindings.Get(r.matcher).(*expressions.DeleteExpression)
	innerRef := d.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughDeleteRule)(nil)
