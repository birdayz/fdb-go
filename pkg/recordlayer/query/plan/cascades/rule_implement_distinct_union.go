package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctUnionRule implements Distinct(Union(legs...)) as a
// merge-sorted union plan. It matches LogicalDistinctExpression over
// LogicalUnionExpression, finds compatible orderings across all union
// legs, and creates a RecordQueryMergeSortUnionPlan with deduplication.
//
// Ports Java's ImplementDistinctUnionRule. The full algorithm:
//  1. Get requested orderings from planner constraints
//  2. For each cross-product combo of per-leg plan partitions:
//     a. Verify common primary key across all legs
//     b. Extract ordering from each leg's partition
//     c. Incrementally merge orderings (bail on incompatibility)
//     d. Verify PK values are covered by merged ordering
//     e. Enumerate comparison keys satisfying the requested ordering
//     f. Create MergeSortUnionPlan with comparison keys
type ImplementDistinctUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctUnionRule() *ImplementDistinctUnionRule {
	return &ImplementDistinctUnionRule{
		matcher: &logicalDistinctMatcher{},
	}
}

func (r *ImplementDistinctUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctUnionRule) OnMatch(call *ImplementationRuleCall) {
	distinct := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)

	distinctQs := distinct.GetQuantifiers()
	if len(distinctQs) != 1 {
		return
	}
	unionRef := distinctQs[0].GetRangesOver()
	if unionRef == nil {
		return
	}

	var unionExpr *expressions.LogicalUnionExpression
	for _, m := range unionRef.AllMembers() {
		if u, ok := m.(*expressions.LogicalUnionExpression); ok {
			unionExpr = u
			break
		}
	}
	if unionExpr == nil {
		return
	}

	unionQs := unionExpr.GetQuantifiers()
	if len(unionQs) < 2 {
		return
	}

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		requestedOrderings = []*RequestedOrdering{PreserveOrdering()}
	}

	legPartitions := make([][]*PlanPartition, len(unionQs))
	for i, q := range unionQs {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		partitions := ToPlanPartitions(ref)
		var filtered []*PlanPartition
		for _, p := range partitions {
			if p.IsStoredRecord() && p.HasPrimaryKey() {
				filtered = append(filtered, p)
			}
		}
		allExcept := AllAttributesExcept(properties.PropDistinctRecords)
		rolled := RollUpPlanPartitions(filtered, allExcept...)
		if len(rolled) == 0 {
			return
		}
		legPartitions[i] = rolled
	}

	combos := CrossProduct(legPartitions)

	for _, requestedOrdering := range requestedOrderings {
		for _, q := range unionQs {
			if ref := q.GetRangesOver(); ref != nil {
				call.PushConstraint(ref, []*RequestedOrdering{requestedOrdering})
			}
		}

		for _, combo := range combos {
			r.tryYieldPlan(call, unionQs, combo, requestedOrdering)
		}
	}
}

func (r *ImplementDistinctUnionRule) tryYieldPlan(
	call *ImplementationRuleCall,
	unionQs []expressions.Quantifier,
	combo []*PlanPartition,
	requestedOrdering *RequestedOrdering,
) {
	pkValues := getCommonPK(combo)
	if pkValues == nil {
		return
	}

	orderings := make([]*RichOrdering, len(combo))
	for i, partition := range combo {
		o := partition.GetOrdering()
		bm := make(map[values.Value][]OrderingBinding)
		for _, k := range o.Keys {
			bm[k] = []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
		}
		orderings[i] = NewRichOrdering(bm, o.Keys, false)
	}

	var mergedOrdering *RichOrdering
	for i, o := range orderings {
		if i == 0 {
			mergedOrdering = CreateUnionOrdering(o)
		} else {
			merged := MergeOrderings(mergedOrdering, o)
			if !isPrimaryKeyCompatibleWithOrdering(pkValues, merged) {
				return
			}
			mergedOrdering = merged
		}
	}

	if mergedOrdering == nil {
		return
	}

	var childPlans []plans.RecordQueryPlan
	var newQuantifiers []expressions.Quantifier
	for i, partition := range combo {
		planExprs := partition.GetExpressions()
		if len(planExprs) == 0 {
			return
		}
		newRef := call.MemoizeFinalExpressionsFromOther(
			unionQs[i].GetRangesOver(), planExprs)
		newQuantifiers = append(newQuantifiers,
			expressions.NewPhysicalQuantifier(newRef))
		for _, pe := range planExprs {
			if ph, ok := pe.(physicalPlanExpression); ok {
				childPlans = append(childPlans, ph.GetRecordQueryPlan())
			}
		}
	}

	if len(childPlans) < 2 {
		return
	}

	satisfyingKeys := mergedOrdering.EnumerateSatisfyingComparisonKeyValues(requestedOrdering)
	for _, comparisonKeyValues := range satisfyingKeys {
		comparisonParts := mergedOrdering.DirectionalOrderingParts(
			comparisonKeyValues, requestedOrdering, ProvidedSortOrderFixed)
		isReverse := ResolveComparisonDirection(comparisonParts)
		comparisonParts = AdjustFixedBindings(comparisonParts, isReverse)

		comparisonKeys := make([]values.Value, len(comparisonParts))
		for i, p := range comparisonParts {
			comparisonKeys[i] = p.Value
		}

		unionPlan := plans.NewRecordQueryMergeSortUnionPlan(
			childPlans, comparisonKeys, isReverse, true)
		wrapper := NewPhysicalMergeSortUnionWrapper(unionPlan, newQuantifiers)
		call.YieldFinalExpression(wrapper)
	}
}

func isPrimaryKeyCompatibleWithOrdering(pkValues []values.Value, ordering *RichOrdering) bool {
	if ordering == nil || len(ordering.GetKeys()) == 0 {
		return len(pkValues) == 0
	}
	orderingKeySet := make(map[string]struct{}, len(ordering.GetKeys()))
	for _, k := range ordering.GetKeys() {
		orderingKeySet[values.ExplainValue(k)] = struct{}{}
	}
	for _, pkVal := range pkValues {
		if _, ok := orderingKeySet[values.ExplainValue(pkVal)]; !ok {
			return false
		}
	}
	return true
}

func getCommonPK(partitions []*PlanPartition) []values.Value {
	if len(partitions) == 0 {
		return nil
	}
	first := partitions[0].GetPartitionPropertyValue(properties.PropPrimaryKey)
	if first == nil {
		return nil
	}
	firstPK, ok := first.([]values.Value)
	if !ok {
		return nil
	}
	for _, p := range partitions[1:] {
		other := p.GetPartitionPropertyValue(properties.PropPrimaryKey)
		if other == nil {
			return nil
		}
		otherPK, ok := other.([]values.Value)
		if !ok || len(otherPK) != len(firstPK) {
			return nil
		}
		for i := range firstPK {
			if !valuesEqual(firstPK[i], otherPK[i]) {
				return nil
			}
		}
	}
	return firstPK
}

var _ ImplementationRule = (*ImplementDistinctUnionRule)(nil)

type logicalDistinctMatcher struct{}

func (m *logicalDistinctMatcher) RootType() string { return "LogicalDistinctExpression" }

func (m *logicalDistinctMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalDistinctExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
