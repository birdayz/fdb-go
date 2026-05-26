package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

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
		scan := expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)

		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				&predicates.ComparisonPredicate{
					Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
					Comparison: predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ConstantValue{Value: int64(1)},
					},
				},
			}, scanQ)
		filterRef := expressions.InitialOf(filter)
		filterQ := expressions.ForEachQuantifier(filterRef)

		proj := expressions.NewLogicalProjectionExpression(
			[]values.Value{
				&values.FieldValue{Field: "A", Typ: values.UnknownType},
				&values.FieldValue{Field: "B", Typ: values.UnknownType},
			}, filterQ)
		rootRef := expressions.InitialOf(proj)

		ctx := NewPlanContextFromIndexDefs([]IndexDef{
			&stubIndexDef{name: "idx_a", columns: []string{"A"}, recordTypes: []string{"T"}},
		})
		return rootRef, ctx
	}

	rules := DefaultExpressionRules()
	rules = append(rules, MatchingRules()...)
	implRules := DefaultImplementationRules()

	var firstPlan string
	for i := 0; i < 20; i++ {
		ref, ctx := buildTree()
		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(implRules).
			WithMaxTasks(100_000)
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
