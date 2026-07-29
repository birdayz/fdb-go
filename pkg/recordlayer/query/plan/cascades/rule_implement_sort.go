package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementSortRule removes a logical LogicalSortExpression when the
// inner plan already satisfies the requested ordering. This is Java's
// RemoveSortRule pattern: sort is a constraint, not a physical operator.
//
// During PLANNING's top-down pass, the sort expression's requested
// ordering is pushed as a constraint to the inner reference (via
// GetRequestedOrderings). During the bottom-up pass, this rule checks
// if the inner partition's ordering satisfies the request, and if so,
// yields the inner plans directly (removing the sort).
//
// Ports Java's RemoveSortRule (ImplementationCascadesRule).
type ImplementSortRule struct {
	matcher matching.BindingMatcher
}

func NewImplementSortRule() *ImplementSortRule {
	return &ImplementSortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("implement_sort"),
	}
}

func (r *ImplementSortRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementSortRule) OnMatch(call *ImplementationRuleCall) {
	s := call.Bindings.Get(r.matcher).(*expressions.LogicalSortExpression)

	requestedOrdering := sortExpressionToRequestedOrdering(s)

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Top-down: push ordering constraint to inner reference so
	// downstream rules (index scans) can satisfy it.
	call.PushConstraint(innerRef, []*properties.RequestedOrdering{requestedOrdering})

	if requestedOrdering.IsPreserve() {
		for _, m := range innerRef.AllMembers() {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			call.YieldFinalExpression(m)
		}
		return
	}

	requestedParts := requestedOrdering.GetParts()
	sortValueNames := sortRequestedNames(requestedParts)
	preserveDistinctReq := properties.NewRequestedOrdering(
		requestedParts,
		properties.DistinctnessPreserveDistinctness,
		requestedOrdering.IsExhaustive(),
	)

	partitions := ToPlanPartitions(innerRef)
	for _, partition := range partitions {
		// Ordering is validated per expression, not once from the partition's
		// representative. Most producers are partitioned by their plain
		// Ordering, but a frozen FlatMap/NLJ carries a richer order derived
		// from its exact two child plans. Treating the first join as
		// representative of every otherwise-identical join in the bucket could
		// drop a sort over an unordered sibling.
		for _, expr := range partition.GetExpressions() {
			ph, ok := expr.(physicalPlanExpression)
			if !ok {
				continue
			}
			ordering := computeWrapperRichOrdering(ph)
			if ordering == nil || !ordering.Satisfies(preserveDistinctReq) {
				// A FlatMap's Java ordering cases depend on the concrete pair
				// of child plans. Its initial implementation is built before
				// child data-access re-exploration has produced requested-order
				// index alternatives, so form the exact-child variants here,
				// at the bottom-up sort boundary where both child groups are
				// complete. Each candidate is verified through the same rich
				// property before the enforcer is removed.
				for _, candidate := range orderedFlatMapCandidatesAtSort(
					call, expr, preserveDistinctReq) {
					call.YieldFinalExpression(candidate)
				}
				continue
			}

			eqBoundNames := sortEqualityBoundNames(ordering)
			equalityBoundUnsorted := len(ordering.GetEqualityBoundValues())
			seenEqBound := make(map[string]bool, len(requestedParts))
			for _, part := range requestedParts {
				name := values.ExplainValue(part.Value)
				if _, ok := eqBoundNames[name]; ok && !seenEqBound[name] {
					seenEqBound[name] = true
					equalityBoundUnsorted--
				}
			}

			// Java RemoveSortRule lines 112-125: when the partition is
			// distinct and all ordering values are covered by sort keys
			// or equality-bound keys, yield a strictlySorted copy.
			if partition.IsDistinct() {
				if sortCoverageAllCovered(ordering, sortValueNames, eqBoundNames) {
					if pinned := pinOrderedSpine(expr, preserveDistinctReq, call.CostModel()); pinned != nil {
						call.YieldFinalExpression(makeStrictlySorted(pinned))
					}
					continue
				}
			}

			// Java RemoveSortRule lines 127-141: check this plan for
			// unique-index coverage → strictlySorted.
			//
			// Every yield here DROPS the sort on the strength of expr's claimed
			// ordering — an order-preserving wrapper must therefore have its
			// delegation spine pinned (pinOrderedSpine): otherwise extraction's
			// generic rebuild can relink its child group to a cheaper UNORDERED
			// sibling after the sort is gone. Unpinnable expressions are simply
			// not yielded — the in-memory sort alternative still competes.
			numKeys := len(requestedParts) + equalityBoundUnsorted
			pinned := pinOrderedSpine(expr, preserveDistinctReq, call.CostModel())
			if pinned == nil {
				continue
			}
			if strictlyOrderedIfUnique(pinned, numKeys) {
				call.YieldFinalExpression(makeStrictlySorted(pinned))
			} else {
				call.YieldFinalExpression(pinned)
			}
		}
	}
}

// sortRequestedNames renders the REQUESTED ordering parts into the key set the
// coverage decision probes. Exists so the decision has ONE definition: a test
// that rebuilds this set itself is testing its own copy, and its retirement
// trigger cannot fire when the rule's version changes.
func sortRequestedNames(parts []properties.RequestedOrderingPart) map[string]struct{} {
	names := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		names[values.ExplainValue(part.Value)] = struct{}{}
	}
	return names
}

// sortEqualityBoundNames renders the PROVIDED ordering's equality-bound values.
// Same single-definition reason as sortRequestedNames.
func sortEqualityBoundNames(ordering *properties.RichOrdering) map[string]struct{} {
	eqBound := ordering.GetEqualityBoundValues()
	names := make(map[string]struct{}, len(eqBound))
	for v := range eqBound {
		names[values.ExplainValue(v)] = struct{}{}
	}
	return names
}

// sortCoverageAllCovered is Java RemoveSortRule's `allCovered` decision: every
// provided ordering key must be accounted for, either by a requested sort key or
// by being equality-bound.
//
// It runs AFTER RichOrdering.Satisfies has already said yes, and the two can
// disagree — that disagreement is a sort that survives a request the planner
// already proved was met, which is why it is pinned separately in
// ordering_identity_decisions_test.go rather than assumed to follow.
//
// The `inEq` disjunct is not optional garnish: inEq can be true for a
// MIXED-binding key (one fixed binding plus one sorted), because GetOrderingKeys
// excludes all-fixed keys while GetEqualityBoundValues includes any-fixed ones. A
// coverage check that dropped it would refuse every equality-prefixed scan.
func sortCoverageAllCovered(
	ordering *properties.RichOrdering,
	sortValueNames, eqBoundNames map[string]struct{},
) bool {
	for _, v := range ordering.GetOrderingKeys() {
		name := values.ExplainValue(v)
		_, inSort := sortValueNames[name]
		_, inEq := eqBoundNames[name]
		if !inSort && !inEq {
			return false
		}
	}
	return true
}

func orderedFlatMapCandidatesAtSort(
	call *ImplementationRuleCall,
	base expressions.RelationalExpression,
	requested *properties.RequestedOrdering,
) []expressions.RelationalExpression {
	flatMap, ok := base.(*plans.RecordQueryFlatMapPlan)
	if !ok || requested == nil || requested.IsPreserve() {
		return nil
	}
	quantifiers := flatMap.GetQuantifiers()
	if len(quantifiers) != 2 {
		return nil
	}
	outerRef := quantifiers[0].GetRangesOver()
	innerRef := quantifiers[1].GetRangesOver()
	resultValue := flatMap.GetResultValue()
	if outerRef == nil || innerRef == nil || resultValue == nil {
		return nil
	}
	outerOrderingResultValue := flatMapOrderingResultForChild(
		flatMap, quantifiers[0].GetAlias(), true)

	localAliases := map[values.CorrelationIdentifier]struct{}{
		quantifiers[0].GetAlias(): {},
		quantifiers[1].GetAlias(): {},
	}
	outerRequested := pushRequestedOrderingToSelectChild(
		requested, outerOrderingResultValue,
		quantifiers[0].GetAlias(), localAliases)
	innerRequested := pushRequestedOrderingToSelectChild(
		requested, resultValue, quantifiers[1].GetAlias(), localAliases)
	less := lessWithHashTieBreak(call.CostModel())

	rawOuters := collectJoinLegOrderingVariants(
		outerRef, properties.PreserveOrdering(), outerOrderingResultValue,
		quantifiers[0].GetAlias(), less, false, call.Context)
	rawInners := collectJoinLegOrderingVariants(
		innerRef, properties.PreserveOrdering(), resultValue,
		quantifiers[1].GetAlias(), less, false, call.Context)
	orderedOuters := collectJoinLegOrderingVariants(
		outerRef, outerRequested, outerOrderingResultValue,
		quantifiers[0].GetAlias(), less, true, call.Context)
	orderedInners := collectJoinLegOrderingVariants(
		innerRef, innerRequested, resultValue,
		quantifiers[1].GetAlias(), less, true, call.Context)
	rebuild := func(
		outer, inner expressions.RelationalExpression,
	) expressions.RelationalExpression {
		if outer == nil || inner == nil {
			return nil
		}
		exactQuantifiers := []expressions.Quantifier{
			expressions.RebuildQuantifier(
				quantifiers[0], call.MemoizeFinalExpression(outer)),
			expressions.RebuildQuantifier(
				quantifiers[1], call.MemoizeFinalExpression(inner)),
		}
		return flatMap.WithQuantifiers(exactQuantifiers)
	}

	var result []expressions.RelationalExpression
	add := func(outer, inner expressions.RelationalExpression) {
		candidate := rebuild(outer, inner)
		ph, ok := candidate.(physicalPlanExpression)
		if !ok {
			return
		}
		ordering := computeWrapperRichOrdering(ph)
		if ordering == nil || !ordering.Satisfies(requested) {
			return
		}
		for _, existing := range result {
			existingPhysical, ok := existing.(physicalPlanExpression)
			if ok && plans.Equals(
				existingPhysical.GetRecordQueryPlan(),
				ph.GetRecordQueryPlan(),
			) {
				return
			}
		}
		result = append(result, candidate)
	}

	for _, pair := range orderedJoinLegPairs(
		rawOuters, rawInners, orderedOuters, orderedInners,
		requested, less,
	) {
		add(pair.outer, pair.inner)
	}
	return result
}

func (r *ImplementSortRule) GetRequestedOrderings(
	expr expressions.RelationalExpression,
) []*properties.RequestedOrdering {
	s, ok := expr.(*expressions.LogicalSortExpression)
	if !ok {
		return nil
	}
	return []*properties.RequestedOrdering{sortExpressionToRequestedOrdering(s)}
}

func sortExpressionToRequestedOrdering(s *expressions.LogicalSortExpression) *properties.RequestedOrdering {
	keys := s.GetSortKeys()
	if len(keys) == 0 {
		return properties.PreserveOrdering()
	}
	parts := make([]properties.RequestedOrderingPart, len(keys))
	for i, k := range keys {
		// Carry the explicit NULL placement (k.NullsFirst, nil = natural:
		// ASC→FIRST, DESC→LAST). An explicit non-natural placement maps to the
		// *NullsLast/*NullsFirst variant so the satisfaction check retains the
		// sort instead of eliding it against an opposite-null-placement scan.
		var sortOrder properties.RequestedSortOrder
		if !k.Reverse {
			sortOrder = properties.RequestedSortOrderAscending
			if k.NullsFirst != nil && !*k.NullsFirst {
				sortOrder = properties.RequestedSortOrderAscendingNullsLast
			}
		} else {
			sortOrder = properties.RequestedSortOrderDescending
			if k.NullsFirst != nil && *k.NullsFirst {
				sortOrder = properties.RequestedSortOrderDescendingNullsFirst
			}
		}
		parts[i] = properties.RequestedOrderingPart{
			Value:     k.Value,
			SortOrder: sortOrder,
		}
	}
	return properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false)
}

// strictlyOrderedIfUnique checks whether the given expression is a unique
// index scan whose column count is covered by numKeys (requested sort keys +
// equality-bound unsorted keys). Mirrors Java's RemoveSortRule.strictlyOrderedIfUnique.
// Looks through Fetch wrappers to find the underlying index scan.
func strictlyOrderedIfUnique(expr expressions.RelationalExpression, numKeys int) bool {
	if p, ok := expr.(*plans.RecordQueryIndexPlan); ok {
		return p.IsUnique() && numKeys >= len(p.GetColumnNames())
	}
	if fw, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		ref := fw.GetInnerQuantifier().GetRangesOver()
		if ref == nil {
			return false
		}
		for _, m := range ref.AllMembers() {
			if p, ok := m.(*plans.RecordQueryIndexPlan); ok {
				return p.IsUnique() && numKeys >= len(p.GetColumnNames())
			}
		}
	}
	return false
}

// makeStrictlySorted returns an expression with its inner plan marked
// as strictlySorted. For index scans, this creates a new wrapper with
// a cloned plan. For Fetch wrappers, creates a new Fetch wrapping a
// strictlySorted index plan. For other plan types, returns unchanged.
func makeStrictlySorted(expr expressions.RelationalExpression) expressions.RelationalExpression {
	if p, ok := expr.(*plans.RecordQueryIndexPlan); ok {
		// WithStrictlySorted is a struct copy — it preserves the index metadata
		// (columns/pk/unique/covering) the plan already carries (RFC-184 W2).
		return p.WithStrictlySorted()
	}
	if fw, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		inner := fw.GetInner()
		if idxPlan, ok := inner.(*plans.RecordQueryIndexPlan); ok {
			// The index scan is its own cascades expression (RFC-184 W2), carrying its
			// metadata on the plan; WithStrictlySorted preserves it (struct copy). The
			// strictly-sorted path is only reached for a unique index (see
			// strictlyOrderedIfUnique), so the plan's unique flag is already true.
			newIdxPlan := idxPlan.WithStrictlySorted()
			newIdxRef := expressions.InitialOf(newIdxPlan)
			newFetchQ := expressions.ForEachQuantifier(newIdxRef)
			// The fetch is its own cascades expression carrying the live newIdxRef
			// edge (RFC-184 W2).
			return plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				newFetchQ,
				fw.GetTranslateValueFunction(),
				fw.GetResultType(),
				fw.GetFetchIndexRecords(),
			)
		}
	}
	return expr
}

var _ ImplementationRule = (*ImplementSortRule)(nil)
