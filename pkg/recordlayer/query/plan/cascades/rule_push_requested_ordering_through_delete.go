package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
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
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughDeleteRule() *PushRequestedOrderingThroughDeleteRule {
	return &PushRequestedOrderingThroughDeleteRule{
		matcher: &deletePushMatcher{},
	}
}

func (r *PushRequestedOrderingThroughDeleteRule) Matcher() matching.BindingMatcher {
	return r.matcher
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

// deletePushMatcher matches DeleteExpression for the constraint-push
// rule.
type deletePushMatcher struct{}

func (m *deletePushMatcher) RootType() string { return "DeleteExpression" }

func (m *deletePushMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.DeleteExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
