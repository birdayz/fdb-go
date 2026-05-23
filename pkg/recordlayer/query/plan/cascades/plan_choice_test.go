package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestPlanChoice_IndexScanChosenOverFullScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Product"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "CATEGORY", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "electronics"),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_category",
			columns:     []string{"CATEGORY"},
			recordTypes: []string{"Product"},
			unique:      false,
		},
	})

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if bestExpr == nil {
		t.Fatal("Plan returned nil")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()

	switch physicalPlan.(type) {
	case *plans.RecordQueryIndexPlan:
		// Index scan chosen — optimizer correctly preferred index over full scan.
	default:
		t.Fatalf("optimizer should choose IndexScan for equality on indexed column, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
}

type planChoiceIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d *planChoiceIndexDef) IndexName() string                { return d.name }
func (d *planChoiceIndexDef) IndexColumnNames() []string       { return d.columns }
func (d *planChoiceIndexDef) IndexRecordTypes() []string       { return d.recordTypes }
func (d *planChoiceIndexDef) IndexIsUnique() bool              { return d.unique }
func (d *planChoiceIndexDef) IndexPrimaryKeyColumns() []string { return nil }
