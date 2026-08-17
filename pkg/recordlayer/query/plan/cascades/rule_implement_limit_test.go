package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func smallImplementRowType() *values.RecordType {
	return values.NewRecordType("SmallImplementRuleRow", false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
}

func mustSmallImplementConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct small implementation-rule fixture: " + err.Error())
	}
	return value
}

func smallImplementScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustSmallImplementConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, smallImplementRowType()))
}

func fireSmallImplementRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func TestImplementLimit_Fires(t *testing.T) {
	t.Parallel()

	scan := smallImplementScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := mustSmallImplementConstruct(expressions.NewLogicalLimitExpression(10, 0, scanQ))
	ref := expressions.InitialOf(lim)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit, got %T", plan)
	}
}

func TestImplementLimit_WithOffset(t *testing.T) {
	t.Parallel()

	scan := smallImplementScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := mustSmallImplementConstruct(expressions.NewLogicalLimitExpression(5, 20, scanQ))
	ref := expressions.InitialOf(lim)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit, got %T", plan)
	}

	explain := ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("Explain: %s", explain)
}

func TestImplementLimit_LimitOverScan(t *testing.T) {
	t.Parallel()

	scan := smallImplementScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	lim := mustSmallImplementConstruct(expressions.NewLogicalLimitExpression(10, 0, scanQ))
	ref := expressions.InitialOf(lim)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalLimit(plan) {
		t.Fatalf("expected physical limit at top, got %T", plan)
	}
}
