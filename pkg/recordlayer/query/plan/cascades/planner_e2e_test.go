package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestE2E_ScanOnlyPlan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	rootRef := expressions.InitialOf(scan)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if _, ok := plan.(*physicalScanWrapper); !ok {
		t.Fatalf("expected physicalScanWrapper, got %T", plan)
	}
}

func TestE2E_FilterOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, 42),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if _, ok := plan.(*physicalFilterWrapper); !ok {
		t.Fatalf("expected physicalFilterWrapper, got %T", plan)
	}
}

func TestE2E_SortOverFilterOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, 10),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{
			Value:   &values.FieldValue{Field: "x", Typ: values.UnknownType},
			Reverse: false,
		}},
		expressions.ForEachQuantifier(filterRef),
	)
	rootRef := expressions.InitialOf(sort)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_DistinctOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(distinct)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_LimitOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	limit := expressions.NewLogicalLimitExpression(5, 0,
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(limit)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_UnionOfTwoScans(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	rootRef := expressions.InitialOf(union)

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}
