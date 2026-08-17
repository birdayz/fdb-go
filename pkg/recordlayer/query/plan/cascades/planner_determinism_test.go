package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustDeterminismConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner-determinism fixture: " + err.Error())
	}
	return value
}

func determinismRowType() values.Type {
	return values.NewRecordType("DeterminismRow", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func determinismRoot(q expressions.Quantifier) values.Value {
	flowedType := mustDeterminismConstruct(q.GetFlowedObjectType())
	return mustDeterminismConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func determinismField(root values.Value, ordinal int) values.Value {
	return mustDeterminismConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

// TestPlanDeterminism_ExtractedPlanStable verifies that Plan() produces
// the exact same physical plan explain string across 20 independent
// runs on the same logical tree with the same rules and index context.
//
// This catches the bug where non-deterministic Go map iteration in
// PlanPropertiesMap caused different plan selection when costs tied
// (e.g., tied StreamingAgg alternatives).
func TestPlanDeterminism_ExtractedPlanStable(t *testing.T) {
	t.Parallel()

	buildTree := func() (*expressions.Reference, PlanContext) {
		scan := mustDeterminismConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, determinismRowType()))
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		scanRoot := determinismRoot(scanQ)

		filter := mustDeterminismConstruct(expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					determinismField(scanRoot, 0),
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
			}, scanQ))
		filterRef := expressions.InitialOf(filter)
		filterQ := expressions.ForEachQuantifier(filterRef)
		filterRoot := determinismRoot(filterQ)

		proj := mustDeterminismConstruct(expressions.NewLogicalProjectionExpression(
			[]values.Value{
				determinismField(filterRoot, 0),
				determinismField(filterRoot, 1),
			}, filterQ))
		rootRef := expressions.InitialOf(proj)

		ctx := NewPlanContextFromIndexDefs([]IndexDef{
			&stubIndexDef{name: "idx_a", columns: []string{"A"}, recordTypes: []string{"T"}},
		})
		return rootRef, ctx
	}

	rules := DefaultExpressionRules()
	implRules := DefaultImplementationRules()

	var firstPlan string
	for i := 0; i < 20; i++ {
		ref, ctx := buildTree()
		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(implRules).
			WithMaxTasks(3_500)
		best, _, err := p.Plan(ref)
		if err != nil {
			t.Fatalf("run %d: Plan failed: %v", i, err)
		}
		if best == nil {
			t.Fatalf("run %d: Plan returned nil", i)
		}
		type physPlanExpr interface {
			GetRecordQueryPlan() interface{ Explain() string }
		}
		var plan string
		if ph, ok := best.(physPlanExpr); ok && ph.GetRecordQueryPlan() != nil {
			plan = ph.GetRecordQueryPlan().Explain()
		} else {
			plan = best.GetResultValue().Name()
		}
		if i == 0 {
			firstPlan = plan
			t.Logf("plan: %s", plan)
		} else if plan != firstPlan {
			t.Fatalf("run %d produced different plan:\n  first: %s\n  this:  %s", i, firstPlan, plan)
		}
	}
}

type stubIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d *stubIndexDef) IndexName() string                { return d.name }
func (d *stubIndexDef) IndexColumnNames() []string       { return d.columns }
func (d *stubIndexDef) IndexRecordTypes() []string       { return d.recordTypes }
func (d *stubIndexDef) IndexIsUnique() bool              { return d.unique }
func (d *stubIndexDef) IndexPrimaryKeyColumns() []string { return nil }
func (d *stubIndexDef) IndexCreatesDuplicates() bool     { return false }
func (d *stubIndexDef) IndexRowType() values.Type        { return determinismRowType() }
func (d *stubIndexDef) IndexKeyComponentTypes() []values.Type {
	result := make([]values.Type, len(d.columns))
	for i := range result {
		result[i] = values.NotNullLong
	}
	return result
}
