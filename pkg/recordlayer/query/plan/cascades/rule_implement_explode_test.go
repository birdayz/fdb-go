package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func newTestArrayValue(elems ...any) values.Value {
	elementType := values.Type(values.NotNullLong)
	if len(elems) > 0 {
		if _, ok := elems[0].(string); ok {
			elementType = values.NotNullString
		}
	}
	return &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: elementType},
		Value: elems,
	}
}

func mustImplementExplodeConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct implement-explode fixture: " + err.Error())
	}
	return value
}

func TestImplementExplode_Fires(t *testing.T) {
	t.Parallel()

	collVal := newTestArrayValue(1, 2, 3)
	explode := mustImplementExplodeConstruct(expressions.NewExplodeExpression(collVal))
	ref := expressions.InitialOf(explode)

	yielded := mustFireExpressionRule(t, NewImplementExplodeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementExplodeRule yielded %d, want 1", len(yielded))
	}

	// RFC-184 W2: ImplementExplodeRule yields the bare *plans.RecordQueryExplodePlan.
	plan, ok := yielded[0].(*plans.RecordQueryExplodePlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryExplodePlan", yielded[0])
	}
	if plan.GetCollectionValue() != collVal {
		t.Fatal("plan collection value mismatch")
	}
}

func TestImplementExplode_ViaPlanner(t *testing.T) {
	t.Parallel()

	collVal := newTestArrayValue("a", "b")
	explode := mustImplementExplodeConstruct(expressions.NewExplodeExpression(collVal))
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
		if _, ok := m.(*plans.RecordQueryExplodePlan); ok {
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
	explode := mustImplementExplodeConstruct(expressions.NewExplodeExpression(collVal))
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

	rqp, ok := plan.(*plans.RecordQueryExplodePlan)
	if !ok {
		t.Fatalf("plan = %T, want *plans.RecordQueryExplodePlan", plan)
	}
	explain := rqp.Explain()
	if explain == "" {
		t.Fatal("Explain returned empty")
	}
	t.Logf("Explode Explain: %s", explain)
}

func TestImplementExplode_NilCollectionRejectedAtLogicalBoundary(t *testing.T) {
	t.Parallel()

	explode, err := expressions.NewExplodeExpression(nil)
	if err == nil {
		t.Fatal("nil collection produced an executable logical Explode")
	}
	if explode != nil {
		t.Fatalf("nil collection returned %T together with error %v", explode, err)
	}
}
