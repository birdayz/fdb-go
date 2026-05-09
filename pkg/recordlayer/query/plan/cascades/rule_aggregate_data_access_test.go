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

func TestAggregateDataAccessRule_MultipleAggregates_OnlyOneCandidate(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Query has TWO aggregates but only one candidate — can't satisfy
	// via single-aggregate match, can't intersect with only one
	// candidate.
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
		t.Fatal("AggregateDataAccessRule should NOT fire for multi-aggregate query with only one candidate")
	}
}

func TestAggregateDataAccessRule_MultiAggregateIntersection(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Two aggregates: SUM(amount) and COUNT(id), grouped by region.
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Two candidates covering each aggregate, same grouping.
	sumCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	countCand := NewAggregateIndexMatchCandidate(
		"Orders$count_id_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggCount,
		"id",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 multi-intersection result, got %d", len(results))
	}
	if !IsPhysicalMultiIntersection(results[0]) {
		t.Fatalf("expected physicalMultiIntersectionWrapper, got %T", results[0])
	}
	plan := GetPhysicalMultiIntersectionPlan(results[0])
	if plan == nil {
		t.Fatal("GetPhysicalMultiIntersectionPlan returned nil")
	}
	if len(plan.GetChildren()) != 2 {
		t.Fatalf("expected 2 children, got %d", len(plan.GetChildren()))
	}
	if len(plan.GetComparisonKey()) != 1 {
		t.Fatalf("expected 1 comparison key (region), got %d", len(plan.GetComparisonKey()))
	}
	compKey := plan.GetComparisonKey()[0]
	fv, ok := compKey.(*values.FieldValue)
	if !ok {
		t.Fatalf("comparison key should be FieldValue, got %T", compKey)
	}
	if fv.Field != "region" {
		t.Fatalf("comparison key field should be 'region', got %q", fv.Field)
	}
	rv := plan.GetResultValue()
	if rv == nil {
		t.Fatal("result value should not be nil")
	}
}

func TestAggregateDataAccessRule_MultiAggregateMismatchedGrouping(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Two aggregates grouped by region.
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// SUM candidate groups by region, COUNT candidate groups by status
	// — grouping mismatch prevents intersection.
	sumCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region",
		[]string{"Orders"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)
	countCand := NewAggregateIndexMatchCandidate(
		"Orders$count_id_by_status",
		[]string{"Orders"},
		[]string{"status"}, // different grouping!
		expressions.AggCount,
		"id",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("should not produce intersection with mismatched grouping columns")
	}
}

func TestAggregateDataAccessRule_MultiAggregateThreeWay(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Three aggregates: SUM(amount), COUNT(id), MAX(price).
	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "region", Typ: values.UnknownType},
			&values.FieldValue{Field: "year", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
			{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "price", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	sumCand := NewAggregateIndexMatchCandidate(
		"Orders$sum_amount_by_region_year",
		[]string{"Orders"},
		[]string{"region", "year"},
		expressions.AggSum,
		"amount",
	)
	countCand := NewAggregateIndexMatchCandidate(
		"Orders$count_id_by_region_year",
		[]string{"Orders"},
		[]string{"region", "year"},
		expressions.AggCount,
		"id",
	)
	maxCand := NewAggregateIndexMatchCandidate(
		"Orders$max_price_by_region_year",
		[]string{"Orders"},
		[]string{"region", "year"},
		expressions.AggMax,
		"price",
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand, maxCand}}

	results := FireExpressionRuleWithMemo(
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 multi-intersection result, got %d", len(results))
	}
	if !IsPhysicalMultiIntersection(results[0]) {
		t.Fatalf("expected physicalMultiIntersectionWrapper, got %T", results[0])
	}
	plan := GetPhysicalMultiIntersectionPlan(results[0])
	if plan == nil {
		t.Fatal("GetPhysicalMultiIntersectionPlan returned nil")
	}
	if len(plan.GetChildren()) != 3 {
		t.Fatalf("expected 3 children for 3-way intersection, got %d", len(plan.GetChildren()))
	}
	if len(plan.GetComparisonKey()) != 2 {
		t.Fatalf("expected 2 comparison keys (region, year), got %d", len(plan.GetComparisonKey()))
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
