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
	sortValueNames := make(map[string]struct{}, len(requestedParts))
	for _, part := range requestedParts {
		sortValueNames[values.ExplainValue(part.Value)] = struct{}{}
	}
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

			eqBound := ordering.GetEqualityBoundValues()
			eqBoundNames := make(map[string]struct{}, len(eqBound))
			for v := range eqBound {
				eqBoundNames[values.ExplainValue(v)] = struct{}{}
			}
			equalityBoundUnsorted := len(eqBound)
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
			if partition.IsDistinct() && expressionStorageOrderingIsComplete(expr) {
				allCovered := true
				for _, v := range ordering.GetOrderingKeys() {
					name := values.ExplainValue(v)
					_, inSort := sortValueNames[name]
					// inEq can be true for mixed-binding keys (one fixed +
					// one sorted) — GetOrderingKeys excludes all-fixed but
					// GetEqualityBoundValues includes any-fixed.
					_, inEq := eqBoundNames[name]
					if !inSort && !inEq {
						allCovered = false
						break
					}
				}
				if allCovered {
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
	scan := indexScanUnderOrderPreservingWrappers(expr)
	if scan == nil {
		return false
	}
	return numKeys >= len(scan.GetColumnNames()) &&
		indexHasStreamEnforcedUniqueKey(scan)
}

// indexScanUnderOrderPreservingWrappers finds the index scan this expression's
// claimed ordering ultimately comes from.
//
// IT DELIBERATELY DOES NOT DESCEND THROUGH A FILTER, and that is a soundness
// requirement rather than a missing case. R2's two routes are NOT symmetric
// across its two consumers, which is the mirror image of the asymmetry recorded
// at indexHasStreamEnforcedUniqueKey:
//
//   - For the DISTINCT consumer, a residual filter's conjuncts are valid
//     evidence. The elision happens at the DISTINCT node, which sits ABOVE the
//     filter, so the stream the proof is about is the stream the decision is
//     made on.
//   - For THIS consumer it is not, because there is nowhere to put the
//     conclusion. `strictlySorted` is a field on RecordQueryIndexPlan, and
//     ordering.go:1222 hands it to NewRichOrdering as the scan's own DISTINCTNESS.
//     A filter's conjuncts prove something about the FILTER's output; marking the
//     scan below it asserts that property of a stream that still contains the
//     index's NULL entries. The claim is then readable from the bare scan node by
//     any consumer that never sees the filter — measured: `WHERE n <> 'z' ORDER
//     BY n` produced `PredicatesFilter(IndexScan(U, [*]))` whose scan reported
//     IsDistinct() == true over a full scan including every NULL entry.
//
// So only evidence that is a property of the SCAN ITSELF may license this claim,
// which is exactly the scan-range route: the range determines what the scan
// emits, so a range excluding the NULL boundary is a fact about the marked node.
// The refutation is pinned in the SQL-path witness rather than left as prose.
func indexScanUnderOrderPreservingWrappers(
	expr expressions.RelationalExpression,
) *plans.RecordQueryIndexPlan {
	if p, ok := expr.(*plans.RecordQueryIndexPlan); ok {
		return p
	}
	// A fetch reshapes rows without removing any, so the scan under it emits
	// exactly what the fetch does and the flag describes the right stream.
	if fw, ok := expr.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		ref := fw.GetInnerQuantifier().GetRangesOver()
		if ref == nil {
			return nil
		}
		for _, m := range ref.AllMembers() {
			if p, ok := m.(*plans.RecordQueryIndexPlan); ok {
				return p
			}
		}
	}
	return nil
}

// indexHasStreamEnforcedUniqueKey is the strict-ordering consumer of the same
// gate the DISTINCT elision uses, and the nullability clause inside it is a
// DELIBERATE DIVERGENCE FROM JAVA rather than an extra check to be simplified
// away.
//
// Java decides the whole question at RemoveSortRule.java:153 —
//
//	return matchCandidate.isUnique() && numKeys >= matchCandidate.getColumnSize();
//
// with NO nullability term anywhere on the path into it. Over a UNIQUE index on
// a NULLABLE column that claim is FALSE: under NULLS DISTINCT the uniqueness
// check is skipped when a component is NULL, so the index legitimately holds
// (NULL, pk=1), (NULL, pk=2), (NULL, pk=3) — three entries whose claimed sort
// key is identical. A consumer trusting strictlySorted to mean "no two rows
// compare equal on the sort key" is being told something false by construction,
// not in an edge case. Upstream soundness bug; see DIVERGENCES.md.
//
// The gate is therefore NOT relaxed to match Java. What makes the claim reachable
// on the streams where it is TRUE is a NULL-rejecting predicate emptying the
// index's exempt set, exactly as for the DISTINCT consumer.
//
// R2's two routes are not interchangeable, and the difference was recorded here
// as an open soundness question before it was settled. It is settled now, and
// the answer is in null_rejecting_scan_range.go with the binder citations: the
// DISTINCT route reads FILTER conjuncts, where three-valued semantics does the
// work, while an index scan's ComparisonRange is a PHYSICAL KEY ENCLOSURE, and
// comparison_range.go:196-203 classifies ComparisonEquals, ComparisonIsNull and
// ComparisonNotDistinctFrom alike as scan-range equality types.
//
// The worry was that an Equals over a NULL literal or a NULL-bound parameter
// would compile to a singleton range ENCLOSING the NULL boundary and, with no
// residual filter, emit NULL entries under a strict-sorting claim that has no
// residual to catch the error. That shape IS constructible — the planner builds
// the equality range and leaves no residual — but it does not enclose the NULL
// boundary: the binder short-circuits a NULL-valued equality to an EMPTY range
// (scan_range_binding.go:321-325) rather than seeking [null]. Zero rows have no
// ties. The two kinds that DO enclose the NULL boundary, IS NULL and IS NOT
// DISTINCT FROM, are the two the allow-list already refuses.
//
// So the scan-range route licenses the claim, and it is the ONLY route that may.
// The `WHERE email IS NOT NULL` of §5.7 is SARGed into the scan range, so this is
// also the route that fires on the shape the RFC quotes.
//
// The FILTER route is deliberately absent, and its absence is a second soundness
// finding rather than an omission — see indexScanUnderOrderPreservingWrappers.
// R2's routes are not symmetric across its two consumers: a residual filter's
// conjuncts prove something about the FILTER's output, and this consumer's only
// place to record a conclusion is a field on the SCAN below it.
//
// indexHasStreamEnforcedUniqueKey is therefore the same predicate restricted to
// ONE STREAM, exactly as the DISTINCT consumer's clause 8 is — with the stream
// described by what the scan's own range encloses.
func indexHasStreamEnforcedUniqueKey(p *plans.RecordQueryIndexPlan) bool {
	if p == nil || !p.IsUnique() {
		return false
	}
	keyColumns := p.GetColumnNames()
	// Each key column needs its own evidence and the positions are never rounded
	// up: a composite UNIQUE (a, b) with only `a` constrained still admits
	// (1, NULL) and (1, NULL), so an uncovered position stays false and declines
	// the proof. nullRejectedByScanRange states the evidence in the index's own
	// KEY COLUMN ORDER, which is the order GetKeyComponentTypes arrives in and
	// therefore the only order the gate can consume.
	return properties.SecondaryUniqueKeyEnforcedOnStream(
		p.GetKeyComponentTypes(), len(keyColumns),
		nullRejectedByScanRange(p.GetScanComparisons(), len(keyColumns)),
	)
}

// makeStrictlySorted returns an expression with its inner plan marked
// as strictlySorted. For index scans, this creates a new wrapper with
// a cloned plan. For Fetch wrappers, creates a new Fetch wrapping a
// strictlySorted index plan. For other plan types, returns unchanged.
//
// The wrappers it rebuilds must be exactly those
// indexScanUnderOrderPreservingWrappers descends through: a shape the walk
// admits but this function returns unchanged is a claim that was proved and then
// silently dropped, so the two enumerations move together.
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
