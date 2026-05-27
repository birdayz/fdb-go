package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughUnionRule is a PLANNING-phase
// ImplementationRule that pushes a RequestedOrdering constraint through
// a LogicalUnionExpression to all of its child branch References.
//
// For a union to produce ordered output, all branches must be
// independently ordered — a merge-union above combines the sorted
// streams. The first branch receives exhaustive orderings (all
// satisfying plans are needed to enumerate merge-compatible options);
// remaining branches receive the orderings as-is (they'll be
// specifically requested by the union implementation rule).
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op.
//
// Ports Java's PushRequestedOrderingThroughUnionRule.
type PushRequestedOrderingThroughUnionRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughUnionRule() *PushRequestedOrderingThroughUnionRule {
	return &PushRequestedOrderingThroughUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("push_requested_ordering_through_union"),
	}
}

func (r *PushRequestedOrderingThroughUnionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughUnionRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	union := call.Bindings.Get(r.matcher).(*expressions.LogicalUnionExpression)
	children := union.GetQuantifiers()
	if len(children) == 0 {
		return
	}

	// Build exhaustive versions of the orderings for the first branch.
	// Java: the first quantifier needs to produce all possible orderings;
	// the other ones get specifically requested by the union
	// implementation rule.
	exhaustive := make([]*RequestedOrdering, len(orderings))
	for i, o := range orderings {
		exhaustive[i] = o.Exhaustive()
	}

	for i, child := range children {
		childRef := child.GetRangesOver()
		if childRef == nil {
			continue
		}
		if i == 0 {
			call.PushConstraint(childRef, exhaustive)
		} else {
			call.PushConstraint(childRef, orderings)
		}
	}
}

var _ ImplementationRule = (*PushRequestedOrderingThroughUnionRule)(nil)
