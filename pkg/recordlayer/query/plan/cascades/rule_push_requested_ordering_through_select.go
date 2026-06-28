package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushRequestedOrderingThroughSelectRule is a PLANNING-phase
// ImplementationRule that pushes RequestedOrdering constraints through
// a SelectExpression with a single ForEach quantifier. Ordering parts
// are translated through the SELECT's result value (projection) so
// they reference the inner quantifier's columns.
//
// For preserve orderings, preserve is pushed through unchanged.
// For concrete orderings, PushDownThroughValue translates each
// ordering part through the projection.
//
// Ports Java's PushRequestedOrderingThroughSelectRule.
type PushRequestedOrderingThroughSelectRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughSelectRule() *PushRequestedOrderingThroughSelectRule {
	return &PushRequestedOrderingThroughSelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("push_req_ord_select"),
	}
}

func (r *PushRequestedOrderingThroughSelectRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughSelectRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	sel := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	// Find the first ForEach quantifier. Java's matcher uses
	// forEachQuantifierOverRef to bind exactly one.
	var innerQuantifier *expressions.Quantifier
	for i := range sel.GetQuantifiers() {
		q := &sel.GetQuantifiers()[i]
		if q.Kind() == expressions.QuantifierForEach {
			innerQuantifier = q
			break
		}
	}
	if innerQuantifier == nil {
		return
	}

	lowerRef := innerQuantifier.GetRangesOver()
	if lowerRef == nil {
		return
	}

	orderings := call.GetRequestedOrderings()
	// Java: orElse(ImmutableSet.of()) — empty set means push nothing,
	// but we still proceed so that an empty push lands on the child.

	resultValue := sel.GetResultValue()
	var toBePushed []*RequestedOrdering
	for _, o := range orderings {
		if o.IsPreserve() {
			toBePushed = append(toBePushed, PreserveOrdering())
		} else {
			toBePushed = append(toBePushed, o.PushDownThroughValue(resultValue, innerQuantifier.GetAlias()))
		}
	}

	call.PushConstraint(lowerRef, toBePushed)
}

var _ ImplementationRule = (*PushRequestedOrderingThroughSelectRule)(nil)
