package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

const (
	aggregateDataRegion = iota
	aggregateDataStatus
	aggregateDataAmount
	aggregateDataID
	aggregateDataYear
	aggregateDataPrice
)

func mustAggregateDataConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct aggregate data-access fixture: " + err.Error())
	}
	return value
}

func aggregateDataRowType(recordName string) *values.RecordType {
	return values.NewRecordType(recordName, false, []values.Field{
		{Name: "region", FieldType: values.NullableString},
		{Name: "status", FieldType: values.NullableString},
		{Name: "amount", FieldType: values.NullableLong},
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "year", FieldType: values.NullableString},
		{Name: "price", FieldType: values.NullableLong},
	})
}

type aggregateDataSpec struct {
	function expressions.AggregateFunction
	ordinal  int
}

func aggregateDataGroupBy(
	groupingOrdinals []int,
	specs ...aggregateDataSpec,
) *expressions.GroupByExpression {
	scan := mustAggregateDataConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Orders"}, aggregateDataRowType("Orders")))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	row := mustAggregateDataConstruct(scanQ.RequireFlowedObjectValue())
	grouping := make([]values.Value, len(groupingOrdinals))
	for i, ordinal := range groupingOrdinals {
		grouping[i] = mustAggregateDataConstruct(values.ResolveFieldOrdinals(
			row, []int{ordinal}))
	}
	aggregates := make([]expressions.AggregateSpec, len(specs))
	for i, spec := range specs {
		aggregates[i] = expressions.AggregateSpec{
			Function: spec.function,
			Operand: mustAggregateDataConstruct(values.ResolveFieldOrdinals(
				row, []int{spec.ordinal})),
		}
	}
	return mustAggregateDataConstruct(expressions.NewGroupByExpression(
		grouping, aggregates, scanQ))
}

func aggregateDataCandidate(
	indexName string,
	recordType string,
	groupColumns []string,
	function expressions.AggregateFunction,
	aggregateColumn string,
	groupTypes []values.Type,
) *AggregateIndexMatchCandidate {
	return NewAggregateIndexMatchCandidate(
		indexName,
		[]string{recordType},
		groupColumns,
		function,
		aggregateColumn,
		aggregateDataRowType(recordType),
		groupTypes,
		len(groupColumns),
	)
}

func aggregateDataProjectedInner(
	t *testing.T,
	expression expressions.RelationalExpression,
	expectedType values.Type,
) plans.RecordQueryPlan {
	t.Helper()
	projection, ok := expression.(*plans.RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("expected exact aggregate-output projection, got %T", expression)
	}
	if got := projection.GetResultType(); !got.Equals(expectedType) {
		t.Fatalf("projected aggregate type = %s, want %s", got, expectedType)
	}
	inner := projection.GetInner()
	if inner == nil {
		t.Fatal("aggregate-output projection has no inner plan")
	}
	return inner
}

func TestAggregateDataAccessRule_Fires(t *testing.T) {
	t.Parallel()

	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := mustFireExpressionRuleWithMemo(t,
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) == 0 {
		t.Fatal("AggregateDataAccessRule didn't fire")
	}
	inner := aggregateDataProjectedInner(t, results[0], gb.GetResultValue().Type())
	if !IsPhysicalAggregateIndex(inner) {
		t.Fatalf("expected aggregate-index inner plan, got %T", inner)
	}
}

func TestAggregateDataAccessRule_WrongAggFunction(t *testing.T) {
	t.Parallel()

	// Query asks for COUNT, index is SUM.
	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggCount, ordinal: aggregateDataAmount},
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := mustFireExpressionRuleWithMemo(t,
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

	// Query groups by "status", index groups by "region".
	gb := aggregateDataGroupBy(
		[]int{aggregateDataStatus},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := mustFireExpressionRuleWithMemo(t,
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

	// Query has TWO aggregates but only one candidate — can't satisfy
	// via single-aggregate match, can't intersect with only one
	// candidate.
	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
		aggregateDataSpec{function: expressions.AggCount, ordinal: aggregateDataID},
	)
	gbRef := expressions.InitialOf(gb)

	aggCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := mustFireExpressionRuleWithMemo(t,
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

	// Two aggregates: SUM(amount) and COUNT(id), grouped by region.
	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
		aggregateDataSpec{function: expressions.AggCount, ordinal: aggregateDataID},
	)
	gbRef := expressions.InitialOf(gb)

	// Two candidates covering each aggregate, same grouping.
	sumCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	countCand := aggregateDataCandidate(
		"Orders$count_id_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggCount,
		"id",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand}}

	results := mustFireExpressionRuleWithMemo(t,
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 multi-intersection result, got %d", len(results))
	}
	inner := aggregateDataProjectedInner(t, results[0], gb.GetResultValue().Type())
	if !IsPhysicalMultiIntersection(inner) {
		t.Fatalf("expected multi-intersection inner plan, got %T", inner)
	}
	plan := GetPhysicalMultiIntersectionPlan(inner)
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
	fv, ok := values.AsFieldValue(compKey)
	if !ok {
		t.Fatalf("comparison key should be FieldValue, got %T", compKey)
	}
	if fv.DisplayName() != "region" {
		t.Fatalf("comparison key field should be 'region', got %q", fv.DisplayName())
	}
	rv := plan.GetResultValue()
	if rv == nil {
		t.Fatal("result value should not be nil")
	}
}

func TestAggregateDataAccessRule_MultiAggregateMismatchedGrouping(t *testing.T) {
	t.Parallel()

	// Two aggregates grouped by region.
	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
		aggregateDataSpec{function: expressions.AggCount, ordinal: aggregateDataID},
	)
	gbRef := expressions.InitialOf(gb)

	// SUM candidate groups by region, COUNT candidate groups by status
	// — grouping mismatch prevents intersection.
	sumCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region",
		"Orders",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	countCand := aggregateDataCandidate(
		"Orders$count_id_by_status",
		"Orders",
		[]string{"status"}, // different grouping!
		expressions.AggCount,
		"id",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand}}

	results := mustFireExpressionRuleWithMemo(t,
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

	// Three aggregates: SUM(amount), COUNT(id), MAX(price).
	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion, aggregateDataYear},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
		aggregateDataSpec{function: expressions.AggCount, ordinal: aggregateDataID},
		aggregateDataSpec{function: expressions.AggMax, ordinal: aggregateDataPrice},
	)
	gbRef := expressions.InitialOf(gb)

	sumCand := aggregateDataCandidate(
		"Orders$sum_amount_by_region_year",
		"Orders",
		[]string{"region", "year"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString, values.NullableString},
	)
	countCand := aggregateDataCandidate(
		"Orders$count_id_by_region_year",
		"Orders",
		[]string{"region", "year"},
		expressions.AggCount,
		"id",
		[]values.Type{values.NullableString, values.NullableString},
	)
	maxCand := aggregateDataCandidate(
		"Orders$max_price_by_region_year",
		"Orders",
		[]string{"region", "year"},
		expressions.AggMax,
		"price",
		[]values.Type{values.NullableString, values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{sumCand, countCand, maxCand}}

	results := mustFireExpressionRuleWithMemo(t,
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("expected 1 multi-intersection result, got %d", len(results))
	}
	inner := aggregateDataProjectedInner(t, results[0], gb.GetResultValue().Type())
	if !IsPhysicalMultiIntersection(inner) {
		t.Fatalf("expected multi-intersection inner plan, got %T", inner)
	}
	plan := GetPhysicalMultiIntersectionPlan(inner)
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

	gb := aggregateDataGroupBy(
		[]int{aggregateDataRegion},
		aggregateDataSpec{function: expressions.AggSum, ordinal: aggregateDataAmount},
	)
	gbRef := expressions.InitialOf(gb)

	// Aggregate index is on "Products", not "Orders".
	aggCand := aggregateDataCandidate(
		"Products$sum_amount_by_region",
		"Products",
		[]string{"region"},
		expressions.AggSum,
		"amount",
		[]values.Type{values.NullableString},
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{aggCand}}

	results := mustFireExpressionRuleWithMemo(t,
		NewAggregateDataAccessRule(),
		gbRef,
		ctx,
		nil,
	)
	if len(results) != 0 {
		t.Fatal("AggregateDataAccessRule should NOT fire for wrong record type")
	}
}
