package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestDistinctOverGroupByElim_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.NullableString}},
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

func TestDistinctOverGroupByElim_FloatingOrUnknownKeyDoesNotFire(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		typ  values.Type
	}{
		{name: "FLOAT", typ: values.NotNullFloat},
		{name: "DOUBLE", typ: values.NullableDouble},
		{name: "nested DOUBLE", typ: values.NewArrayType(true, values.NullableDouble)},
		{name: "unknown", typ: values.UnknownType},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
			groupBy := expressions.NewGroupByExpression(
				[]values.Value{&values.FieldValue{Field: "K", Typ: test.typ}},
				nil,
				expressions.ForEachQuantifier(expressions.InitialOf(scan)),
			)
			distinct := expressions.NewLogicalDistinctExpression(
				expressions.ForEachQuantifier(expressions.InitialOf(groupBy)),
			)
			if results := FireExpressionRule(
				NewDistinctOverGroupByElimRule(), expressions.InitialOf(distinct),
			); len(results) != 0 {
				t.Fatalf("rule eliminated DISTINCT over %s grouping identity", test.name)
			}
		})
	}
}

func TestDistinctOverScalarGroupByElim_Fires(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	groupBy := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(groupBy)),
	)
	if results := FireExpressionRule(
		NewDistinctOverGroupByElimRule(), expressions.InitialOf(distinct),
	); len(results) != 1 {
		t.Fatalf("scalar GROUP BY emits at most one row: got %d rewrites, want 1", len(results))
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
