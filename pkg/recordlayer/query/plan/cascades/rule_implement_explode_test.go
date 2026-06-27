package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func newTestArrayValue(elems ...any) values.Value {
	return &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: values.UnknownType},
		Value: elems,
	}
}

func TestImplementExplode_Fires(t *testing.T) {
	t.Parallel()

	collVal := newTestArrayValue(1, 2, 3)
	explode := expressions.NewExplodeExpression(collVal)
	ref := expressions.InitialOf(explode)

	yielded := FireExpressionRule(NewImplementExplodeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementExplodeRule yielded %d, want 1", len(yielded))
	}

	wrap, ok := yielded[0].(*physicalExplodeWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalExplodeWrapper", yielded[0])
	}
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryExplodePlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryExplodePlan", wrap.GetRecordQueryPlan())
	}
	if plan.GetCollectionValue() != collVal {
		t.Fatal("plan collection value mismatch")
	}
}

func TestImplementExplode_ViaPlanner(t *testing.T) {
	t.Parallel()

	collVal := newTestArrayValue("a", "b")
	explode := expressions.NewExplodeExpression(collVal)
	ref := expressions.InitialOf(explode)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	found := false
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*physicalExplodeWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("planner did not produce a physical Explode member")
	}
}

func TestImplementExplode_PlanOutput(t *testing.T) {
	t.Parallel()

	collVal := newTestArrayValue(42)
	explode := expressions.NewExplodeExpression(collVal)
	ref := expressions.InitialOf(explode)

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

	wrap, ok := plan.(*physicalExplodeWrapper)
	if !ok {
		t.Fatalf("plan = %T, want *physicalExplodeWrapper", plan)
	}
	rqp, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryExplodePlan)
	if !ok {
		t.Fatalf("inner plan = %T, want *RecordQueryExplodePlan", wrap.GetRecordQueryPlan())
	}
	explain := rqp.Explain()
	if explain == "" {
		t.Fatal("Explain returned empty")
	}
	t.Logf("Explode Explain: %s", explain)
}

func TestImplementExplode_NilCollectionValue(t *testing.T) {
	t.Parallel()

	explode := expressions.NewExplodeExpression(nil)
	ref := expressions.InitialOf(explode)

	yielded := FireExpressionRule(NewImplementExplodeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementExplodeRule yielded %d for nil collection, want 1", len(yielded))
	}

	wrap := yielded[0].(*physicalExplodeWrapper)
	plan := wrap.GetRecordQueryPlan().(*plans.RecordQueryExplodePlan)
	if plan.GetCollectionValue() != nil {
		t.Fatal("expected nil collection value")
	}
	if plan.Explain() != "Explode(<nil>)" {
		t.Fatalf("Explain = %q", plan.Explain())
	}
}
