package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// javaBindingCounter is a rule whose Java matcher binds more than once per rule
// call (once per plan partition, quantifier, combination or permutation) where
// the Go rule binds once and enumerates in OnMatch. javaBindings is Java's
// binding count for expr, the number MaxNumMatchesPerRuleCall bounds.
type javaBindingCounter interface {
	javaBindings(expr expressions.RelationalExpression) int
}

// ruleCallMatches is what MaxNumMatchesPerRuleCall bounds for one call of rule
// on expr: the rule's Java binding count, else its goBindings. Java counts each
// binding before onMatch runs, so the count does not depend on OnMatch's guards.
func ruleCallMatches(rule any, expr expressions.RelationalExpression, goBindings int) int {
	if adapter, ok := rule.(*expressionRuleAdapter); ok {
		rule = adapter.rule
	}
	if counter, ok := rule.(javaBindingCounter); ok {
		return counter.javaBindings(expr)
	}
	return goBindings
}

// javaPlanPartitions is Java's Reference.toPlanPartitions: the plans of ref
// partitioned by ordering, distinct records, stored record and primary key.
func javaPlanPartitions(ref *expressions.Reference) []*PlanPartition {
	if ref == nil {
		return nil
	}
	computeRefPlanProperties(ref)
	return withPlans(RollUpPlanPartitions(ToPlanPartitions(ref), properties.PropRichOrdering,
		properties.PropDistinctRecords, properties.PropStoredRecord, properties.PropPrimaryKey))
}

// javaStoredRecordPartitions is filterPlanPartitions(storedRecord, …) over
// javaPlanPartitions.
func javaStoredRecordPartitions(ref *expressions.Reference) []*PlanPartition {
	return FilterPlanPartitions(javaPlanPartitions(ref), func(p *PlanPartition) bool {
		return p.GetPartitionPropertiesMap().GetBool(properties.PropStoredRecord)
	})
}

func withPlans(partitions []*PlanPartition) []*PlanPartition {
	return FilterPlanPartitions(partitions, func(p *PlanPartition) bool {
		return len(p.GetPhysicalExpressions()) > 0
	})
}

// onlyQuantifier returns expr's quantifier when it has exactly one (Java's
// only/exactly) of kind, else nil.
func onlyQuantifier(expr expressions.RelationalExpression, kind expressions.QuantifierKind) *expressions.Reference {
	qs := expr.GetQuantifiers()
	if len(qs) != 1 || qs[0].Kind() != kind {
		return nil
	}
	return qs[0].GetRangesOver()
}

func countQuantifiers(expr expressions.RelationalExpression, kind expressions.QuantifierKind) int {
	n := 0
	for _, q := range expr.GetQuantifiers() {
		if q.Kind() == kind {
			n++
		}
	}
	return n
}

// ImplementFilterRule.java: logicalFilterExpression(all(anyCompensatablePredicate()),
// only(forEachQuantifierOverRef(planPartitions(any(anyPlanPartition()))))).
func (r *ImplementFilterRule) javaBindings(expr expressions.RelationalExpression) int {
	f, ok := expr.(*expressions.LogicalFilterExpression)
	if !ok {
		return 0
	}
	for _, pred := range f.GetPredicates() {
		if predicateContainsUncompensatableValues(pred) {
			return 0
		}
	}
	return len(javaPlanPartitions(onlyQuantifier(f, expressions.QuantifierForEach)))
}

// ImplementTypeFilterRule.java: logicalTypeFilterExpression(exactly(
// forEachQuantifierOverRef(planPartitions(filterPlanPartitions(storedRecord, any(…)))))).
func (r *ImplementTypeFilterRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.LogicalTypeFilterExpression); !ok {
		return 0
	}
	return len(javaStoredRecordPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// ImplementDistinctRule.java: logicalDistinctExpression(only(
// forEachQuantifierOverRef(planPartitions(filterPlanPartitions(storedRecord, any(…)))))).
func (r *ImplementDistinctFinalRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.LogicalDistinctExpression); !ok {
		return 0
	}
	return len(javaStoredRecordPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// ImplementSimpleSelectRule.java: selectExpression(all(anyCompensatablePredicate()),
// exactly(anyQuantifierOverRef(planPartitions(any(anyPlanPartition()))))).
func (r *ImplementSimpleSelectRule) javaBindings(expr expressions.RelationalExpression) int {
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		return 0
	}
	for _, pred := range sel.GetPredicates() {
		if predicateContainsUncompensatableValues(pred) {
			return 0
		}
	}
	qs := sel.GetQuantifiers()
	if len(qs) != 1 {
		return 0
	}
	return len(javaPlanPartitions(qs[0].GetRangesOver()))
}

// ImplementInsertRule.java: insertExpression(only(forEachQuantifierOverRef(
// planPartitions(any(anyPlanPartition()))))).
func (r *ImplementInsertRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.InsertExpression); !ok {
		return 0
	}
	return len(javaPlanPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// ImplementTempTableInsertRule.java: as ImplementInsertRule.
func (r *ImplementTempTableInsertRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.TempTableInsertExpression); !ok {
		return 0
	}
	return len(javaPlanPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// ImplementDeleteRule.java: deleteExpression(only(forEachQuantifierOverRef(
// planPartitions(filterPlanPartitions(storedRecord, any(…)))))).
func (r *ImplementDeleteRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.DeleteExpression); !ok {
		return 0
	}
	return len(javaStoredRecordPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// ImplementUpdateRule.java: as ImplementDeleteRule over an UpdateExpression.
func (r *ImplementUpdateRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.UpdateExpression); !ok {
		return 0
	}
	return len(javaStoredRecordPartitions(onlyQuantifier(expr, expressions.QuantifierForEach)))
}

// RemoveSortRule.java: logicalSortExpression(exactly(forEachQuantifierOverRef(planPartitions(
// rollUpPartitionsTo(any(anyPlanPartition()), {ordering, distinctRecords, primaryKey}))))).
func (r *ImplementSortRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.LogicalSortExpression); !ok {
		return 0
	}
	ref := onlyQuantifier(expr, expressions.QuantifierForEach)
	if ref == nil {
		return 0
	}
	computeRefPlanProperties(ref)
	return len(withPlans(RollUpPlanPartitions(ToPlanPartitions(ref), properties.PropRichOrdering,
		properties.PropDistinctRecords, properties.PropPrimaryKey)))
}

// PartitionSelectRule.java: selectExpression(combinations(all(anyQuantifier()), 0, n)),
// one binding per subset of fewer than all n quantifiers: 2^n - 1.
func (r *PartitionSelectRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.SelectExpression); !ok {
		return 0
	}
	n := len(expr.GetQuantifiers())
	if n >= 31 {
		return math.MaxInt32
	}
	return 1<<n - 1
}

// PushRequestedOrderingThroughSelectRule.java: selectExpression(any(forEachQuantifierOverRef(anyRef()))).
func (r *PushRequestedOrderingThroughSelectRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.SelectExpression); !ok {
		return 0
	}
	return countQuantifiers(expr, expressions.QuantifierForEach)
}

// PushRequestedOrderingThroughSelectExistentialRule.java:
// selectExpression(any(existentialQuantifierOverRef(anyRef()))).
func (r *PushRequestedOrderingThroughSelectExistentialRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.SelectExpression); !ok {
		return 0
	}
	return countQuantifiers(expr, expressions.QuantifierExistential)
}

// ImplementNestedLoopJoinRule.java: selectExpression(exactlyInAnyOrder(outer, inner))
// .where(canBeImplemented()): both orders of two quantifiers whose references
// all hold plans. Go's planner fires the second order as its swapped firing.
func (r *ImplementNestedLoopJoinRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.SelectExpression); !ok {
		return 0
	}
	qs := expr.GetQuantifiers()
	if len(qs) != 2 {
		return 0
	}
	for _, q := range qs {
		if ref := q.GetRangesOver(); ref == nil || len(ref.FinalMembers()) == 0 {
			return 0
		}
	}
	return 2
}

// PartitionBinarySelectRule.java: selectExpression(exactlyInAnyOrder(anyQuantifier(), anyQuantifier())).
func (r *PartitionBinarySelectRule) javaBindings(expr expressions.RelationalExpression) int {
	if _, ok := expr.(*expressions.SelectExpression); !ok {
		return 0
	}
	if len(expr.GetQuantifiers()) != 2 {
		return 0
	}
	return 2
}
