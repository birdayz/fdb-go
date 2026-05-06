package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestAggregateDataAccessRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) == 0 {
		t.Fatal("AggregateDataAccessRule didn't fire")
	}
	if !IsPhysicalIndexScan(results[0]) {
		t.Fatalf("expected physicalIndexScanWrapper, got %T", results[0])
	}
}

func TestAggregateDataAccessRule_WrongAggFunction(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Query asks for COUNT, index is SUM.
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("AggregateDataAccessRule should NOT fire for mismatched aggregate function")
	}
}

func TestAggregateDataAccessRule_WrongGroupingKeys(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Query groups by "status", index groups by "region".
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "status", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("AggregateDataAccessRule should NOT fire for mismatched grouping keys")
	}
}

func TestAggregateDataAccessRule_MultipleAggregates(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Query has TWO aggregates — single-agg index can't satisfy it.
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("AggregateDataAccessRule should NOT fire for multi-aggregate query")
	}
}

func TestAggregateDataAccessRule_WrongRecordType(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Aggregate index is on "Products", not "Orders".
	aggCand := NewAggregateIndexMatchCandidate(
		"Products$sum_amount_by_region",
		[]string{"Products"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("AggregateDataAccessRule should NOT fire for wrong record type")
	}
}
