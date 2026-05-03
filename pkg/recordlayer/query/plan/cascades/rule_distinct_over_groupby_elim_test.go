package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestDistinctOverGroupByElim_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	distinct := expressions.NewLogicalDistinctExpression(gbQ)
	distinctRef := expressions.InitialOf(distinct)

	results := FireExpressionRule(NewDistinctOverGroupByElimRule(), distinctRef)
	if len(results) == 0 {
		t.Fatal("DistinctOverGroupByElimRule didn't fire")
	}

	// The result should be the GroupByExpression itself.
	if _, ok := results[0].(*expressions.GroupByExpression); !ok {
		t.Fatalf("expected *GroupByExpression, got %T", results[0])
	}
}

func TestDistinctOverGroupByElim_DoesNotFireOverFilter(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Distinct over a Filter (not a GroupBy) — should not fire.
	filter := expressions.NewLogicalFilterExpression(nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	distinct := expressions.NewLogicalDistinctExpression(filterQ)
	distinctRef := expressions.InitialOf(distinct)

	results := FireExpressionRule(NewDistinctOverGroupByElimRule(), distinctRef)
	if len(results) != 0 {
		t.Fatal("DistinctOverGroupByElimRule should not fire over a filter")
	}
}
