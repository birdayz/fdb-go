package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughSortRule is a PLANNING-phase
// ImplementationRule that reads a LogicalSortExpression's sort keys
// and pushes them as a RequestedOrdering constraint to the child
// Reference. The sort expression is the SOURCE of ordering constraints
// — this rule creates the initial constraint that transparent rules
// (Distinct, Unique, Delete) then propagate further.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op — ImplementSortRule handles
// the actual sort elimination / implementation.
//
// Ports Java's PushRequestedOrderingThroughSortRule.
type PushRequestedOrderingThroughSortRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughSortRule() *PushRequestedOrderingThroughSortRule {
	return &PushRequestedOrderingThroughSortRule{
		matcher: &logicalSortPushMatcher{},
	}
}

func (r *PushRequestedOrderingThroughSortRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughSortRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	s := call.Bindings.Get(r.matcher).(*expressions.LogicalSortExpression)
	if s.IsUnsorted() {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	requestedOrdering := sortExpressionToRequestedOrdering(s)
	call.PushConstraint(innerRef, []*RequestedOrdering{requestedOrdering})
}

var _ ImplementationRule = (*PushRequestedOrderingThroughSortRule)(nil)

// logicalSortPushMatcher matches LogicalSortExpression for the
// constraint-push rule. Separate from logicalSortMatcher used by
// ImplementSortRule to avoid matcher identity collisions.
type logicalSortPushMatcher struct{}

func (m *logicalSortPushMatcher) RootType() string { return "LogicalSortExpression" }

func (m *logicalSortPushMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalSortExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
