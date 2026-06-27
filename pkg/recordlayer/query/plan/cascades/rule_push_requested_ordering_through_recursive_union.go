package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughRecursiveUnionRule is a PLANNING-phase
// ImplementationRule that pushes a RequestedOrdering constraint through
// a RecursiveUnionExpression. Currently, it only allows pushing
// preserve-type orderings to both the initial and recursive legs.
//
// Ports Java's PushRequestedOrderingThroughRecursiveUnionRule.
type PushRequestedOrderingThroughRecursiveUnionRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughRecursiveUnionRule() *PushRequestedOrderingThroughRecursiveUnionRule {
	return &PushRequestedOrderingThroughRecursiveUnionRule{
		matcher: NewExpressionMatcher[*expressions.RecursiveUnionExpression]("push_req_ord_recursive_union"),
	}
}

func (r *PushRequestedOrderingThroughRecursiveUnionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughRecursiveUnionRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	// Push only the preserve ordering (if any). Java filters the
	// stream for RequestedOrdering.isPreserve() and takes the first.
	var preserve *RequestedOrdering
	for _, o := range orderings {
		if o.IsPreserve() {
			preserve = o
			break
		}
	}
	if preserve == nil {
		return
	}

	toBePushed := []*RequestedOrdering{preserve}

	ru := call.Bindings.Get(r.matcher).(*expressions.RecursiveUnionExpression)

	// Push to both legs: initial (index 0) and recursive (index 1).
	initialRef := ru.GetInitialState().GetRangesOver()
	if initialRef != nil {
		call.PushConstraint(initialRef, toBePushed)
	}
	recursiveRef := ru.GetRecursiveState().GetRangesOver()
	if recursiveRef != nil {
		call.PushConstraint(recursiveRef, toBePushed)
	}
}

var _ ImplementationRule = (*PushRequestedOrderingThroughRecursiveUnionRule)(nil)
