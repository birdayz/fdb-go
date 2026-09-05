package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushRequestedOrderingThroughProjectionRule is a PLANNING-phase
// ImplementationRule that translates a RequestedOrdering constraint
// through a LogicalProjectionExpression's value mapping and pushes the
// translated ordering to the child Reference.
//
// Projection is NOT ordering-transparent — sort keys reference
// post-projection column aliases which must be translated to their
// pre-projection source expressions via PushDownThroughValue. If all
// keys translate, the translated ordering is pushed to the child; if
// any key fails to translate, nothing is pushed.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// projection case).
type PushRequestedOrderingThroughProjectionRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughProjectionRule() *PushRequestedOrderingThroughProjectionRule {
	return &PushRequestedOrderingThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalProjectionExpression]("push_requested_ordering_through_projection"),
	}
}

func (r *PushRequestedOrderingThroughProjectionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughProjectionRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	proj := call.Bindings.Get(r.matcher).(*expressions.LogicalProjectionExpression)

	innerRef := proj.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	// The crossing is requestedOrderingBelow — the translation the
	// satisfaction walk uses over the PHYSICAL projection, through the
	// projection's OWN result value (the only row description that cannot
	// drift from what it emits) — so the constraint pushed to the child and
	// the request judged against the child's members are the same value, in
	// the child's current-row space.
	var translated []*properties.RequestedOrdering
	for _, reqOrd := range orderings {
		// A constraint on this Reference is stated in its current-row space —
		// every pusher that reshapes a request rebases it through
		// requestedOrderingAtInnerCurrent before storing it, and a pass-through
		// pusher hands on what it received — so a request rooted anywhere else
		// cannot be this projection's output, and it is LOUD: silently skipping
		// it would drop the ordering the way an un-rebased sort key once did
		// (see requestedOrderingAtInnerCurrent), and pushing it would ask the
		// child for a foreign order — a same-shaped sibling's output is one such
		// root. The message states the fact, not a diagnosis: a select pusher
		// passes a non-local (outer) key through untranslated, so an outer
		// root can arrive here without any pusher having erred.
		root, ok := requestedOrderingRoot(reqOrd)
		if !ok {
			// No single row to push through — a part with no correlation (a
			// literal, a parameter) or parts over two roots. Not a pusher's
			// defect; the request is not expressible below this projection.
			continue
		}
		if root != values.CurrentCorrelation() {
			call.Fail(fmt.Errorf("requested ordering on a projection is rooted at %v, not at the projection's current row", root))
			return
		}
		// A part the result value cannot express drops the whole request:
		// a partial constraint would ask the child for an order the
		// projection cannot restate.
		if below, ok := requestedOrderingBelow(proj, reqOrd); ok && !below.IsPreserve() {
			translated = append(translated, below)
		}
	}

	if len(translated) > 0 {
		call.PushConstraint(innerRef, translated)
	}
}

var _ ImplementationRule = (*PushRequestedOrderingThroughProjectionRule)(nil)
