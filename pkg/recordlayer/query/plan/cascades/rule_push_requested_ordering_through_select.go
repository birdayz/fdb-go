package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushRequestedOrderingThroughSelectRule is a PLANNING-phase
// ImplementationRule that pushes RequestedOrdering constraints through
// a SelectExpression to every ForEach quantifier. Ordering parts are
// translated through the SELECT's result value (projection) so each child
// receives only the parts that can be expressed in that child's value space.
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

	orderings := call.GetRequestedOrderings()
	// Java: orElse(ImmutableSet.of()) — empty set means push nothing,
	// but we still visit every ForEach child.

	resultValue := sel.GetResultValue()
	localAliases := make(map[values.CorrelationIdentifier]struct{}, len(sel.GetQuantifiers()))
	for _, quantifier := range sel.GetQuantifiers() {
		localAliases[quantifier.GetAlias()] = struct{}{}
	}
	firstForEach := true
	for _, innerQuantifier := range sel.GetQuantifiers() {
		if innerQuantifier.Kind() != expressions.QuantifierForEach {
			continue
		}
		isFirstForEach := firstForEach
		firstForEach = false
		lowerRef := innerQuantifier.GetRangesOver()
		if lowerRef == nil {
			continue
		}

		var (
			toBePushed  []*properties.RequestedOrdering
			hasConcrete bool
		)
		for _, o := range orderings {
			if o.IsPreserve() {
				toBePushed = append(toBePushed, properties.PreserveOrdering())
			} else {
				pushed := pushRequestedOrderingToSelectChild(
					o, resultValue, innerQuantifier.GetAlias(), localAliases)
				toBePushed = append(toBePushed, pushed)
				hasConcrete = hasConcrete || !pushed.IsPreserve()
			}
		}
		// A concrete parent request that has no values in this child's space
		// pushes Preserve in Java. In Go, absence already has precisely that
		// meaning, while storing the explicit no-op re-arms and re-explores the
		// child group. Skip the no-op batch; every child with at least one
		// translated ordering part still receives its concrete requirement.
		// Retain the pre-190.6 first-ForEach preserve push so the existing
		// constraint-watermark scheduling and task budgets are unchanged.
		// Additional non-contributing legs are the new no-op work to suppress.
		if !hasConcrete && !isFirstForEach {
			continue
		}
		call.PushConstraint(lowerRef, toBePushed)
	}
}

// pushRequestedOrderingToSelectChild translates a SELECT-output request into
// one child's value space and removes parts owned by sibling quantifiers.
// Value.PushDownThroughValue handles the projection shape; this local-alias
// filter supplies Java's alias-map/constant-alias discipline that the direct Go
// value algorithm does not otherwise carry.
func pushRequestedOrderingToSelectChild(
	ordering *properties.RequestedOrdering,
	resultValue values.Value,
	childAlias values.CorrelationIdentifier,
	localAliases map[values.CorrelationIdentifier]struct{},
) *properties.RequestedOrdering {
	if ordering == nil || ordering.IsPreserve() {
		return properties.PreserveOrdering()
	}
	pushed := ordering.PushDownThroughValue(resultValue, childAlias)
	if pushed.IsPreserve() {
		return pushed
	}

	parts := make([]properties.RequestedOrderingPart, 0, len(pushed.GetParts()))
	for _, part := range pushed.GetParts() {
		correlations := values.GetCorrelatedToOfValue(part.Value)
		ownedByChild := false
		ownedBySibling := false
		for alias := range correlations {
			if _, isLocal := localAliases[alias]; !isLocal {
				continue
			}
			if alias == childAlias {
				ownedByChild = true
			} else {
				ownedBySibling = true
			}
		}
		if ownedBySibling {
			continue
		}
		if len(localAliases) > 1 && !ownedByChild {
			// A correlation-free field in a multi-child SELECT has no
			// defensible owner. Decline it rather than ask every leg for a
			// same-named index and risk proving order on the wrong source.
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return properties.PreserveOrdering()
	}
	return properties.NewRequestedOrdering(
		parts, pushed.GetDistinctness(), pushed.IsExhaustive())
}

var _ ImplementationRule = (*PushRequestedOrderingThroughSelectRule)(nil)
