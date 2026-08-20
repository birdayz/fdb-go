package executor

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestExecutePlanRejectsFilteredIndexesAtEveryIndexLeaf is the executor half
// of filtered-index admission. Planning deliberately creates no sparse-index
// candidates until predicate implication exists, but physical plans are public
// Go values and can be hand-built or survive a metadata change. Every index
// leaf must therefore fail before it evaluates dynamic inputs, constructs a
// maintainer, or opens the incomplete index range.
func TestExecutePlanRejectsFilteredIndexesAtEveryIndexLeaf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ks := testSubspace(t)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	filtered := func(proto.Message) bool { return true }
	builder.AddIndex("Order", recordlayer.NewIndex(
		"filtered_value", recordlayer.Field("price")).SetPredicate(filtered))
	builder.AddIndex("Order", recordlayer.NewVectorIndex(
		"filtered_vector",
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")),
		2,
	).SetPredicate(filtered))
	builder.AddIndex("Order", recordlayer.NewCountIndex(
		"filtered_count", recordlayer.GroupAll(recordlayer.Field("price"))).SetPredicate(filtered))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	orderType := PositionalTypeForRecordLayout((&gen.Order{}).ProtoReflect().Descriptor(), false)
	priceOrdinal, ok := orderType.FieldIndexUnique("price")
	if !ok {
		t.Fatal("Order exact row type has no unique price field")
	}
	aggregateType := exactTestRowType(
		values.Field{Name: "price", FieldType: orderType.Fields[priceOrdinal].FieldType},
		values.Field{Name: "COUNT", FieldType: values.NotNullLong},
	)

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}

		// The value plan's unsupported physical type would fail in range
		// binding if the filtered-index invariant were checked too late.
		valuePlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"filtered_value",
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, predicates.ComparisonEquals,
					&values.ConstantValue{Value: []any{int64(1)}, Typ: values.NewArrayType(false, values.NotNullLong)}),
			},
			[]string{"Order"}, orderType, false,
		)).WithKeyComponentTypes([]values.Type{values.AnyType})

		// Both dynamic inputs are deliberately ill-typed. The sparse-index
		// rejection must precede vector partition validation and evaluation.
		vectorPlan := mustExecutorConstruct(plans.NewRecordQueryVectorIndexPlan(
			"filtered_vector", nil,
			&values.ConstantValue{Value: "not a vector", Typ: values.NotNullString},
			&values.ConstantValue{Value: "not a rank cap", Typ: values.NotNullString},
			predicates.ComparisonDistanceRankLessThanOrEq,
			nil, nil, []string{"Order"}, orderType,
		))

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"filtered_count", nil, []string{"Order"}, orderType, false,
		))
		aggregatePlan := mustExecutorConstruct(plans.NewRecordQueryAggregateIndexPlan(
			indexPlan, "Order", aggregateType, "COUNT",
		)).WithGroupColumns([]string{"price"}, "COUNT")

		for _, test := range []struct {
			name string
			plan plans.RecordQueryPlan
			want string
		}{
			{name: "value", plan: valuePlan, want: "filtered_value"},
			{name: "vector", plan: vectorPlan, want: "filtered_vector"},
			{name: "aggregate", plan: aggregatePlan, want: "filtered_count"},
		} {
			t.Run(test.name, func(t *testing.T) {
				cursor, execErr := ExecutePlan(
					ctx, test.plan, store, EmptyEvaluationContext(), nil,
					recordlayer.DefaultExecuteProperties(),
				)
				if cursor != nil {
					cursor.Close()
					t.Fatal("filtered index returned a cursor")
				}
				var filteredErr *FilteredIndexPlanError
				if !errors.As(execErr, &filteredErr) {
					t.Fatalf("error = %T(%v), want FilteredIndexPlanError", execErr, execErr)
				}
				if filteredErr.IndexName != test.want {
					t.Fatalf("filtered index name = %q, want %q", filteredErr.IndexName, test.want)
				}
			})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute filtered-plan guards: %v", err)
	}
}
