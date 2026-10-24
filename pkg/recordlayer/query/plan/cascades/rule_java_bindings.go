package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// javaBindingCounter is a rule whose Java matcher binds per plan partition,
// quantifier, combination or permutation; javaBindings is that count for expr.
type javaBindingCounter interface {
	javaBindings(expr expressions.RelationalExpression) int
}

// ruleCallMatches is what MaxNumMatchesPerRuleCall bounds for one rule call:
// Java's count where the rule reports it, else the Go bindings.
func ruleCallMatches(rule any, expr expressions.RelationalExpression, goBindings int) int {
	if adapter, ok := rule.(*expressionRuleAdapter); ok {
		rule = adapter.rule
	}
	if counter, ok := rule.(javaBindingCounter); ok {
		return counter.javaBindings(expr)
	}
	return goBindings
}

// javaPlanPartitions is Java's Reference.toPlanPartitions: the final plans of ref
// partitioned by ordering, distinct records, stored record and primary key.
func javaPlanPartitions(ref *expressions.Reference) []*PlanPartition {
	return javaPartitions(ref, properties.PropRichOrdering, properties.PropDistinctRecords,
		properties.PropStoredRecord, properties.PropPrimaryKey)
}

// javaPartitions partitions ref's final plans by props. Java's properties map
// holds final plans only, and the count must not touch the map the rules read,
// so it is built afresh.
func javaPartitions(ref *expressions.Reference, props ...*properties.ExpressionProperty) []*PlanPartition {
	if ref == nil {
		return nil
	}
	pm := NewPlanPropertiesMap()
	for _, m := range ref.FinalMembers() {
		if plan, ok := m.(physicalPlanExpression); ok {
			pm.Add(plan)
		}
	}
	return withPlans(RollUpPlanPartitions(toPartitionsFromMap(pm), props...))
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

// allPredicatesCompensatable is Java's all(anyCompensatablePredicate()): no
// predicate carries an index-only value.
func allPredicatesCompensatable(preds []predicates.QueryPredicate) bool {
	for _, pred := range preds {
		if predicateContainsUncompensatableValues(pred) {
			return false
		}
	}
	return true
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
	if !ok || !allPredicatesCompensatable(f.GetPredicates()) {
		return 0
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
	if !ok || !allPredicatesCompensatable(sel.GetPredicates()) {
		return 0
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
	return len(javaPartitions(onlyQuantifier(expr, expressions.QuantifierForEach),
		properties.PropRichOrdering, properties.PropDistinctRecords, properties.PropPrimaryKey))
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
