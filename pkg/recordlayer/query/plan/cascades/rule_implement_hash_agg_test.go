package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestImplementHashAgg_UnorderedInput(t *testing.T) {
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

	// Physicalize the scan.
	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementHashAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementHashAggregationRule didn't fire")
	}

	wrapper := results[0].(*physicalHashAggWrapper)
	if wrapper.GetPlan() == nil {
		t.Fatal("hash agg wrapper has nil plan")
	}
	explain := wrapper.GetPlan().Explain()
	if explain == "" {
		t.Fatal("empty explain")
	}
}

func TestImplementHashAgg_NoOutputOrdering(t *testing.T) {
	t.Parallel()

	// Hash agg over an unordered scan — verify the wrapper reports
	// no output ordering (IsKnown=false).
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

	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	hashResults := FireExpressionRule(NewImplementHashAggregationRule(), gbRef)
	if len(hashResults) == 0 {
		t.Fatal("ImplementHashAggregationRule didn't fire")
	}

	hashWrapper := hashResults[0].(*physicalHashAggWrapper)
	o := hashWrapper.HintOrdering()
	if o.IsKnown {
		t.Fatal("hash agg should report IsKnown=false (no output ordering)")
	}
}

func TestImplementHashAgg_MultipleAggregates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "salary", Typ: values.UnknownType}},
			{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "bonus", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementHashAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementHashAggregationRule didn't fire with 3 aggregates")
	}

	wrapper := results[0].(*physicalHashAggWrapper)
	explain := wrapper.GetPlan().Explain()
	if explain == "" {
		t.Fatal("empty explain")
	}
	t.Logf("Explain: %s", explain)
}

func TestImplementHashAgg_NoGroupingKeys(t *testing.T) {
	t.Parallel()

	// Global aggregate: COUNT(*) with no GROUP BY
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		nil, // no grouping keys — global aggregate
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementHashAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementHashAggregationRule should fire on global aggregate")
	}
}
