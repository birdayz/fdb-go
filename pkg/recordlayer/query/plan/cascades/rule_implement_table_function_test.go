package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func newTestStreamValue() values.Value {
	return &values.ConstantValue{
		Typ:   values.NewPrimitiveType(values.TypeCodeInt, false),
		Value: 42,
	}
}

func TestImplementTableFunction_Fires(t *testing.T) {
	t.Parallel()

	stream := newTestStreamValue()
	tf := expressions.NewTableFunctionExpression(stream)
	ref := expressions.InitialOf(tf)

	yielded := FireExpressionRule(NewImplementTableFunctionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTableFunctionRule yielded %d, want 1", len(yielded))
	}

	// RFC-184 W2: ImplementTableFunctionRule yields the bare *plans.RecordQueryTableFunctionPlan.
	plan, ok := yielded[0].(*plans.RecordQueryTableFunctionPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTableFunctionPlan", yielded[0])
	}
	if plan.GetStreamValue() != stream {
		t.Fatal("plan stream value mismatch")
	}
}

func TestImplementTableFunction_ViaPlanner(t *testing.T) {
	t.Parallel()

	stream := newTestStreamValue()
	tf := expressions.NewTableFunctionExpression(stream)
	ref := expressions.InitialOf(tf)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	found := false
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*plans.RecordQueryTableFunctionPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("planner did not produce a physical TableFunction member")
	}
}

func TestImplementTableFunction_PlanOutput(t *testing.T) {
	t.Parallel()

	stream := newTestStreamValue()
	tf := expressions.NewTableFunctionExpression(stream)
	ref := expressions.InitialOf(tf)

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

	rqp, ok := plan.(*plans.RecordQueryTableFunctionPlan)
	if !ok {
		t.Fatalf("plan = %T, want *plans.RecordQueryTableFunctionPlan", plan)
	}
	explain := rqp.Explain()
	if explain == "" {
		t.Fatal("Explain returned empty")
	}
	t.Logf("TableFunction Explain: %s", explain)
}

func TestImplementTableFunction_NilStreamValue(t *testing.T) {
	t.Parallel()

	tf := expressions.NewTableFunctionExpression(nil)
	ref := expressions.InitialOf(tf)

	yielded := FireExpressionRule(NewImplementTableFunctionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTableFunctionRule yielded %d for nil stream, want 1", len(yielded))
	}

	plan := yielded[0].(*plans.RecordQueryTableFunctionPlan)
	if plan.GetStreamValue() != nil {
		t.Fatal("expected nil stream value")
	}
	if plan.Explain() != "TableFunction(<nil>)" {
		t.Fatalf("Explain = %q", plan.Explain())
	}
}
