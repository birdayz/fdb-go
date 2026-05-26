package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	wrap, ok := yielded[0].(*physicalTableFunctionWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTableFunctionWrapper", yielded[0])
	}
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTableFunctionPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryTableFunctionPlan", wrap.GetRecordQueryPlan())
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
		if _, ok := m.(*physicalTableFunctionWrapper); ok {
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

	wrap, ok := plan.(*physicalTableFunctionWrapper)
	if !ok {
		t.Fatalf("plan = %T, want *physicalTableFunctionWrapper", plan)
	}
	rqp, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTableFunctionPlan)
	if !ok {
		t.Fatalf("inner plan = %T, want *RecordQueryTableFunctionPlan", wrap.GetRecordQueryPlan())
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

	wrap := yielded[0].(*physicalTableFunctionWrapper)
	plan := wrap.GetRecordQueryPlan().(*plans.RecordQueryTableFunctionPlan)
	if plan.GetStreamValue() != nil {
		t.Fatal("expected nil stream value")
	}
	if plan.Explain() != "TableFunction(<nil>)" {
		t.Fatalf("Explain = %q", plan.Explain())
	}
}
