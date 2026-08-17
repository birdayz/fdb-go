package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlanResultValuesAreExactStableAndValidated(t *testing.T) {
	t.Parallel()

	rowType := exactTestRecordType()
	otherRowType := values.NewRecordType("other_row", false, []values.Field{
		{Name: "ONLY", FieldType: values.NullableString},
	})
	newScan := func(t testing.TB, name string, flowedType values.Type) *RecordQueryScanPlan {
		t.Helper()
		return mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{name}, flowedType, false)
		})
	}

	t.Run("leaf scan owns one stable exact result", func(t *testing.T) {
		t.Parallel()
		scan := newScan(t, "T", rowType)
		assertStableExactPlanResult(t, scan, rowType)
		if _, ok := values.AsQuantifiedObjectValue(scan.GetResultValue()); !ok {
			t.Fatalf("scan result = %T, want exact QuantifiedObjectValue", scan.GetResultValue())
		}
	})

	t.Run("multi-type scan accepts exact erased record", func(t *testing.T) {
		t.Parallel()
		anyRecord := values.NewAnyRecordType(false)
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"T", "U"}, anyRecord, false)
		})
		assertStableExactPlanResult(t, scan, anyRecord)
		if scan.GetResultType().Equals(values.NewRecordType("", false, nil)) {
			t.Fatal("multi-type scan's AnyRecord result collapsed into concrete RECORD<>")
		}
	})

	t.Run("unary rebuild adopts replacement exact type", func(t *testing.T) {
		t.Parallel()
		original := newScan(t, "T", rowType)
		limit := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
			return NewRecordQueryLimitPlan(original, 5, 0)
		})
		assertStableExactPlanResult(t, limit, rowType)

		replacement := newScan(t, "U", otherRowType)
		rebuiltExpression, err := limit.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{replacement}))
		if err != nil {
			t.Fatalf("WithQuantifiers(replacement): %v", err)
		}
		rebuilt, ok := rebuiltExpression.(*RecordQueryLimitPlan)
		if !ok {
			t.Fatalf("WithQuantifiers returned %T, want *RecordQueryLimitPlan", rebuiltExpression)
		}
		assertStableExactPlanResult(t, rebuilt, otherRowType)
		assertStableExactPlanResult(t, limit, rowType)
	})

	t.Run("set plan rejects disagreeing exact child types", func(t *testing.T) {
		t.Parallel()
		plan, err := NewRecordQueryComparatorPlan(
			[]RecordQueryPlan{
				newScan(t, "T", rowType),
				newScan(t, "U", otherRowType),
			},
			nil,
			0,
			false,
			false,
		)
		if err == nil {
			t.Fatal("NewRecordQueryComparatorPlan accepted disagreeing child types")
		}
		if plan != nil {
			t.Fatalf("NewRecordQueryComparatorPlan returned %#v with an error", plan)
		}
	})

	t.Run("explode derives exact element and ordinality types", func(t *testing.T) {
		t.Parallel()
		collectionType := values.NewArrayType(false, values.NullableString)
		collection := &values.ConstantValue{Value: []string{"a"}, Typ: collectionType}
		bare := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
			return NewRecordQueryExplodePlan(collection)
		})
		assertStableExactPlanResult(t, bare, values.NullableString)

		withOrdinality := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
			return NewRecordQueryExplodePlanWithOrdinality(collection, true)
		})
		assertStableExactPlanResult(t, withOrdinality, values.ExplodeOrdinalityResultType(values.NullableString))
	})

	t.Run("streaming count result is nullable long", func(t *testing.T) {
		t.Parallel()
		plan := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
			return NewRecordQueryStreamingAggregationPlan(
				newScan(t, "T", rowType),
				nil,
				[]expressions.AggregateSpec{{Function: expressions.AggCount, Alias: "count"}},
			)
		})
		assertStableExactPlanResult(t, plan, values.NewRecordType("", false, []values.Field{
			{Name: "COUNT", FieldType: values.NullableLong},
		}))
		resultType, ok := plan.GetResultType().(*values.RecordType)
		if !ok || len(resultType.Fields) != 1 || !resultType.Fields[0].FieldType.Equals(values.NullableLong) {
			t.Fatalf("COUNT output type = %v, want one NullableLong field", plan.GetResultType())
		}
	})

	t.Run("invalid result types return errors without objects", func(t *testing.T) {
		t.Parallel()
		if scan, err := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false); err == nil || scan != nil {
			t.Fatalf("scan(UnknownType) = (%#v, %v), want (nil, error)", scan, err)
		}
		if load, err := NewRecordQueryLoadByKeysPlanFromParameter("keys", values.UnknownType); err == nil || load != nil {
			t.Fatalf("load-by-keys(UnknownType) = (%#v, %v), want (nil, error)", load, err)
		}
		if explode, err := NewRecordQueryExplodePlan(
			&values.ConstantValue{Value: []any{}, Typ: values.NewArrayType(false, nil)},
		); err == nil || explode != nil {
			t.Fatalf("explode(erased array) = (%#v, %v), want (nil, error)", explode, err)
		}
	})
}

func assertStableExactPlanResult(t testing.TB, plan RecordQueryPlan, want values.Type) {
	t.Helper()
	first := plan.GetResultValue()
	second := plan.GetResultValue()
	if first == nil || second == nil {
		t.Fatalf("%T result Value is nil", plan)
	}
	if first != second {
		t.Fatalf("%T returned different result Values across reads", plan)
	}
	if _, err := values.SnapshotExactType(first.Type()); err != nil {
		t.Fatalf("%T result type is not exact: %v", plan, err)
	}
	if want == nil || !want.Equals(first.Type()) {
		t.Fatalf("%T result type = %v, want %v", plan, first.Type(), want)
	}
}
