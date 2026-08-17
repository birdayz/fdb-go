package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_requested_ordering_through_sort"),
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

	requestedOrdering, err := requestedOrderingAtInnerCurrent(
		sortExpressionToRequestedOrdering(s), s.GetInner())
	if err != nil {
		call.Fail(err)
		return
	}
	call.PushConstraint(innerRef, []*properties.RequestedOrdering{requestedOrdering})
}

// requestedOrderingAtInnerCurrent moves a sort's ordering values from the inner
// quantifier's alias onto the reserved-current handle for the row that
// quantifier delivers — Java's
//
//	AliasMap.ofAliases(innerQuantifier.getAlias(), Quantifier.current())
//
// applied to every ordering part before the constraint is pushed
// (PushRequestedOrderingThroughSortRule.java:77-85). A requested ordering is
// attached to the CHILD reference, and everything that reads it there —
// push-down through a select/filter, an index candidate's satisfaction check —
// interprets its values in that reference's own current-row space. A part still
// rooted at the sort's alias for the child names a correlation nothing below the
// sort has ever heard of.
//
// The cost of skipping this is not a wrong ordering; it is a SILENTLY DROPPED
// one. Push-down through the select below cannot express a T-rooted value over
// that select's result, so it declines and returns Preserve — and a Preserve
// request is satisfied by every access path, so every index becomes a viable
// zero-prefix full scan. On the stress suite's `WHERE cat IN (...) ORDER BY id`
// that turned one useful candidate into three and tripled the planner's task
// count. It stayed invisible until FieldValues carried an exact root: an
// unrooted sort key rebased to nothing and pushed down through anything.
func requestedOrderingAtInnerCurrent(
	requested *properties.RequestedOrdering,
	inner expressions.Quantifier,
) (*properties.RequestedOrdering, error) {
	if requested == nil || requested.IsPreserve() {
		return requested, nil
	}
	edge, err := inner.RequireFlowedObjectValue()
	if err != nil {
		// A quantifier that cannot state a flowed object value has no exact row
		// phase to name, so there is no alias to rebase away from.
		return requested, nil //nolint:nilerr // the error IS the "nothing to rebase" answer
	}
	target, err := values.CurrentPhaseCarrierForEdge(edge)
	if err != nil {
		return nil, err
	}
	parts := requested.GetParts()
	rebased := make([]properties.RequestedOrderingPart, len(parts))
	for i, part := range parts {
		value, err := values.TranslateDeclaredEdgeRoot(part.Value, edge, target)
		if err != nil {
			return nil, err
		}
		rebased[i] = properties.RequestedOrderingPart{Value: value, SortOrder: part.SortOrder}
	}
	return properties.NewRequestedOrdering(rebased, requested.GetDistinctness(), false), nil
}

var _ ImplementationRule = (*PushRequestedOrderingThroughSortRule)(nil)
