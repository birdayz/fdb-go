package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughUpdateRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through an UpdateExpression. Update passes through rows unchanged
// (it applies transforms and emits them for downstream
// counting/projection), so the requested ordering passes through
// unchanged to the child Reference.
//
// Java's version translates orderings through makeComputationValue
// (a RecordConstructor with old/new columns). Go's UpdateExpression
// does not yet model that structure -- GetResultValue returns the
// inner's flowed object value directly -- so the pass-through is
// correct for Go's current expression model.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op -- ImplementUpdateRule handles
// the actual update implementation.
//
// Ports Java's PushRequestedOrderingThroughUpdateRule.
type PushRequestedOrderingThroughUpdateRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughUpdateRule() *PushRequestedOrderingThroughUpdateRule {
	return &PushRequestedOrderingThroughUpdateRule{
		matcher: &updatePushMatcher{},
	}
}

func (r *PushRequestedOrderingThroughUpdateRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughUpdateRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	upd := call.Bindings.Get(r.matcher).(*expressions.UpdateExpression)
	innerRef := upd.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughUpdateRule)(nil)

// updatePushMatcher matches UpdateExpression for the constraint-push
// rule.
type updatePushMatcher struct{}

func (m *updatePushMatcher) RootType() string { return "UpdateExpression" }

func (m *updatePushMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.UpdateExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
