package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// containsPhysical walks ref recursively, returning true the first time
// predicate(member) returns true for any member in any reachable Reference.
func containsPhysical(ref *expressions.Reference, predicate func(expressions.RelationalExpression) bool) bool {
	visited := map[*expressions.Reference]bool{}
	var walk func(r *expressions.Reference) bool
	walk = func(r *expressions.Reference) bool {
		if r == nil || visited[r] {
			return false
		}
		visited[r] = true
		for _, m := range r.AllMembers() {
			if predicate(m) {
				return true
			}
			for _, q := range m.GetQuantifiers() {
				if walk(q.GetRangesOver()) {
					return true
				}
			}
		}
		return false
	}
	return walk(ref)
}

// allRules returns the EXPLORE-phase rule set for end-to-end planner
// tests. BatchA and DML rules fire during PLANNING via
// WithPlanningExpressionRules.
func allRules() []ExpressionRule {
	return DefaultExpressionRules()
}

// exploreAndVerify runs the planner on ref (EXPLORE + PLANNING) and fatals if
// it doesn't converge. Physical wrappers land in Members after PLANNING;
// containsPhysical uses AllMembers() to find them.
func exploreAndVerify(t *testing.T, ref *expressions.Reference, rules []ExpressionRule, ctx PlanContext) {
	t.Helper()
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	planningRules := append(BatchAExpressionRules(), DMLImplementationRules()...)
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(planningRules).
		WithImplementationRules(DefaultImplementationRules())
	_, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
}

// TestPlanner_NLJFromSelectWithTwoQuantifiers verifies that a Select
// with two quantifiers (simulating a JOIN over two scans) produces a
// *plans.RecordQueryNestedLoopJoinPlan after running through the planner with
// all rules.
func TestPlanner_NLJFromSelectWithTwoQuantifiers(t *testing.T) {
	t.Parallel()

	scan1 := phase3Scan(t, "Order")
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2 := phase3Scan(t, "Customer")
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	selValue, selErr := expressions.NewSelectExpression(
		values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "ORDER", Value: phase3FlowedValue(t, q1)},
			values.RecordConstructorField{Name: "CUSTOMER", Value: phase3FlowedValue(t, q2)},
		),
		[]expressions.Quantifier{q1, q2},
		nil,
	)
	sel := mustConstruct(t, selValue, selErr)
	ref := expressions.InitialOf(sel)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalNestedLoopJoin) && !containsPhysical(ref, IsPhysicalFlatMap) {
		t.Fatal("expected *plans.RecordQueryNestedLoopJoinPlan or *plans.RecordQueryFlatMapPlan in explored members")
	}
}

// TestPlanner_LimitProducesPhysicalPlan verifies that
// LogicalLimitExpression(10, 0, Scan) yields a *plans.RecordQueryLimitPlan
// after exploration.
func TestPlanner_LimitProducesPhysicalPlan(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	limitValue, limitErr := expressions.NewLogicalLimitExpression(10, 0, q)
	limit := mustConstruct(t, limitValue, limitErr)
	ref := expressions.InitialOf(limit)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalLimit) {
		t.Fatal("expected *plans.RecordQueryLimitPlan in explored members")
	}
}

// TestPlanner_GroupByProducesAggregation verifies that a GroupByExpression
// with one group key and one COUNT aggregate produces a
// physicalStreamingAggWrapper.
func TestPlanner_GroupByProducesAggregation(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	groupByValue, groupByErr := expressions.NewGroupByExpression(
		[]values.Value{phase3Field(t, q, 0)},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{
				Value: int64(1), Typ: values.NotNullLong,
			}},
		},
		q,
	)
	groupBy := mustConstruct(t, groupByValue, groupByErr)
	ref := expressions.InitialOf(groupBy)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalStreamingAgg) {
		// Dump member types for diagnostics.
		var types []string
		for _, m := range ref.Members() {
			types = append(types, fmt.Sprintf("%T", m))
		}
		t.Fatalf("expected *plans.RecordQueryStreamingAggregationPlan, found member types: %v", types)
	}
}

// TestPlanner_RecursiveUnionProducesDfsJoin verifies that a
// RecursiveUnionExpression with PREORDER strategy wrapping a
// TempTableScanExpression and a scan as the recursive step produces a
// plans.RecordQueryRecursiveDfsJoinPlan.
func TestPlanner_RecursiveUnionProducesDfsJoin(t *testing.T) {
	t.Parallel()

	tempAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	// Initial state: a simple scan.
	initialScan := phase3Scan(t, "Tree")
	initialRef := expressions.InitialOf(initialScan)
	initialQ := expressions.ForEachQuantifier(initialRef)

	// Recursive state: a temp table scan.
	tempScan := mustTempTableScan(t, tempAlias, phase3RowType())
	recursiveRef := expressions.InitialOf(tempScan)
	recursiveQ := expressions.ForEachQuantifier(recursiveRef)

	recUnion := mustRecursiveUnion(t,
		initialQ,
		recursiveQ,
		tempAlias,
		insertAlias,
		expressions.TraversalPreorder,
	)
	ref := expressions.InitialOf(recUnion)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalRecursiveDfsJoin) {
		t.Fatal("expected plans.RecordQueryRecursiveDfsJoinPlan in explored members")
	}
}

// TestPlanner_ProjectionOverScanProducesPhysicalProjection verifies
// that LogicalProjectionExpression over a Scan produces a bare
// *plans.RecordQueryProjectionPlan.
func TestPlanner_ProjectionOverScanProducesPhysicalProjection(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	projValue, projErr := expressions.NewLogicalProjectionExpression(
		[]values.Value{phase3Field(t, q, 1)},
		q,
	)
	proj := mustConstruct(t, projValue, projErr)
	ref := expressions.InitialOf(proj)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	isPhysicalProjection := func(expr expressions.RelationalExpression) bool {
		_, ok := expr.(*plans.RecordQueryProjectionPlan)
		return ok
	}
	if !containsPhysical(ref, isPhysicalProjection) {
		t.Fatal("expected *plans.RecordQueryProjectionPlan in explored members")
	}
}

// TestPlanner_InsertOverScanProducesPhysicalInsert verifies that
// InsertExpression over a Scan produces a bare *plans.RecordQueryInsertPlan (RFC-184 W2).
func TestPlanner_InsertOverScanProducesPhysicalInsert(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	insValue, insErr := expressions.NewInsertExpression(q, "Order", phase3RowType())
	ins := mustConstruct(t, insValue, insErr)
	ref := expressions.InitialOf(ins)

	rules := allRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalInsert) {
		t.Fatal("expected *plans.RecordQueryInsertPlan in explored members")
	}
}

// TestPlanner_DeleteOverScanProducesPhysicalDelete verifies that
// DeleteExpression over a Scan produces a bare *plans.RecordQueryDeletePlan (RFC-184 W2).
func TestPlanner_DeleteOverScanProducesPhysicalDelete(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	delValue, delErr := expressions.NewDeleteExpression(q, "Order")
	del := mustConstruct(t, delValue, delErr)
	ref := expressions.InitialOf(del)

	rules := allRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalDelete) {
		t.Fatal("expected *plans.RecordQueryDeletePlan in explored members")
	}
}

// TestPlanner_UnionOverTwoScansProducesPhysicalUnion verifies that a
// LogicalUnionExpression over two scans produces a physical union
// implementation. Physical yields land ONLY in FinalMembers and
// OptimizeGroup prunes finals to the winner (Java's prune-to-1), so a
// SPECIFIC union wrapper type is no longer guaranteed to be visible
// after Plan() — Go's extra concat RecordQueryUnionPlan competes with the
// Java-aligned unordered concat implementation and either may win. The wrapper's
// FORMATION is pinned by the direct-fire tests in
// rule_implement_union_test.go; this test pins that the full planner
// keeps SOME valid 2-child physical union as the winner.
func TestPlanner_UnionOverTwoScansProducesPhysicalUnion(t *testing.T) {
	t.Parallel()

	scan1 := phase3Scan(t, "Order")
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2 := phase3Scan(t, "Customer")
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	unionValue, unionErr := expressions.NewLogicalUnionExpression([]expressions.Quantifier{q1, q2})
	union := mustConstruct(t, unionValue, unionErr)
	ref := expressions.InitialOf(union)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	var unionPlan plans.RecordQueryPlan
	isPhysicalUnion := func(expr expressions.RelationalExpression) bool {
		switch w := expr.(type) {
		case *plans.RecordQueryUnionPlan:
			unionPlan = w.GetRecordQueryPlan()
			return true
		case *plans.RecordQueryUnorderedUnionPlan:
			unionPlan = w.GetRecordQueryPlan()
			return true
		}
		return false
	}
	if !containsPhysical(ref, isPhysicalUnion) {
		t.Fatal("expected a physical union implementation (Go concat or Java-aligned unordered) in explored members")
	}
	kids, ok := unionPlan.(interface {
		GetChildren() []plans.RecordQueryPlan
	})
	if !ok {
		t.Fatalf("winning union plan %T has no children accessor", unionPlan)
	}
	if got := len(kids.GetChildren()); got != 2 {
		t.Fatalf("winning union children: got %d, want 2", got)
	}
}

// TestPlanner_IntersectionOverTwoScansProducesPhysicalIntersection
// verifies that a LogicalIntersectionExpression over two scans
// produces a physical RecordQueryIntersectionPlan.
func TestPlanner_IntersectionOverTwoScansProducesPhysicalIntersection(t *testing.T) {
	t.Parallel()

	rt := values.NewRecordType("Order", false, []values.Field{{
		Name:      "ID",
		FieldType: values.NullableLong,
		Ordinal:   0,
	}})
	keyRootValue, keyRootErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("intersection_key"), rt)
	keyRoot := mustConstruct(t, keyRootValue, keyRootErr)
	comparisonKeyValue, comparisonKeyErr := values.ResolveFieldOrdinals(keyRoot, []int{0})
	comparisonKey := mustConstruct(t, comparisonKeyValue, comparisonKeyErr)
	scan1Value, scan1Err := plans.NewRecordQueryScanPlan([]string{"Order"}, rt, false)
	scan1 := mustConstruct(t, scan1Value, scan1Err).
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey([]values.Value{comparisonKey})
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2Value, scan2Err := plans.NewRecordQueryScanPlan([]string{"Order"}, rt, false)
	scan2 := mustConstruct(t, scan2Value, scan2Err).
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKey([]values.Value{comparisonKey})
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	intersectionValue, intersectionErr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{q1, q2},
		[]values.Value{comparisonKey},
	)
	intersection := mustConstruct(t, intersectionValue, intersectionErr)
	ref := expressions.InitialOf(intersection)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalIntersection) {
		t.Fatal("expected physical RecordQueryIntersectionPlan in explored members")
	}
}

// TestPlanner_SortOverScanStaysLogical verifies that a
// LogicalSortExpression over an unordered scan stays logical
// during exploration (sort is handled during PLANNING phase
// per Java's RemoveSortRule pattern, not during exploration).
func TestPlanner_SortOverScanStaysLogical(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	sortValue, sortErr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: phase3Field(t, q, 2), Reverse: false},
		},
		q,
	)
	sort := mustConstruct(t, sortValue, sortErr)
	ref := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	hasLogicalSort := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.LogicalSortExpression); ok {
			hasLogicalSort = true
			break
		}
	}
	if !hasLogicalSort {
		t.Fatal("sort should remain logical during exploration (no matching index)")
	}
}

// TestPlanner_FilterOverScanProducesPhysicalFilter verifies that a
// LogicalFilterExpression with a predicate over a scan produces a
// physical filter.
func TestPlanner_FilterOverScanProducesPhysicalFilter(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	filter := phase3Filter(t,
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		q,
	)
	ref := expressions.InitialOf(filter)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalFilter) {
		t.Fatal("expected a physical filter in explored members")
	}
}

// TestPlanner_TypeFilterOverScanProducesPhysicalTypeFilter verifies
// that a LogicalTypeFilterExpression over a scan produces a bare
// *plans.RecordQueryTypeFilterPlan (RFC-184 W2).
func TestPlanner_TypeFilterOverScanProducesPhysicalTypeFilter(t *testing.T) {
	t.Parallel()

	scan := mustFullUnorderedScan(t, []string{"Order", "Customer"}, phase3RowType())
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	typeFilterValue, typeFilterErr := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, q)
	typeFilter := mustConstruct(t, typeFilterValue, typeFilterErr)
	ref := expressions.InitialOf(typeFilter)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	isPhysicalTypeFilter := func(expr expressions.RelationalExpression) bool {
		_, ok := expr.(*plans.RecordQueryTypeFilterPlan)
		return ok
	}
	if !containsPhysical(ref, isPhysicalTypeFilter) {
		t.Fatal("expected *plans.RecordQueryTypeFilterPlan in explored members")
	}
}

// TestPlanner_DistinctOverScanElided verifies that a
// LogicalDistinctExpression over a scan is elided via the PLANNING
// phase (ImplementDistinctFinalRule) because the scan's authoritative LONG
// primary key makes physical record identity congruent with logical row
// equality.
func TestPlanner_DistinctOverScanElided(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("Order", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
	scan := mustFullUnorderedScan(t, []string{"Order"}, rowType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	distinctValue, distinctErr := expressions.NewLogicalDistinctExpression(q)
	distinct := mustConstruct(t, distinctValue, distinctErr)
	ref := expressions.InitialOf(distinct)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, &pkPlanContext{pk: map[string][]string{"Order": {"ID"}}}).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(1_000)
	best, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if best == nil {
		t.Fatal("expected a plan, got nil")
	}
	// This scan's safe PK proves logical row distinctness, so the Distinct
	// operator should be elided (no Distinct wrapper in the plan).
	explained := explainPlan(best)
	if containsDistinctInPlan(explained) {
		t.Fatalf("expected Distinct to be elided for scan, but got: %s", explained)
	}
}

func containsDistinctInPlan(explained string) bool {
	return containsSubstring(explained, "Distinct")
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func explainPlan(expr expressions.RelationalExpression) string {
	type explainer interface {
		Explain() string
	}
	type planGetter interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	if pg, ok := expr.(planGetter); ok {
		plan := pg.GetRecordQueryPlan()
		if e, ok := plan.(explainer); ok {
			return e.Explain()
		}
	}
	return ""
}

// TestPlanner_UpdateOverScanProducesPhysicalUpdate verifies that an
// UpdateExpression over a scan produces a bare *plans.RecordQueryUpdatePlan (RFC-184 W2).
func TestPlanner_UpdateOverScanProducesPhysicalUpdate(t *testing.T) {
	t.Parallel()

	scan := phase3Scan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	updateValue, updateErr := expressions.NewUpdateExpression(
		q, "Order", phase3RowType(), []expressions.UpdateTransform{
			{FieldPath: "STATUS", NewValue: values.LiteralValue("SHIPPED")},
		})
	update := mustConstruct(t, updateValue, updateErr)
	ref := expressions.InitialOf(update)

	rules := allRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalUpdate) {
		t.Fatal("expected *plans.RecordQueryUpdatePlan in explored members")
	}
}

// TestPlanner_RecursiveLevelUnionProducesPhysicalLevelUnion verifies
// that a RecursiveUnionExpression with TraversalLevel strategy
// produces a plans.RecordQueryRecursiveLevelUnionPlan.
func TestPlanner_RecursiveLevelUnionProducesPhysicalLevelUnion(t *testing.T) {
	t.Parallel()

	tempAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	initialScan := phase3Scan(t, "Tree")
	initialRef := expressions.InitialOf(initialScan)
	initialQ := expressions.ForEachQuantifier(initialRef)

	tempScan := mustTempTableScan(t, tempAlias, phase3RowType())
	recursiveRef := expressions.InitialOf(tempScan)
	recursiveQ := expressions.ForEachQuantifier(recursiveRef)

	recUnion := mustRecursiveUnion(t,
		initialQ,
		recursiveQ,
		tempAlias,
		insertAlias,
		expressions.TraversalLevel,
	)
	ref := expressions.InitialOf(recUnion)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalRecursiveLevelUnion) {
		t.Fatal("expected plans.RecordQueryRecursiveLevelUnionPlan in explored members")
	}
}

// TestPlanner_ValuesProducesPhysicalValues verifies that a
// LogicalValuesExpression produces a bare *plans.RecordQueryValuesPlan
// (RFC-184 W2 collapsed the physicalValuesWrapper).
func TestPlanner_ValuesProducesPhysicalValues(t *testing.T) {
	t.Parallel()

	valsValue, valsErr := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		&values.ConstantValue{Value: "hello", Typ: values.NotNullString},
	})
	vals := mustConstruct(t, valsValue, valsErr)
	ref := expressions.InitialOf(vals)

	rules := DefaultExpressionRules()
	exploreAndVerify(t, ref, rules, nil)

	isPhysicalValues := func(expr expressions.RelationalExpression) bool {
		_, ok := expr.(*plans.RecordQueryValuesPlan)
		return ok
	}
	if !containsPhysical(ref, isPhysicalValues) {
		t.Fatal("expected *plans.RecordQueryValuesPlan in explored members")
	}
}
