package cascades

import (
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

	// The projection's OWN result value, not a second construction of it.
	// The crossing itself is requestedOrderingBelow — the translation the
	// satisfaction walk uses over the PHYSICAL projection — so the constraint
	// pushed to the child and the request judged against the child's members
	// are the same value. This rule used to push through the projection's
	// result value with the INNER quantifier's alias as the upper alias and
	// without the rebase into the child's current-row space; a constraint
	// rooted at the projection's current (which is how every constraint
	// arrives) then failed the push-down's root check and nothing was pushed,
	// so `ORDER BY u.g` over `(SELECT g FROM t) u` never reached the index on
	// g and `ORDER BY u.id DESC` never produced a reverse scan.
	//
	// This rule used to rebuild the RecordConstructorValue from the projected
	// values and aliases, with its own naming rule — upper-folded, and
	// ExplainValue for an unaliased slot. Both halves disagreed with the
	// projection's actual naming authority (OutputColumnName, which folds
	// nothing and renders an unaliased slot ordinal-free), so the requested
	// ordering was translated through a record whose field names did not
	// match the ones the reference names. It dropped and the ordering was
	// never pushed. Asking the expression for the row it produces is the
	// only construction that cannot drift from it.
	var translated []*properties.RequestedOrdering
	for _, reqOrd := range orderings {
		// A constraint on this Reference is stated in its current-row space —
		// every pusher rebases through requestedOrderingAtInnerCurrent before
		// storing it — so a request rooted anywhere else is not this group's
		// constraint and fails closed: a same-shaped sibling's output is one
		// such root, and pushing it would ask the child for a foreign order.
		if root, ok := requestedOrderingRoot(reqOrd); !ok || root != values.CurrentCorrelation() {
			continue
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
