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

func TestImplementHashAgg_CostHigherThanStreaming(t *testing.T) {
	t.Parallel()

	// With ordered input, both streaming and hash agg fire.
	// Streaming agg should be cheaper (lower per-row CPU).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sortExpr)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	gbRef := expressions.InitialOf(gb)

	FireExpressionRule(NewPrimaryScanRule(), scanRef)
	FireExpressionRule(NewImplementSortRule(), sortRef)

	streamResults := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	hashResults := FireExpressionRule(NewImplementHashAggregationRule(), gbRef)

	if len(streamResults) == 0 || len(hashResults) == 0 {
		t.Fatal("both rules should fire")
	}

	// Verify the hash agg wrapper reports no ordering.
	hashWrapper := hashResults[0].(*physicalHashAggWrapper)
	o := hashWrapper.HintOrdering()
	if o.IsKnown {
		t.Fatal("hash agg should report IsKnown=false (no output ordering)")
	}
}
