package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
