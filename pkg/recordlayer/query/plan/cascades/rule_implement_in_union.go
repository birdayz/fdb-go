// Portions derived from FoundationDB Record Layer (ImplementInUnionRule.java,
// InComparandSource.java, OrderingPart.java, Reference.java, and others),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"errors"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// bakeMergeComparisonKeys resolves LAZY childless FieldValue merge keys into
// PLAN-TIME-BAKED values — Java's comparison-key values are ordinal-resolved
// OrderingPart values, never name-resolved at runtime. The keys arrive lazy
// because the physical wrappers' Hint(Rich)Ordering builds
// them from index/PK COLUMN NAMES for ordering MATCHING (a string-keyed axis)
// — fine for matching, but evaluating a lazy key against the runtime ordinal
// row would need a runtime name resolution, which does not exist (loud
// OrdinalResolutionError). Two bake authorities, in order:
//
//  1. The REQUESTED ordering's own part value — the translator's bake for the
//     same column (matched by ColumnNameValue, the same bridge orderingKeyFor
//     uses). The requested ordering was pushed down TO this select, so its
//     values are already in the merge row's domain. A column name carried by
//     two semantically different requested parts is ambiguous — skipped.
//  2. The inner plan's flowed RecordType (when the plan flows one), by field
//     name — case-insensitively, and only when the name matches EXACTLY ONE
//     field (uniqueUpperFieldIndex). A name carried by two fields of the
//     flowed row is ambiguous and bakes through neither authority.
//
// Baked keys and computed/correlated keys pass through untouched. A lazy key
// that resolves through NEITHER authority passes through lazy: a record-backed
// merge row still evaluates it through the proto descriptor (the
// legitimate record-boundary resolution, same as Java's Message field access),
// and a positional merge row fails LOUD (OrdinalResolutionError) — never a
// silent wrong slot.
func bakeMergeComparisonKeys(keys []values.Value, requested *properties.RequestedOrdering, rowType values.Type) []values.Value {
	var reqByCol map[string]values.Value
	if requested != nil {
		for _, part := range requested.GetParts() {
			fv, isFV := values.AsFieldValue(part.Value)
			if !isFV || fv.ChildValue() == nil || fv.Path().Len() == 0 {
				continue
			}
			col := values.ColumnNameValue(part.Value)
			if col == "" {
				continue
			}
			if reqByCol == nil {
				reqByCol = map[string]values.Value{}
			}
			if prev, dup := reqByCol[col]; dup && (prev == nil || !values.SemanticEqualsUnderAliasMap(prev, part.Value, nil)) {
				// Ambiguous: two different bakes share the rendered column
				// name. Poison the entry so neither substitutes.
				reqByCol[col] = nil
				continue
			}
			reqByCol[col] = part.Value
		}
	}
	rt, isRT := rowType.(*values.RecordType)
	// Positions >= reqCount are the enumeration's FREE suffix: the
	// permutation's prefix is pinned to the requested keys
	// (SatisfyingPermutations), so everything after position
	// len(requested parts) is ordering the request never asked for — the
	// trimmed-PK-suffix keys the scan's full-key ordering contributes.
	reqCount := -1
	if requested != nil && !requested.IsPreserve() {
		reqCount = len(requested.GetParts())
	}
	out := make([]values.Value, 0, len(keys))
	for i, k := range keys {
		fv, isFV := values.AsFieldValue(k)
		if !isFV {
			out = append(out, k)
			continue
		}
		// RFC-232 makes every FieldValue fully resolved and childful. There is
		// no name-only merge key left to bake here; preserve its exact source
		// root/path. Physical layout selection later binds or reanchors it.
		if fv.ChildValue() != nil && fv.Path().Len() > 0 {
			out = append(out, k)
			continue
		}
		if rv, hit := reqByCol[values.ColumnNameValue(fv)]; hit && rv != nil {
			out = append(out, rv)
			continue
		}
		if isRT && rt != nil {
			// Bake only when the name resolves UNIQUELY. A first-match FieldIndex
			// over a RecordType with DUPLICATE names (a join row flowing here)
			// would silently probe the wrong slot. A duplicate passes through
			// lazy (loud at runtime, never a wrong slot).
			// Single-table branches carry no dups, so this never fires today; it
			// fences the join-flows-here future.
			if _, unique := uniqueUpperFieldIndex(rt, fv.DisplayName()); unique {
				return nil // unreachable for admitted FV; never reconstruct by name
			}
		}
		// Still lazy through both authorities. In the FREE suffix (positions
		// past the requested prefix — the trimmed-PK tiebreak the scan's
		// full-key ordering contributed) an unresolvable key means this
		// comparison-key candidate cannot be planned soundly: the merge
		// cursor DEDUPS on the packed comparison-key tuple, so truncating
		// the tiebreak would collapse distinct rows that tie on the prefix
		// (silent row drops), and passing the lazy key
		// keeps only the record-backed path working. DECLINE the candidate
		// (nil → caller skips this yield; other satisfying permutations and
		// the in-memory-sort alternative still plan). Inside the requested
		// prefix the key passes through lazy — pre-existing behavior: a
		// record-backed merge row resolves it at the record boundary, a
		// positional row fails loud.
		if reqCount >= 0 && i >= reqCount {
			return nil
		}
		out = append(out, k)
	}
	return out
}

// uniqueUpperFieldIndex returns the ordinal (slice position) of the field whose
// name matches `name` case-insensitively, and true ONLY when exactly one field
// matches. A duplicate name (a merged join RecordType) returns false so the
// caller declines to first-match-bake a colliding column.
func uniqueUpperFieldIndex(rt *values.RecordType, name string) (int, bool) {
	idx, count := -1, 0
	for i, f := range rt.Fields {
		if strings.EqualFold(f.Name, name) {
			idx, count = i, count+1
		}
	}
	return idx, count == 1
}

// ImplementInUnionRule implements a SELECT over ExplodeExpressions
// as a RecordQueryInUnionPlan — the inner plan is executed once per
// IN value and results are merge-sorted by comparison keys.
//
// Ports Java's ImplementInUnionRule. The rule adjusts the inner plan's
// ordering bindings: fixed bindings referencing explode aliases are
// promoted to directional (sorted) bindings, enabling merge-sorted
// output. Comparison keys are derived from the adjusted ordering.
type ImplementInUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInUnionRule() *ImplementInUnionRule {
	return &ImplementInUnionRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("implement_in_union").WithRootPredicate(
			func(sel *expressions.SelectExpression) bool { return len(sel.GetQuantifiers()) >= 2 }),
	}
}

func (r *ImplementInUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

// ConstraintDependencies is Java's ImmutableSet.of(REQUESTED_ORDERING).
func (r *ImplementInUnionRule) ConstraintDependencies() []any {
	return []any{RequestedOrderingConstraintKey}
}

func (r *ImplementInUnionRule) OnMatch(call *ImplementationRuleCall) {
	selectExpr := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	if selectExpr.HasPredicates() {
		return
	}

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) < 2 {
		return
	}
	// InUnion merge execution does not own StrictSingle semantics.
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

	explodeAliases := make(map[values.CorrelationIdentifier]struct{}, len(explodeQuantifiers))
	for _, eq := range explodeQuantifiers {
		explodeAliases[eq.GetAlias()] = struct{}{}
	}

	bindingAliases := make([]values.CorrelationIdentifier, len(explodeQuantifiers))
	inSources := make([][]any, len(explodeQuantifiers))
	inComparands := make([]values.Value, len(explodeQuantifiers))
	for i, eq := range explodeQuantifiers {
		bindingAliases[i] = eq.GetAlias()
		if ref := eq.GetRangesOver(); ref != nil {
			for _, member := range ref.AllMembers() {
				if expl, ok := member.(*expressions.ExplodeExpression); ok {
					cv := expl.GetCollectionValue()
					if cv != nil {
						// Plan-time IN-list extraction. A row-independent
						// source planning cannot evaluate (a runtime CAST
						// item, say) is carried as a comparand and evaluated
						// when the plan opens, Java's InComparandSource; it
						// is never dropped.
						if ev, err := cv.Evaluate(nil); err == nil {
							if arr, ok := ev.([]any); ok {
								inSources[i] = arr
							}
						} else if values.IsConstantValue(cv) {
							// Deduplicated at evaluation, as Java's
							// ValueComparison arm explodes
							// ArrayDistinctValue(comparand).
							if _, distinct := cv.(*values.ArrayDistinctValue); !distinct {
								cv = &values.ArrayDistinctValue{Child: cv, Typ: cv.Type()}
							}
							inComparands[i] = cv
						}
					}
					break
				}
			}
		}
	}

	// The raw partition key contains only the reduced Ordering property. An
	// equality-bound index access and an unbounded scan of the same index can
	// therefore share a partition: both advertise the same plain key sequence,
	// while their RichOrdering bindings are materially different (A=fixed-to-IN
	// versus A=sorted). Reading the first member's rich ordering and then cost-
	// selecting from that mixed partition can bake an InUnion over the unbounded
	// residual scan, repeating the full scan once per binding and losing the
	// bounded ordered alternative during group pruning.
	//
	// RichOrdering is every property this rule reads to derive its merge keys.
	// Roll up to that exact property so every member considered for a baked
	// ordered spine has the same fixed/directional binding contract.
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
	}

	// Every plan this rule yields carries the configured maximum product of
	// IN-source sizes; execution refuses a larger one (Java's
	// ImplementInUnionRule reads it once, as here). Java's rule call always has
	// a planner context, so a call without one is a harness defect, not size 0.
	if call.Context == nil {
		call.Fail(errors.New("ImplementInUnionRule: rule call has no planner context to read the in-union size from"))
		return
	}
	maxSize := call.Context.GetPlannerConfiguration().AttemptFailedInJoinAsUnionMaxSize

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}
		innerExprs := partition.GetExpressions()

		richOrdering, _ := partition.GetPartitionPropertyValue(properties.PropRichOrdering).(*properties.RichOrdering)
		var partitionRef *expressions.Reference

		for _, requestedOrdering := range requestedOrderings {
			if requestedOrdering.IsPreserve() {
				continue
			}
			// A requested part this partition reaches only past the record-type
			// coordinate of its primary key is one the target's data access never
			// satisfies, so its reference holds no such leg and it builds no
			// in-union over one (WS-F 4.3 item 2). The marked key may still be a
			// free comparison-key suffix past the request, as in the target's
			// `COMPARE BY (_.COL1, _.ID)`.
			if richOrdering.RequestReachesPastRecordTypeHorizon(requestedOrdering) {
				continue
			}

			adjustedOrdering := adjustBindingsForInUnion(
				richOrdering, explodeAliases, requestedOrdering)
			if adjustedOrdering == nil {
				continue
			}

			satisfyingKeys := adjustedOrdering.EnumerateSatisfyingComparisonKeyValues(requestedOrdering)
			for _, comparisonKeyValues := range satisfyingKeys {
				comparisonParts := adjustedOrdering.DirectionalOrderingParts(
					comparisonKeyValues, requestedOrdering, properties.ProvidedSortOrderFixed)
				isReverse := ResolveComparisonDirection(comparisonParts)
				comparisonParts = AdjustFixedBindings(comparisonParts, isReverse)

				// The merge compares raw tuple-encoded Values in ONE direction,
				// so every comparison part must agree with that direction. A
				// part that disagrees — or asks for counterflow NULLs — needs
				// the ordered-bytes encoding Go does not evaluate yet; decline
				// the candidate rather than merge a descending key forward.
				// This is the same fail-closed gate the intersection plans use.
				//
				// Reachable: over the 2834-query corpus 6 of 424 candidate
				// evaluations decline, each a two-part key mixing ASC and DESC.
				comparisonKeys, natural := properties.NaturalComparisonKeyValues(comparisonParts, isReverse)
				if !natural {
					continue
				}
				// Reading a property off member [0] of a raw partition is
				// normally a fact about one member asserted of all of them
				// (see ToPlanPartitions), and the plan finally baked below is
				// `best`, not [0]. The ROW SHAPE is the one property where
				// index 0 is genuinely representative, and not because of the
				// partition: every member here is a member of ONE equivalence
				// class (innerRef), and an equivalence class has a single
				// result type by construction — Java resolves it by reducing
				// over all members under Verify.verify(left.equals(right))
				// (Reference.java getResultType), so a disagreement is a memo
				// defect, not a shape to handle. Go cannot crash there, so the
				// disagreement is counted and witnessed instead
				// (recordMergeSlotTypeDisagreement, positional_merge.go).
				//
				// It is load-bearing because the bake resolves lazy keys by
				// FIELD NAME against this type: were two members to flow
				// different rows, a name that resolved to different ordinals
				// would bake a wrong-slot read into the merge comparison —
				// silently, since the rows a merge emits are unchanged.
				comparisonKeys = bakeMergeComparisonKeys(comparisonKeys, requestedOrdering, innerPlans[0].GetResultType())
				if comparisonKeys == nil {
					// Unresolvable free-suffix tiebreak — candidate declined
					// (see bakeMergeComparisonKeys); the sort-based
					// alternative still plans.
					continue
				}

				// Go's own soundness gate (Java checks nothing): the merge's
				// dedup on the key must never collapse two rows of the join,
				// whichever member of the partition runs.
				if !partitionMergeKeyIdentifiesRows(innerExprs, comparisonParts) {
					continue
				}

				// The partition is memoized WHOLE, as Java's
				// memoizeMemberPlansFromOther(innerReference,
				// planPartition.getPlans()) does (WS-F 4.3 item 3): every
				// member provides the partition's ordering, so costing chooses
				// among them, and push-through rules see every member (a
				// covering member under its fetch included). The memoizer
				// carries the inner reference's requested orderings, so the
				// copy's optimization keeps the members that satisfy them;
				// extraction verifies the chosen child provides the comparison
				// keys' order (checkInUnionChildOrdering).
				if partitionRef == nil {
					partitionRef = call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
				}
				inUnionPlan, err := plans.NewRecordQueryInUnionPlanFromQuantifierWithBindingAliases(
					expressions.NewPhysicalQuantifier(partitionRef),
					bindingAliases, comparisonKeys, isReverse, maxSize)
				if err != nil {
					call.Fail(err)
					return
				}
				inUnionPlan = inUnionPlan.WithInSources(inSources).WithInComparands(inComparands)
				call.YieldFinalExpression(inUnionPlan)
			}
		}
	}
}

// inUnionMergeKeyIdentifiesRows reports whether the merge's dedup on the
// comparison key can never collapse two rows of the join it implements: the
// baked inner must prove its rows distinct over coordinates inside the key. A
// per-stream claim names the explode-bound coordinates that separate the
// branches; a record-identity claim is a key across all of them. Java's
// ImplementInUnionRule checks nothing, and its UnionCursor drops the tied rows
// of a projection that lost the primary key.
func inUnionMergeKeyIdentifiesRows(inner physicalPlanExpression, parts []properties.ProvidedOrderingPart) bool {
	keys := make([]values.Value, len(parts))
	for i, part := range parts {
		keys[i] = part.Value
	}
	return computeWrapperRichOrdering(inner).RowsIdentifiedBy(keys)
}

// partitionMergeKeyIdentifiesRows is inUnionMergeKeyIdentifiesRows for every
// member of a partition: the merge ranges over the whole partition, and
// extraction may choose any member, so each must prove its rows identified.
func partitionMergeKeyIdentifiesRows(members []expressions.RelationalExpression, parts []properties.ProvidedOrderingPart) bool {
	if len(members) == 0 {
		return false
	}
	for _, member := range members {
		plan, ok := member.(physicalPlanExpression)
		if !ok || !inUnionMergeKeyIdentifiesRows(plan, parts) {
			return false
		}
	}
	return true
}

// adjustBindingsForInUnion adjusts the inner ordering's bindings:
// fixed bindings whose comparison references an explode alias are
// promoted to directional (sorted) bindings. This enables the InUnion
// to merge-sort output by those keys.
func adjustBindingsForInUnion(
	ordering *properties.RichOrdering,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	requestedOrdering *properties.RequestedOrdering,
) *properties.RichOrdering {
	if ordering == nil || len(ordering.GetKeys()) == 0 {
		return nil
	}

	reqMap := requestedOrdering.GetValueRequestedSortOrderMap()
	adjustedBM := make(map[values.Value][]properties.OrderingBinding, len(ordering.GetBindingMap()))

	for val, bindings := range ordering.GetBindingMap() {
		sortOrder := properties.SortOrderOf(bindings)
		if sortOrder.IsDirectional() {
			adjustedBM[val] = []properties.OrderingBinding{properties.SortedBinding(sortOrder)}
			continue
		}

		if !properties.AreAllBindingsFixed(bindings) || properties.HasMultipleFixedBindings(bindings) {
			adjustedBM[val] = bindings
			continue
		}

		b := properties.SingleFixedBinding(bindings)
		comp := b.GetComparison()
		if comp == nil {
			adjustedBM[val] = bindings
			continue
		}

		cr, ok := comp.(*predicates.ComparisonRange)
		if !ok {
			adjustedBM[val] = bindings
			continue
		}
		eqComp := cr.GetEqualityComparison()
		if eqComp == nil {
			adjustedBM[val] = bindings
			continue
		}

		correlated := eqComp.GetCorrelatedTo()
		isExplodeCorrelated := false
		for alias := range correlated {
			if _, ok := explodeAliases[alias]; ok {
				isExplodeCorrelated = true
				break
			}
		}

		if !isExplodeCorrelated {
			adjustedBM[val] = bindings
			continue
		}

		if reqSort, ok := reqMap[val]; ok && reqSort.IsDirectional() {
			if reqSort.IsAnyAscending() {
				adjustedBM[val] = []properties.OrderingBinding{properties.SortedBinding(properties.ProvidedSortOrderAscending)}
			} else {
				adjustedBM[val] = []properties.OrderingBinding{properties.SortedBinding(properties.ProvidedSortOrderDescending)}
			}
		} else {
			adjustedBM[val] = []properties.OrderingBinding{properties.ChooseBinding()}
		}
	}

	// Binding promotion changes how a key is compared by the merge; it does
	// not change the ordering relationships the selected leg already proved.
	// In particular, an IN-correlated fixed prefix is independent in the
	// provider's ordering set. Promoting it to CHOOSE must not reconstruct a
	// new dependency from the storage-key sequence and force that prefix back
	// in front of the requested suffix. Java's ImplementInUnionRule likewise
	// creates the adjusted UNION ordering with the provider's existing
	// ordering set.
	return properties.NewRichOrderingWithDeps(
		adjustedBM,
		ordering.GetKeys(),
		ordering.OrderingSet().DependencyMap(),
		ordering.DistinctnessClaim(),
	)
}

var _ ImplementationRule = (*ImplementInUnionRule)(nil)
