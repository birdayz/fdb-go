package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushRequestedOrderingThroughUpdateRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through an UpdateExpression. Update passes through rows unchanged
// (it applies transforms and emits them for downstream
// counting/projection), so the requested ordering passes through
// unchanged to the child Reference.
//
// Like Java, concrete orderings are translated through the OLD/NEW computation
// row. Only OLD fields can become input-provided ordering keys; NEW is bound
// after mutation and therefore reduces to a preserve request at this boundary.
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
		matcher: NewExpressionMatcher[*expressions.UpdateExpression]("push_requested_ordering_through_update"),
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

	oldValue, err := upd.GetInner().RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	// The NEW object is a computation-time binding, deliberately distinct from
	// the input edge. pushDMLRequestedOrderingsThroughValue filters it after the
	// structural push-down, matching Java's constant/correlation scope gate.
	computationValue := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "OLD", Value: oldValue},
		values.RecordConstructorField{
			Name: "NEW",
			Value: values.NewObjectValue(
				values.UniqueCorrelationIdentifier(), upd.GetTargetType()),
		},
	)
	call.PushConstraint(innerRef, pushDMLRequestedOrderingsThroughValue(
		orderings, computationValue, upd.GetInner().GetAlias()))
}

var _ ImplementationRule = (*PushRequestedOrderingThroughUpdateRule)(nil)
