// Portions derived from FoundationDB Record Layer (
// PushRequestedOrderingThroughSelectRule.java, RequestedOrdering.java),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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

func (r *PushRequestedOrderingThroughSelectRule) hasConstraintEffect(cm *ConstraintMap, ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	orderings, _ := Get(cm, ref, RequestedOrderingConstraintKey)
	pushes, err := selectOrderingPushes(expr.(*expressions.SelectExpression), orderings)
	if err != nil {
		// Keep the ordinary rule task responsible for reporting translation errors.
		return true
	}
	for _, push := range pushes {
		current, present := Get(cm, push.ref, RequestedOrderingConstraintKey)
		if !present {
			return true
		}
		if _, changed := properties.CombineRequestedOrderings(current, push.orderings); changed {
			return true
		}
	}
	return false
}

// ConstraintDependencies is Java's ImmutableSet.of(REQUESTED_ORDERING).
func (r *PushRequestedOrderingThroughSelectRule) ConstraintDependencies() []any {
	return []any{RequestedOrderingConstraintKey}
}

func (r *PushRequestedOrderingThroughSelectRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	sel := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)
	pushes, err := selectOrderingPushes(sel, call.GetRequestedOrderings())
	if err != nil {
		call.Fail(err)
		return
	}
	for _, push := range pushes {
		call.PushConstraint(push.ref, push.orderings)
	}
}

func selectOrderingPushes(sel *expressions.SelectExpression, orderings []*properties.RequestedOrdering) ([]pendingRequestedOrderingConstraint, error) {
	var pushes []pendingRequestedOrderingConstraint
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
				// Java's pushDown finishes with rebase(childAlias -> current) and
				// keeps only the parts that land entirely in current space
				// (RequestedOrdering.java:220-232). The child-space result above
				// is the half before that rebase; a constraint attached to the
				// child REFERENCE is read there in the child's own current-row
				// space, so it has to cross.
				pushed, err := requestedOrderingAtInnerCurrent(
					pushRequestedOrderingToSelectChild(
						o, resultValue, innerQuantifier.GetAlias(), localAliases),
					innerQuantifier)
				if err != nil {
					return nil, err
				}
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
		pushes = append(pushes, pendingRequestedOrderingConstraint{ref: lowerRef, orderings: toBePushed})
	}
	return pushes, nil
}

// pushRequestedOrderingToSelectChild translates a SELECT-output request into
// one child's value space and removes parts owned by sibling quantifiers.
// Value.PushDownThroughValue handles the projection shape; this local-alias
// filter supplies Java's alias-map/constant-alias discipline that the direct Go
// value algorithm does not otherwise carry.
//
// The request arrives expressed over the SELECT's own output row, which is the
// reserved-current handle — Java passes Quantifier.current() as the upper base
// of the same push-down (RequestedOrdering.pushDown, called with
// Quantifier.current()). Passing the CHILD's alias there instead asks the
// push-down to interpret the request in the space it is trying to reach, so
// every part declines and the whole request degrades to Preserve.
func pushRequestedOrderingToSelectChild(
	ordering *properties.RequestedOrdering,
	resultValue values.Value,
	childAlias values.CorrelationIdentifier,
	localAliases map[values.CorrelationIdentifier]struct{},
) *properties.RequestedOrdering {
	return pushRequestedOrderingToSelectChildThroughOutput(
		ordering, resultValue, values.CurrentCorrelation(), childAlias, localAliases)
}

// pushRequestedOrderingToSelectChildThroughOutput separates the alias that
// owns the producer's OUTPUT row from the child whose retained fields may
// satisfy the request. They are normally the same logical SELECT alias. A
// physical FlatMap at an enclosing Sort is different: the Sort names the
// FlatMap's exact current-row carrier, while the result program still names
// the outer and inner runtime bindings. Push-down must therefore interpret
// output ordinals in current-row space, then use childAlias only to reject
// fields owned by the sibling leg.
func pushRequestedOrderingToSelectChildThroughOutput(
	ordering *properties.RequestedOrdering,
	resultValue values.Value,
	outputAlias values.CorrelationIdentifier,
	childAlias values.CorrelationIdentifier,
	localAliases map[values.CorrelationIdentifier]struct{},
) *properties.RequestedOrdering {
	if ordering == nil || ordering.IsPreserve() {
		return properties.PreserveOrdering()
	}
	pushed := ordering.PushDownThroughValue(resultValue, outputAlias)
	if pushed.IsPreserve() {
		return pushed
	}

	// When the SELECT's result value IS this child's row, every output column
	// is that child's column, so a correlation-free ordering value has exactly
	// one possible owner. Java never faces the question: its pushDown yields
	// values correlated to the child and keeps them (an empty correlation set
	// passes its `allMatch(current)` test vacuously). Go's SQL translator bakes
	// sort keys as positional field reads carrying no correlation at all, so
	// ownership has to be recovered from the result value instead.
	//
	// Without this, an IN-like SELECT (explode quantifiers plus the inner,
	// result = the inner's row) downgraded its parent's concrete ordering
	// request to Preserve. The Preserve then joined the concrete request in the
	// base reference's constraint set, every data access there resolved to BOTH
	// scan directions, and BOTH yields a forward scan only — so no descending
	// access path below an IN was ever enumerated.
	resultIsChildRow := false
	if qov, isQOV := values.AsQuantifiedObjectValue(resultValue); isQOV {
		resultIsChildRow = qov.Correlation() == childAlias
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
		if len(localAliases) > 1 && !ownedByChild && !resultIsChildRow {
			// A correlation-free field in a multi-child SELECT whose result
			// composes several legs has no defensible owner. Decline it rather
			// than ask every leg for a same-named index and risk proving order
			// on the wrong source.
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return properties.PreserveOrdering()
	}
	return properties.NewRequestedOrdering(
		parts, pushed.GetDistinctness(), pushed.IsExhaustive()).CarrySortable(pushed)
}

var (
	_ ImplementationRule        = (*PushRequestedOrderingThroughSelectRule)(nil)
	_ constraintPropagationRule = (*PushRequestedOrderingThroughSelectRule)(nil)
)
