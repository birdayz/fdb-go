package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushFilterThroughGroupBy_AllPredsOnGroupKeys(t *testing.T) {
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

	filterExpr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "region", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "US"),
			),
		}, gbQ)
	filterRef := expressions.InitialOf(filterExpr)

	results := FireExpressionRule(NewPushFilterThroughGroupByRule(), filterRef)
	if len(results) == 0 {
		t.Fatal("PushFilterThroughGroupByRule didn't fire")
	}

	// The yielded expression should be a GroupByExpression wrapping a filter.
	newGB, ok := results[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected *GroupByExpression, got %T", results[0])
	}
	innerRef := newGB.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("inner Reference is nil")
	}
	innerFilter, ok := innerRef.Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected inner to be *LogicalFilterExpression, got %T", innerRef.Get())
	}
	if len(innerFilter.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate pushed, got %d", len(innerFilter.GetPredicates()))
	}
}

func TestPushFilterThroughGroupBy_PredOnNonKey_DoesNotFire(t *testing.T) {
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

	// Filter on "total" which is NOT a grouping key.
	filterExpr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "total", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
			),
		}, gbQ)
	filterRef := expressions.InitialOf(filterExpr)

	results := FireExpressionRule(NewPushFilterThroughGroupByRule(), filterRef)
	if len(results) != 0 {
		t.Fatal("PushFilterThroughGroupByRule should NOT fire when predicate references non-key column")
	}
}

func TestPushFilterThroughGroupBy_PartialPushdown(t *testing.T) {
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

	// Mixed predicates: "region" is a key, "total" is not.
	filterExpr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "region", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "US"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "total", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
			),
		}, gbQ)
	filterRef := expressions.InitialOf(filterExpr)

	results := FireExpressionRule(NewPushFilterThroughGroupByRule(), filterRef)
	if len(results) == 0 {
		t.Fatal("PushFilterThroughGroupByRule should fire with partial pushdown")
	}

	// Result should be: Filter([total > 100], GroupBy(region, ..., Filter([region='US'], Scan)))
	outerFilter, ok := results[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected outer LogicalFilterExpression, got %T", results[0])
	}
	if len(outerFilter.GetPredicates()) != 1 {
		t.Fatalf("expected 1 residual predicate in outer filter, got %d", len(outerFilter.GetPredicates()))
	}
	// Verify the residual is the "total" predicate.
	residualPred := outerFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if residualPred.Operand.(*values.FieldValue).Field != "total" {
		t.Fatalf("residual predicate should be on 'total', got %q", residualPred.Operand.(*values.FieldValue).Field)
	}

	// Inner should be GroupBy.
	innerRef := outerFilter.GetInner().GetRangesOver()
	innerGB, ok := innerRef.Get().(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected GroupByExpression inside outer filter, got %T", innerRef.Get())
	}
	// GroupBy's inner should be a Filter with the pushed predicate.
	gbInnerRef := innerGB.GetInner().GetRangesOver()
	innerFilter, ok := gbInnerRef.Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected LogicalFilterExpression below GroupBy, got %T", gbInnerRef.Get())
	}
	if len(innerFilter.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(innerFilter.GetPredicates()))
	}
	pushedPred := innerFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if pushedPred.Operand.(*values.FieldValue).Field != "region" {
		t.Fatalf("pushed predicate should be on 'region', got %q", pushedPred.Operand.(*values.FieldValue).Field)
	}
}

func TestPushFilterThroughGroupBy_CaseInsensitive(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// GroupBy key is "Region" (uppercase R).
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "Region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	// Filter on "region" (lowercase r) — should still match.
	filterExpr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "region", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "EU"),
			),
		}, gbQ)
	filterRef := expressions.InitialOf(filterExpr)

	results := FireExpressionRule(NewPushFilterThroughGroupByRule(), filterRef)
	if len(results) == 0 {
		t.Fatal("PushFilterThroughGroupByRule should fire with case-insensitive key match")
	}
}

func TestPushFilterThroughGroupBy_ConstantPredPushes(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "x", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "y", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	// Constant TRUE predicate — can always be pushed.
	filterExpr := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		}, gbQ)
	filterRef := expressions.InitialOf(filterExpr)

	results := FireExpressionRule(NewPushFilterThroughGroupByRule(), filterRef)
	if len(results) == 0 {
		t.Fatal("PushFilterThroughGroupByRule should fire with constant TRUE predicate")
	}
}
