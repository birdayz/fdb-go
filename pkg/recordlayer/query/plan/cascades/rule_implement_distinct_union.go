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
// Ports Java's ImplementDistinctUnionRule (simplified — no cross-product
// skip optimization, no RequestedOrdering constraint propagation).
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
	for _, combo := range combos {
		pkValues := getCommonPK(combo)
		if pkValues == nil {
			continue
		}

		var childPlans []plans.RecordQueryPlan
		var newQuantifiers []expressions.Quantifier
		for i, partition := range combo {
			planExprs := partition.GetExpressions()
			if len(planExprs) == 0 {
				break
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
			continue
		}

		comparisonKeys := make([]values.Value, len(pkValues))
		copy(comparisonKeys, pkValues)

		unionPlan := plans.NewRecordQueryMergeSortUnionPlan(
			childPlans, comparisonKeys, false, true)
		wrapper := NewPhysicalMergeSortUnionWrapper(unionPlan, newQuantifiers)
		call.YieldFinalExpression(wrapper)
	}
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
