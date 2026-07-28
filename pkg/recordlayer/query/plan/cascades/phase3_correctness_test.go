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

	scan1 := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2 := expressions.NewFullUnorderedScanExpression([]string{"Customer"}, values.UnknownType)
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{q1, q2},
		nil,
	)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	limit := expressions.NewLogicalLimitExpression(10, 0, q)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.TypeString}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}},
		},
		q,
	)
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
	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Tree"}, values.UnknownType)
	initialRef := expressions.InitialOf(initialScan)
	initialQ := expressions.ForEachQuantifier(initialRef)

	// Recursive state: a temp table scan.
	tempScan := expressions.NewTempTableScanExpression(tempAlias)
	recursiveRef := expressions.InitialOf(tempScan)
	recursiveQ := expressions.ForEachQuantifier(recursiveRef)

	recUnion := expressions.NewRecursiveUnionExpression(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "ID", Typ: values.NullableLong}},
		q,
	)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	ins := expressions.NewInsertExpression(q, "Order", values.UnknownType)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	del := expressions.NewDeleteExpression(q, "Order")
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

	scan1 := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2 := expressions.NewFullUnorderedScanExpression([]string{"Customer"}, values.UnknownType)
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{q1, q2})
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

	rt := &values.RecordType{
		RecordName: "Order",
		Fields: []values.Field{{
			Name:      "ID",
			FieldType: values.NullableLong,
			Ordinal:   0,
		}},
	}
	comparisonKey := &values.FieldValue{Field: "ID", Typ: values.NullableLong}
	scan1 := plans.NewRecordQueryScanPlan([]string{"Order"}, rt, false).
		WithPrimaryKey([]values.Value{comparisonKey})
	scan1Ref := expressions.InitialOf(scan1)
	q1 := expressions.ForEachQuantifier(scan1Ref)

	scan2 := plans.NewRecordQueryScanPlan([]string{"Order"}, rt, false).
		WithPrimaryKey([]values.Value{comparisonKey})
	scan2Ref := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	intersection := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{q1, q2},
		[]values.Value{comparisonKey},
	)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "CREATED_AT", Typ: values.NullableLong}, Reverse: false},
		},
		q,
	)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	typeFilter := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, q)
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
// phase (ImplementDistinctFinalRule) because scans already produce
// distinct records (DistinctRecordsProperty is true for scans).
func TestPlanner_DistinctOverScanElided(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(q)
	ref := expressions.InitialOf(distinct)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, nil).
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
	// Scans produce distinct records, so the Distinct operator should
	// be elided (no Distinct wrapper in the plan).
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	update := expressions.NewUpdateExpression(q, "Order", []expressions.UpdateTransform{
		{FieldPath: "STATUS", NewValue: &values.ConstantValue{Value: "SHIPPED", Typ: values.TypeString}},
	})
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

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Tree"}, values.UnknownType)
	initialRef := expressions.InitialOf(initialScan)
	initialQ := expressions.ForEachQuantifier(initialRef)

	tempScan := expressions.NewTempTableScanExpression(tempAlias)
	recursiveRef := expressions.InitialOf(tempScan)
	recursiveQ := expressions.ForEachQuantifier(recursiveRef)

	recUnion := expressions.NewRecursiveUnionExpression(
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

	vals := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
		&values.ConstantValue{Value: "hello", Typ: values.TypeString},
	})
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
