package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughInsertRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through an InsertExpression. Insert passes through rows unchanged
// (it writes them to the store and emits them for downstream
// counting/projection), so the requested ordering passes through
// unchanged to the child Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op -- ImplementInsertRule handles
// the actual insert implementation.
//
// Ports Java's PushRequestedOrderingThroughInsertRule.
type PushRequestedOrderingThroughInsertRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughInsertRule() *PushRequestedOrderingThroughInsertRule {
	return &PushRequestedOrderingThroughInsertRule{
		matcher: &insertPushMatcher{},
	}
}

func (r *PushRequestedOrderingThroughInsertRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughInsertRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	ins := call.Bindings.Get(r.matcher).(*expressions.InsertExpression)
	innerRef := ins.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughInsertRule)(nil)

// insertPushMatcher matches InsertExpression for the constraint-push
// rule.
type insertPushMatcher struct{}

func (m *insertPushMatcher) RootType() string { return "InsertExpression" }

func (m *insertPushMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.InsertExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
