package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughTempTableInsertRule is a PLANNING-phase
// ImplementationRule that propagates a RequestedOrdering constraint
// through a TempTableInsertExpression. TempTableInsert passes through
// rows into a temp table -- sorting below produces the same final
// order as sorting above -- so the requested ordering passes through
// unchanged to the child Reference.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op -- ImplementTempTableInsertRule
// handles the actual insert implementation.
//
// Ports Java's PushRequestedOrderingThroughInsertTempTableRule.
type PushRequestedOrderingThroughTempTableInsertRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughTempTableInsertRule() *PushRequestedOrderingThroughTempTableInsertRule {
	return &PushRequestedOrderingThroughTempTableInsertRule{
		matcher: NewExpressionMatcher[*expressions.TempTableInsertExpression]("push_requested_ordering_through_temp_table_insert"),
	}
}

func (r *PushRequestedOrderingThroughTempTableInsertRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughTempTableInsertRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	tti := call.Bindings.Get(r.matcher).(*expressions.TempTableInsertExpression)
	innerRef := tti.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	call.PushConstraint(innerRef, orderings)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughTempTableInsertRule)(nil)
