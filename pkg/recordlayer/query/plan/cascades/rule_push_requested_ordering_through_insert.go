package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
		matcher: NewExpressionMatcher[*expressions.InsertExpression]("push_requested_ordering_through_insert"),
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

	flowed, err := ins.GetInner().RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	call.PushConstraint(innerRef, pushDMLRequestedOrderingsThroughValue(
		orderings, flowed, ins.GetInner().GetAlias()))
}

// pushDMLRequestedOrderingsThroughValue states DML requirements in the input
// row's exact coordinates. Java's three DML propagation rules all call
// RequestedOrdering.pushDown; treating them as opaque pass-through operators
// was only harmless while their ordering keys were unrooted name carriers.
//
// PushDownThroughValue performs the structural projection. The correlation
// filter mirrors Java's final current-scope check: a computation-only value
// (UPDATE's post-mutation NEW object) is not an ordering the input can provide.
func pushDMLRequestedOrderingsThroughValue(
	orderings []*properties.RequestedOrdering,
	resultValue values.Value,
	inputAlias values.CorrelationIdentifier,
) []*properties.RequestedOrdering {
	pushedOrderings := make([]*properties.RequestedOrdering, 0, len(orderings))
	for _, ordering := range orderings {
		if ordering.IsPreserve() {
			pushedOrderings = append(pushedOrderings, ordering)
			continue
		}
		pushed := ordering.PushDownThroughValue(resultValue, inputAlias)
		if pushed.IsPreserve() {
			pushedOrderings = append(pushedOrderings, pushed)
			continue
		}
		parts := make([]properties.RequestedOrderingPart, 0, pushed.Size())
		for _, part := range pushed.GetParts() {
			inputScoped := true
			for correlation := range values.GetCorrelatedToOfValue(part.Value) {
				if correlation != inputAlias {
					inputScoped = false
					break
				}
			}
			if inputScoped {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			pushedOrderings = append(pushedOrderings, properties.PreserveOrdering())
			continue
		}
		pushedOrderings = append(pushedOrderings, properties.NewRequestedOrdering(
			parts, properties.DistinctnessPreserveDistinctness, ordering.IsExhaustive()))
	}
	return pushedOrderings
}

var _ ImplementationRule = (*PushRequestedOrderingThroughInsertRule)(nil)
