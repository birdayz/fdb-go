package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestDistinctOverGroupByElim_Fires(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("DistinctGroupByRow", false, []values.Field{
		{Name: "region", FieldType: values.NullableString},
		{Name: "id", FieldType: values.NotNullLong},
	})
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType))
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	scanRoot := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
	region := mustDistinctConstruct(values.ResolveFieldOrdinals(scanRoot, []int{0}))
	id := mustDistinctConstruct(values.ResolveFieldOrdinals(scanRoot, []int{1}))

	gb := mustDistinctConstruct(expressions.NewGroupByExpression(
		[]values.Value{region},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: id},
		},
		scanQ,
	))
	gbRef := expressions.InitialOf(gb)
	gbQ := expressions.ForEachQuantifier(gbRef)

	distinct := distinctRuleDistinct(t, gbQ)
	distinctRef := expressions.InitialOf(distinct)

	results := mustFireDistinctExpressionRule(t, NewDistinctOverGroupByElimRule(), distinctRef)
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
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rowType := values.NewRecordType("DistinctUnsafeGroupKey", false, []values.Field{{
				Name: "K", FieldType: test.typ,
			}})
			scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType))
			scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
			scanRoot := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
			key := mustDistinctConstruct(values.ResolveFieldOrdinals(scanRoot, []int{0}))
			groupBy := mustDistinctConstruct(expressions.NewGroupByExpression(
				[]values.Value{key},
				nil,
				scanQ,
			))
			distinct := distinctRuleDistinct(t,
				expressions.ForEachQuantifier(expressions.InitialOf(groupBy)),
			)
			if results := mustFireDistinctExpressionRule(t,
				NewDistinctOverGroupByElimRule(), expressions.InitialOf(distinct),
			); len(results) != 0 {
				t.Fatalf("rule eliminated DISTINCT over %s grouping identity", test.name)
			}
		})
	}

	unknownRow := values.NewRecordType("DistinctUnknownGroupKey", false, []values.Field{{
		Name: "K", FieldType: values.UnknownType,
	}})
	if _, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, unknownRow); err == nil {
		t.Fatal("UNKNOWN grouping identity was admitted before the rule boundary")
	}
}

func TestDistinctOverScalarGroupByElim_Fires(t *testing.T) {
	t.Parallel()
	scan := distinctRuleScan(t, "T")
	groupBy := mustDistinctConstruct(expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	))
	distinct := distinctRuleDistinct(t,
		expressions.ForEachQuantifier(expressions.InitialOf(groupBy)),
	)
	if results := mustFireDistinctExpressionRule(t,
		NewDistinctOverGroupByElimRule(), expressions.InitialOf(distinct),
	); len(results) != 1 {
		t.Fatalf("scalar GROUP BY emits at most one row: got %d rewrites, want 1", len(results))
	}
}

func TestDistinctOverGroupByElim_DoesNotFireOverFilter(t *testing.T) {
	t.Parallel()

	scan := distinctRuleScan(t, "T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Distinct over a Filter (not a GroupBy) — should not fire.
	filter := mustDistinctConstruct(expressions.NewLogicalFilterExpression(nil, scanQ))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	distinct := distinctRuleDistinct(t, filterQ)
	distinctRef := expressions.InitialOf(distinct)

	results := mustFireDistinctExpressionRule(t, NewDistinctOverGroupByElimRule(), distinctRef)
	if len(results) != 0 {
		t.Fatal("DistinctOverGroupByElimRule should not fire over a filter")
	}
}
