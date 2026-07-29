package cascades

import (
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
//  2. The inner plan's flowed RecordType (when the plan flows one), by
//     first-match field name — the plan-time FieldIndex rule.
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
			fv, isFV := part.Value.(*values.FieldValue)
			if !isFV || fv.Child != nil || fv.Resolved == nil {
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
		fv, isFV := k.(*values.FieldValue)
		if !isFV || fv.Child != nil || fv.Resolved != nil {
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
			if idx, unique := uniqueUpperFieldIndex(rt, fv.Field); unique {
				// The ordinal is resolved against rt HERE, so rt is the layout
				// it indexes and the domain is a proof rather than a claim
				// (RFC-197 step 0).
				out = append(out, values.NewFieldValueWithResolvedOrdinalInDomain(fv.Field, idx, fv.Typ, values.OrdinalDomainOfType(rt)))
				continue
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
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("implement_in_union"),
	}
}

func (r *ImplementInUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

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

	qov, ok := resultValue.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != innerQuantifier.GetAlias() {
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

	bindingNames := make([]string, len(explodeQuantifiers))
	inSources := make([][]any, len(explodeQuantifiers))
	for i, eq := range explodeQuantifiers {
		bindingNames[i] = eq.GetAlias().String()
		if ref := eq.GetRangesOver(); ref != nil {
			for _, member := range ref.AllMembers() {
				if expl, ok := member.(*expressions.ExplodeExpression); ok {
					cv := expl.GetCollectionValue()
					if cv != nil {
						// Plan-time IN-list extraction: an erroring value
						// declines (leaves the source nil) rather than
						// failing planning.
						if ev, err := cv.Evaluate(nil); err == nil {
							if arr, ok := ev.([]any); ok {
								inSources[i] = arr
							}
						}
					}
					break
				}
			}
		}
	}

	partitions := ToPlanPartitions(innerRef)
	if len(partitions) == 0 {
		return
	}

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	}

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}
		innerExprs := partition.GetExpressions()

		var richOrdering *properties.RichOrdering
		for _, expr := range innerExprs {
			if ph, ok := expr.(physicalPlanExpression); ok {
				richOrdering = computeWrapperRichOrdering(ph)
				break
			}
		}

		for _, requestedOrdering := range requestedOrderings {
			if requestedOrdering.IsPreserve() {
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
				// Reachable: it declines exactly twice over the 2475-query
				// corpus, both times parts=[ASC, DESC] against a forward merge.
				comparisonKeys, natural := properties.NaturalComparisonKeyValues(comparisonParts, isReverse)
				if !natural {
					continue
				}
				comparisonKeys = bakeMergeComparisonKeys(comparisonKeys, requestedOrdering, innerPlans[0].GetResultType())
				if comparisonKeys == nil {
					// Unresolvable free-suffix tiebreak — candidate declined
					// (see bakeMergeComparisonKeys); the sort-based
					// alternative still plans.
					continue
				}

				// The comparison keys are the inner's ORDERING CONTRACT for
				// the InUnion merge-dedup — the partition-level first-member
				// ESTIMATE (a delegator's group hint) is not tethered to the
				// baked plan. Pick the cheapest member that STRUCTURALLY
				// satisfies the contract (delegators resolve through their
				// source groups), spine-PIN it (executable-plan verified),
				// and bake THAT plan over a FinalOf singleton; an unpinnable
				// partition skips this candidate — the sort-based
				// alternative still plans.
				legReqParts := make([]properties.RequestedOrderingPart, len(comparisonParts))
				for i, p := range comparisonParts {
					so := properties.RequestedSortOrderAny
					if p.SortOrder != properties.ProvidedSortOrderFixed {
						so = p.SortOrder.ToRequestedSortOrder()
					}
					legReqParts[i] = properties.RequestedOrderingPart{Value: p.Value, SortOrder: so}
				}
				legReq := properties.NewRequestedOrdering(legReqParts, properties.DistinctnessPreserveDistinctness, false)
				tieBrokenLess := lessWithHashTieBreak(call.CostModel())
				var best expressions.RelationalExpression
				for _, pe := range innerExprs {
					if !memberSatisfiesOrdering(pe, legReq) {
						continue
					}
					if best == nil || tieBrokenLess(pe, best) {
						best = pe
					}
				}
				if best == nil {
					continue
				}
				pinned := pinOrderedSpine(best, legReq, call.CostModel())
				if pinned == nil {
					continue
				}
				if _, isPhys := pinned.(physicalPlanExpression); !isPhys {
					continue
				}

				maxSize := 0
				if call.Context != nil {
					maxSize = call.Context.GetPlannerConfiguration().AttemptFailedInJoinAsUnionMaxSize
				}
				// The InUnion is its own cascades expression over the live pinned
				// inner edge (RFC-184 W2); no plan snapshot.
				inUnionPlan := plans.NewRecordQueryInUnionPlanFromQuantifier(
					expressions.NewPhysicalQuantifier(expressions.FinalOf(pinned)),
					bindingNames, comparisonKeys, isReverse, maxSize)
				inUnionPlan.SetInSources(inSources)
				call.YieldFinalExpression(inUnionPlan)
			}
		}

		if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
			newRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
			// The InUnion is its own cascades expression carrying the live newRef
			// inner edge (RFC-184 W2); its per-ordering winner resolves at
			// extraction via ref.Winner(). No plan snapshot — the deferred-winner
			// case.
			inUnionPlan := plans.NewRecordQueryInUnionPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(newRef),
				bindingNames, nil, false, 0)
			inUnionPlan.SetInSources(inSources)
			call.YieldFinalExpression(inUnionPlan)
		}
	}
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

	return properties.NewRichOrdering(adjustedBM, ordering.GetKeys(), ordering.IsDistinct())
}

var _ ImplementationRule = (*ImplementInUnionRule)(nil)
