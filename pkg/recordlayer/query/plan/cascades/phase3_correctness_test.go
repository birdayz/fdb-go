package cascades

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
		for _, m := range r.Members() {
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

// allRules composes DefaultExpressionRules + BatchAExpressionRules +
// DMLImplementationRules into the full rule set used for end-to-end
// planner tests.
func allRules() []ExpressionRule {
	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, DMLImplementationRules()...)
	return rules
}

// exploreAndVerify runs the planner on ref and fatals if it doesn't converge.
func exploreAndVerify(t *testing.T, ref *expressions.Reference, rules []ExpressionRule, ctx PlanContext) {
	t.Helper()
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	p := NewPlanner(rules, ctx)
	_, conv := p.Explore(ref)
	if !conv {
		t.Fatal("planner did not converge")
	}
}

// TestPlanner_NLJFromSelectWithTwoQuantifiers verifies that a Select
// with two quantifiers (simulating a JOIN over two scans) produces a
// physicalNestedLoopJoinWrapper after running through the planner with
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalNestedLoopJoin) {
		t.Fatal("expected physicalNestedLoopJoinWrapper in explored members")
	}
}

// TestPlanner_LimitProducesPhysicalPlan verifies that
// LogicalLimitExpression(10, 0, Scan) yields a physicalLimitWrapper
// after exploration.
func TestPlanner_LimitProducesPhysicalPlan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	limit := expressions.NewLogicalLimitExpression(10, 0, q)
	ref := expressions.InitialOf(limit)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalLimit) {
		t.Fatal("expected physicalLimitWrapper in explored members")
	}
}

// TestPlanner_GroupByProducesAggregation verifies that a GroupByExpression
// with one group key and one COUNT aggregate produces either a
// physicalHashAggWrapper or physicalStreamingAggWrapper.
func TestPlanner_GroupByProducesAggregation(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.TypeString}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}},
		},
		q,
	)
	ref := expressions.InitialOf(groupBy)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	exploreAndVerify(t, ref, rules, nil)

	foundHash := containsPhysical(ref, IsPhysicalHashAgg)
	foundStreaming := containsPhysical(ref, IsPhysicalStreamingAgg)
	if !foundHash && !foundStreaming {
		// Dump member types for diagnostics.
		var types []string
		for _, m := range ref.Members() {
			types = append(types, fmt.Sprintf("%T", m))
		}
		t.Fatalf("expected physicalHashAggWrapper or physicalStreamingAggWrapper, found member types: %v", types)
	}
}

// TestPlanner_RecursiveUnionProducesDfsJoin verifies that a
// RecursiveUnionExpression with PREORDER strategy wrapping a
// TempTableScanExpression and a scan as the recursive step produces a
// physicalRecursiveDfsJoinWrapper.
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	exploreAndVerify(t, ref, rules, nil)

	if !containsPhysical(ref, IsPhysicalRecursiveDfsJoin) {
		t.Fatal("expected physicalRecursiveDfsJoinWrapper in explored members")
	}
}

// TestPlanner_ProjectionOverScanProducesPhysicalProjection verifies
// that LogicalProjectionExpression over a Scan produces a
// physicalProjectionWrapper.
func TestPlanner_ProjectionOverScanProducesPhysicalProjection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "ID", Typ: values.TypeInt}},
		q,
	)
	ref := expressions.InitialOf(proj)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	exploreAndVerify(t, ref, rules, nil)

	isPhysicalProjection := func(expr expressions.RelationalExpression) bool {
		_, ok := expr.(*physicalProjectionWrapper)
		return ok
	}
	if !containsPhysical(ref, isPhysicalProjection) {
		t.Fatal("expected physicalProjectionWrapper in explored members")
	}
}

// TestPlanner_InsertOverScanProducesPhysicalInsert verifies that
// InsertExpression over a Scan produces a physicalInsertWrapper.
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
		t.Fatal("expected physicalInsertWrapper in explored members")
	}
}

// TestPlanner_DeleteOverScanProducesPhysicalDelete verifies that
// DeleteExpression over a Scan produces a physicalDeleteWrapper.
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
		t.Fatal("expected physicalDeleteWrapper in explored members")
	}
}
