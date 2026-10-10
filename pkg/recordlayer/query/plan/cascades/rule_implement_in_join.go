// Portions derived from FoundationDB Record Layer (ImplementInJoinRule.java,
// SortedInValuesSource.java, InSource.java, ConstantValue.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"context"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementInJoinRule implements a SELECT over ExplodeExpressions
// (UNNEST of IN-lists) and a correlated inner plan as a right-deep
// chain of RecordQueryInJoinPlans.
//
// Ports Java's ImplementInJoinRule. A requested ordering part that the inner
// plan's RichOrdering fixes through an equality binding correlated to an
// explode alias makes that explode a sorted IN-source, placed outermost in
// the InJoin chain so the chain delivers the requested order.
type ImplementInJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInJoinRule() *ImplementInJoinRule {
	return &ImplementInJoinRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("implement_in_join").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool { return len(sel.GetQuantifiers()) >= 2 }),
	}
}

func (r *ImplementInJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// ConstraintDependencies is Java's ImmutableSet.of(REQUESTED_ORDERING).
func (r *ImplementInJoinRule) ConstraintDependencies() []any {
	return []any{RequestedOrderingConstraintKey}
}

func (r *ImplementInJoinRule) OnMatch(call *ImplementationRuleCall) {
	if call.IsConstraintOnly() || call.CancellationErr() != nil {
		return
	}
	selectExpr := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	if selectExpr.HasPredicates() {
		return
	}

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) < 2 {
		return
	}
	// IN-join chains have no strict scalar compensation. A malformed/future
	// strict+Explode shape must stay unimplemented rather than bypass the sole
	// FirstOrDefault authority in ImplementNestedLoopJoinRule.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	resultValue := selectExpr.GetResultValue()

	var explodeQuantifiers []expressions.Quantifier
	var innerQuantifier expressions.Quantifier
	hasInner := false

	for _, q := range quantifiers {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		if explode := getExplodeExpression(ref); explode != nil {
			if !isSupportedExplodeValue(explode.GetCollectionValue()) {
				return
			}
			explodeQuantifiers = append(explodeQuantifiers, q)
		} else if !hasInner {
			innerQuantifier = q
			hasInner = true
		} else {
			return
		}
	}

	if !hasInner || len(explodeQuantifiers) == 0 {
		return
	}

	qov, ok := values.AsQuantifiedObjectValue(resultValue)
	if !ok || qov.Correlation() != innerQuantifier.GetAlias() {
		return
	}

	innerRef := innerQuantifier.GetRangesOver()
	if innerRef == nil {
		return
	}

	explodeAliasMap := make(map[values.CorrelationIdentifier]expressions.Quantifier, len(explodeQuantifiers))
	explodeAliases := make(map[values.CorrelationIdentifier]struct{}, len(explodeQuantifiers))
	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		explodeAliasMap[alias] = eq
		explodeAliases[alias] = struct{}{}
	}

	// ROLLED UP TO PropRichOrdering, and the roll-up is not tidiness — it is
	// what makes the partition-level read below TOTAL.
	//
	// The raw partition key carries only the REDUCED Ordering, so an
	// equality-bound index access and a residual filter over a full scan share
	// one partition: both advertise the same plain key sequence (the index's
	// bound prefix is dropped from it, leaving the primary-key suffix, which is
	// exactly what the scan advertises), while their RICH orderings differ
	// materially — A fixed-to-the-IN-binding versus A absent entirely.
	//
	// The rich binding is every property this rule reads: an explode alias
	// becomes a SORTED in-source only because it is correlated to a FIXED
	// binding. Reading the first member's rich ordering out of a mixed
	// partition therefore answers with whichever member the memo happened to
	// list first, and the claim it produces belongs to a plan that may not be
	// the one extraction picks — a `sorted` InJoin whose inner turns out to be
	// the unbounded scan is a false ordering claim, not just a lost
	// optimization. The sibling IN-union rule rolls up for the identical
	// reason; this one read the rich form while partitioning on the plain one.
	partitions := RollUpPlanPartitions(
		ToPlanPartitions(innerRef),
		properties.PropRichOrdering,
	)
	if len(partitions) == 0 {
		return
	}

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	} else {
		hasPreserve := false
		for _, ro := range requestedOrderings {
			if ro.IsPreserve() {
				hasPreserve = true
				break
			}
		}
		if !hasPreserve {
			requestedOrderings = append(requestedOrderings, properties.PreserveOrdering())
		}
	}

	for _, partition := range partitions {
		if call.CancellationErr() != nil {
			return
		}
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		// GetPhysicalExpressions, not GetExpressions: innerPlans below comes from
		// GetPlans, which FILTERS to physical members, while GetExpressions does
		// not. Seeding a FINAL reference from the unfiltered list could memoize a
		// non-physical member as a plan alternative, and the two lists are used
		// together here — one seeds the memo reference, the other drives the plan
		// chain.
		//
		// Latent, not live: instrumenting this site over the full 2407-query
		// corpus found ZERO partitions where the two lists differ, which is why
		// InJoin reports no unreachable edges. Fixed rather than left as a
		// comment because the helper exists precisely for this pairing and had no
		// caller.
		innerExprs := partition.GetPhysicalExpressions()

		for _, requestedOrdering := range requestedOrderings {
			if call.CancellationErr() != nil {
				return
			}
			allOrderings := r.enumerateSourceOrderingsForRequestedOrdering(
				call.RunContext, innerExprs, explodeQuantifiers, explodeAliases, explodeAliasMap,
				requestedOrdering)

			for _, orderedSources := range allOrderings {
				if call.CancellationErr() != nil {
					return
				}
				// Each innerPlans pass re-memoizes the SAME innerExprs group and
				// builds structurally-identical InJoins that dedup in the memo; the
				// specific member no longer seeds a plan snapshot (RFC-184 W2), so
				// only the iteration count is consulted here.
				for range innerPlans {
					if call.CancellationErr() != nil {
						return
					}
					currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)

					for i := len(orderedSources) - 1; i >= 0; i-- {
						if call.CancellationErr() != nil {
							return
						}
						source := orderedSources[i]
						inValues := extractInValues(source.quantifier)
						sorted := source.sorted
						var comparand values.Value
						if inValues == nil {
							// A runtime source cannot back a sorted claim.
							if comparand = inComparandOf(source.quantifier); comparand != nil {
								sorted = false
							}
						}
						if sorted && len(inValues) > 1 {
							// Back the "sorted" claim with actually-sorted values —
							// Java's SortedInValuesSource sorts in its constructor
							// (InSource.sortValues); this is the Go analog at the
							// point the claim is made. If the values genuinely can't
							// be totally ordered (an incomparable pair the planner's
							// type checking should have excluded), fall back to NOT
							// claiming an ordering rather than shipping a false one.
							if sv, ok := sortInJoinValues(inValues, source.reverse); ok {
								inValues = sv
							} else {
								sorted = false
							}
						}
						// The InJoin is its own cascades expression carrying the live
						// currentRef inner edge (RFC-184 W2); its per-ordering winner
						// resolves at extraction via ref.Winner(). No plan snapshot —
						// the deferred-winner case.
						inJoinPlan, err := plans.NewRecordQueryInJoinPlanFromQuantifierWithBindingAlias(
							expressions.NewPhysicalQuantifier(currentRef),
							source.bindingAlias, sorted, source.reverse)
						if err != nil {
							call.Fail(err)
							return
						}
						if inValues != nil {
							inJoinPlan = inJoinPlan.WithInValues(inValues)
						} else if comparand != nil {
							inJoinPlan = inJoinPlan.WithInComparand(comparand)
						}
						inJoinPlan = inJoinPlan.WithSourceKind(classifyInSourceKind(source.quantifier))
						currentRef = call.MemoizeFinalExpression(inJoinPlan)
					}

					for _, m := range currentRef.AllMembers() {
						if call.CancellationErr() != nil {
							return
						}
						if _, ok := m.(physicalPlanExpression); !ok {
							continue
						}
						call.YieldFinalExpression(m)
					}
				}
			}
		}
	}
}

type inJoinSource struct {
	bindingAlias values.CorrelationIdentifier
	sorted       bool
	reverse      bool
	quantifier   expressions.Quantifier
}

// enumerateSourceOrderingsForRequestedOrdering ports Java's
// ImplementInJoinRule.enumerateInSourcesForRequestedOrdering. A requested part
// the inner fixes by an explode's equality binding takes that explode as the
// next sorted outer source. Explodes no part claims follow unsorted in
// declaration order; only an exhaustive request enumerates their orders and
// directions. A chain that consumes every explode must satisfy the request.
func (r *ImplementInJoinRule) enumerateSourceOrderingsForRequestedOrdering(
	runCtx context.Context,
	innerExprs []expressions.RelationalExpression,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
	requestedOrdering *properties.RequestedOrdering,
) [][]inJoinSource {
	if runCtx != nil && runCtx.Err() != nil {
		return nil
	}
	// The FIRST physical member answers for the whole partition, and that is
	// only sound because the caller rolled its partitions up to
	// PropRichOrdering — see the roll-up in OnMatch. Members of such a
	// partition agree on every binding this function reads.
	innerOrdering := properties.EmptyOrdering()
	for _, expr := range innerExprs {
		if ph, ok := expr.(physicalPlanExpression); ok {
			if ro := computeWrapperRichOrdering(ph); ro != nil {
				innerOrdering = ro
			}
			break
		}
	}

	available := make(map[values.CorrelationIdentifier]struct{}, len(explodeAliases))
	for alias := range explodeAliases {
		available[alias] = struct{}{}
	}
	type outerPart struct {
		value     values.Value
		sortOrder properties.ProvidedSortOrder
	}
	type inJoinPrefix struct {
		sources []inJoinSource
		parts   []outerPart
	}
	prefixes := []inJoinPrefix{{}}
	consumed := make(map[string]struct{})

	var parts []properties.RequestedOrderingPart
	if requestedOrdering != nil && !requestedOrdering.IsPreserve() {
		parts = requestedOrdering.GetParts()
	}
	exhaustive := requestedOrdering != nil && requestedOrdering.IsExhaustive()
	for i := 0; i < len(parts) && len(available) > 0; i++ {
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		part := parts[i]
		key, bindings, ok := innerOrdering.BindingsFor(part.Value)
		if !ok || len(bindings) == 0 || properties.SortOrderOf(bindings).IsDirectional() {
			return nil
		}
		correlatedTo := make(map[values.CorrelationIdentifier]struct{})
		for _, b := range bindings {
			for alias := range b.ComparisonCorrelatedTo() {
				correlatedTo[alias] = struct{}{}
			}
		}
		if len(correlatedTo) > 1 {
			return nil
		}
		var explodeAlias values.CorrelationIdentifier
		found := false
		for alias := range correlatedTo {
			if _, isExplode := explodeAliases[alias]; isExplode {
				explodeAlias, found = alias, true
			}
		}
		if !found {
			// Bound by a constant or by a correlation anchored above.
			continue
		}
		if _, ok := available[explodeAlias]; !ok {
			return nil
		}
		attempted := attemptedProvidedSortOrdersForAny(exhaustive)
		if provided, directional := part.SortOrder.ToProvidedSortOrder(); directional {
			attempted = []properties.ProvidedSortOrder{provided}
		}
		next := make([]inJoinPrefix, 0, len(prefixes)*len(attempted))
		for _, prefix := range prefixes {
			for _, sortOrder := range attempted {
				next = append(next, inJoinPrefix{
					sources: append(append([]inJoinSource(nil), prefix.sources...), inJoinSource{
						bindingAlias: explodeAlias,
						sorted:       true,
						reverse:      sortOrder.IsAnyDescending(),
						quantifier:   explodeAliasMap[explodeAlias],
					}),
					parts: append(append([]outerPart(nil), prefix.parts...),
						outerPart{value: part.Value, sortOrder: sortOrder}),
				})
			}
		}
		prefixes = next
		delete(available, explodeAlias)
		consumed[key] = struct{}{}
	}

	var result [][]inJoinSource
	if len(available) == 0 {
		filteredInner := innerOrdering.WithoutKeys(consumed)
		for _, prefix := range prefixes {
			keys := make([]values.Value, len(prefix.parts))
			bindingMap := make(map[values.Value][]properties.OrderingBinding, len(prefix.parts))
			for i, p := range prefix.parts {
				keys[i] = p.value
				bindingMap[p.value] = []properties.OrderingBinding{properties.SortedBinding(p.sortOrder)}
			}
			outer := properties.NewRichOrdering(bindingMap, keys, properties.DistinctOverAllKeys())
			if properties.ConcatOrderings(outer, filteredInner).Satisfies(requestedOrdering) {
				result = append(result, prefix.sources)
			}
		}
		return result
	}

	var remaining []values.CorrelationIdentifier
	for _, q := range explodeQuantifiers {
		if _, ok := available[q.GetAlias()]; ok {
			remaining = append(remaining, q.GetAlias())
		}
	}
	permutations := [][]values.CorrelationIdentifier{remaining}
	var attempted []properties.ProvidedSortOrder
	if exhaustive {
		permutations = nil
		iter := combinatorics.Permutations(remaining)
		for perm := iter.Next(); perm != nil; perm = iter.Next() {
			permutations = append(permutations, append([]values.CorrelationIdentifier(nil), perm...))
		}
		attempted = attemptedProvidedSortOrdersForAny(true)
	}
	for _, prefix := range prefixes {
		for _, perm := range permutations {
			if runCtx != nil && runCtx.Err() != nil {
				return nil
			}
			suffixes := [][]inJoinSource{nil}
			for _, alias := range perm {
				var choices []inJoinSource
				if attempted == nil {
					choices = []inJoinSource{{bindingAlias: alias, quantifier: explodeAliasMap[alias]}}
				}
				for _, sortOrder := range attempted {
					choices = append(choices, inJoinSource{
						bindingAlias: alias,
						sorted:       true,
						reverse:      sortOrder.IsAnyDescending(),
						quantifier:   explodeAliasMap[alias],
					})
				}
				var crossed [][]inJoinSource
				for _, suffix := range suffixes {
					for _, choice := range choices {
						crossed = append(crossed, append(append([]inJoinSource(nil), suffix...), choice))
					}
				}
				suffixes = crossed
			}
			for _, suffix := range suffixes {
				result = append(result, append(append([]inJoinSource(nil), prefix.sources...), suffix...))
			}
		}
	}
	return result
}

// attemptedProvidedSortOrdersForAny ports the Java helper of the same name.
func attemptedProvidedSortOrdersForAny(exhaustive bool) []properties.ProvidedSortOrder {
	if exhaustive {
		return []properties.ProvidedSortOrder{
			properties.ProvidedSortOrderAscending, properties.ProvidedSortOrderDescending,
		}
	}
	return []properties.ProvidedSortOrder{properties.ProvidedSortOrderAscending}
}

func getExplodeExpression(ref *expressions.Reference) *expressions.ExplodeExpression {
	for _, m := range ref.AllMembers() {
		if e, ok := m.(*expressions.ExplodeExpression); ok {
			return e
		}
	}
	return nil
}

// classifyInSourceKind determines the InSourceKind for an explode
// quantifier, mirroring Java's ImplementInJoinRule.computeInSource:
//   - ConstantValue (literal list) → InSourceValues
//   - QuantifiedObjectValue (parameter ref) → InSourceParameter
//   - IsConstantValue catch-all → InSourceComparand
func classifyInSourceKind(q expressions.Quantifier) plans.InSourceKind {
	ref := q.GetRangesOver()
	if ref == nil {
		return plans.InSourceValues
	}
	explode := getExplodeExpression(ref)
	if explode == nil {
		return plans.InSourceValues
	}
	cv := explode.GetCollectionValue()
	if cv == nil {
		return plans.InSourceValues
	}
	if _, isQOV := values.AsQuantifiedObjectValue(cv); isQOV {
		return plans.InSourceParameter
	}
	switch cv.(type) {
	case *values.ConstantValue:
		return plans.InSourceValues
	default:
		if values.IsConstantValue(cv) {
			return plans.InSourceComparand
		}
		return plans.InSourceValues
	}
}

// inComparandOf is the row-independent collection value of an explode source
// whose values could not be extracted at plan time; the InJoin evaluates it
// when it opens.
func inComparandOf(q expressions.Quantifier) values.Value {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	explode := getExplodeExpression(ref)
	if explode == nil {
		return nil
	}
	cv := explode.GetCollectionValue()
	if cv == nil || !values.IsConstantValue(cv) {
		return nil
	}
	return cv
}

func extractInValues(q expressions.Quantifier) []any {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	explode := getExplodeExpression(ref)
	if explode == nil {
		return nil
	}
	cv := explode.GetCollectionValue()
	if cv == nil {
		return nil
	}
	// Plan-time IN-list extraction: an erroring or non-list collection value
	// declines (returns nil) rather than failing planning.
	result, err := cv.Evaluate(nil)
	if err != nil {
		return nil
	}
	if vals, ok := result.([]any); ok {
		return vals
	}
	return nil
}

func isSupportedExplodeValue(v values.Value) bool {
	if v == nil {
		return false
	}
	// An ARRAY<RECORD> Explode is a relation source (inline VALUES is the
	// concrete SQL producer), not an IN-list source. InJoin extracts constant
	// values before FinalizePlan and binds each record as a name-keyed map;
	// exact FieldValue evaluation is deliberately ordinal/protobuf-only, so
	// treating that relation as an IN source makes every correlated child read
	// NULL and silently returns zero rows. Leave record-valued Explodes to the
	// NestedLoopJoin/FlatMap implementation. Scalar arrays retain Java's IN
	// source path unchanged.
	if array, ok := v.Type().(*values.ArrayType); ok && array.ElementType != nil &&
		array.ElementType.Code() == values.TypeCodeRecord {
		return false
	}
	if _, isQOV := values.AsQuantifiedObjectValue(v); isQOV {
		return true
	}
	switch v.(type) {
	case *values.ConstantValue:
		return true
	}
	return values.IsConstantValue(v)
}

var _ ImplementationRule = (*ImplementInJoinRule)(nil)
