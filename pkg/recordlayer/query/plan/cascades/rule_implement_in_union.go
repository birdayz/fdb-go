package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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
