package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// bakeMergeComparisonKeys resolves LAZY childless FieldValue merge keys into
// PLAN-TIME-BAKED values (RFC-173 — Java's comparison-key values are
// ordinal-resolved OrderingPart values, never name-resolved at runtime). The
// keys arrive lazy because the physical wrappers' Hint(Rich)Ordering builds
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
func bakeMergeComparisonKeys(keys []values.Value, requested *RequestedOrdering, rowType values.Type) []values.Value {
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
	out := make([]values.Value, len(keys))
	for i, k := range keys {
		fv, isFV := k.(*values.FieldValue)
		if !isFV || fv.Child != nil || fv.Resolved != nil {
			out[i] = k
			continue
		}
		if rv, hit := reqByCol[values.ColumnNameValue(fv)]; hit && rv != nil {
			out[i] = rv
			continue
		}
		if isRT && rt != nil {
			// Bake only when the name resolves UNIQUELY. A first-match FieldIndex
			// over a RecordType with DUPLICATE names (a join row flowing here)
			// would silently probe the wrong slot — RFC-173's conflation. A
			// duplicate passes through lazy (loud at runtime, never a wrong slot).
			// Single-table branches carry no dups, so this never fires today; it
			// fences the join-flows-here future.
			if idx, unique := uniqueUpperFieldIndex(rt, fv.Field); unique {
				out[i] = values.NewFieldValueWithResolvedOrdinal(fv.Field, idx, fv.Typ)
				continue
			}
		}
		out[i] = k
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
		matcher: &selectExpressionMatcher{},
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
		requestedOrderings = []*RequestedOrdering{PreserveOrdering()}
	}

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}
		innerExprs := partition.GetExpressions()

		var richOrdering *RichOrdering
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
					comparisonKeyValues, requestedOrdering, ProvidedSortOrderFixed)
				isReverse := ResolveComparisonDirection(comparisonParts)
				comparisonParts = AdjustFixedBindings(comparisonParts, isReverse)

				comparisonKeys := make([]values.Value, len(comparisonParts))
				for i, p := range comparisonParts {
					comparisonKeys[i] = p.Value
				}
				comparisonKeys = bakeMergeComparisonKeys(comparisonKeys, requestedOrdering, innerPlans[0].GetResultType())

				maxSize := 0
				if call.Context != nil {
					maxSize = call.Context.GetPlannerConfiguration().AttemptFailedInJoinAsUnionMaxSize
				}
				newRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
				inUnionPlan := plans.NewRecordQueryInUnionPlanWithMaxSize(
					innerPlans[0], bindingNames, comparisonKeys, isReverse, maxSize)
				inUnionPlan.SetInSources(inSources)
				call.YieldFinalExpression(NewPhysicalInUnionWrapper(
					inUnionPlan,
					expressions.NewPhysicalQuantifier(newRef),
				))
			}
		}

		if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
			newRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
			inUnionPlan := plans.NewRecordQueryInUnionPlan(
				innerPlans[0], bindingNames, nil, false)
			inUnionPlan.SetInSources(inSources)
			call.YieldFinalExpression(NewPhysicalInUnionWrapper(
				inUnionPlan,
				expressions.NewPhysicalQuantifier(newRef),
			))
		}
	}
}

// adjustBindingsForInUnion adjusts the inner ordering's bindings:
// fixed bindings whose comparison references an explode alias are
// promoted to directional (sorted) bindings. This enables the InUnion
// to merge-sort output by those keys.
func adjustBindingsForInUnion(
	ordering *RichOrdering,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	requestedOrdering *RequestedOrdering,
) *RichOrdering {
	if ordering == nil || len(ordering.GetKeys()) == 0 {
		return nil
	}

	reqMap := requestedOrdering.GetValueRequestedSortOrderMap()
	adjustedBM := make(map[values.Value][]OrderingBinding, len(ordering.GetBindingMap()))

	for val, bindings := range ordering.GetBindingMap() {
		sortOrder := SortOrderOf(bindings)
		if sortOrder.IsDirectional() {
			adjustedBM[val] = []OrderingBinding{SortedBinding(sortOrder)}
			continue
		}

		if !AreAllBindingsFixed(bindings) || HasMultipleFixedBindings(bindings) {
			adjustedBM[val] = bindings
			continue
		}

		b := SingleFixedBinding(bindings)
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
				adjustedBM[val] = []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
			} else {
				adjustedBM[val] = []OrderingBinding{SortedBinding(ProvidedSortOrderDescending)}
			}
		} else {
			adjustedBM[val] = []OrderingBinding{ChooseBinding()}
		}
	}

	return NewRichOrdering(adjustedBM, ordering.GetKeys(), ordering.IsDistinct())
}

var _ ImplementationRule = (*ImplementInUnionRule)(nil)
