package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

func TestExecutePlanTransactionalIndexState(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"value", "aggregate", "vector"} {
		for _, conflictCheck := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conflict=%t", route, conflictCheck), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				seed := setupStore(t,
					recordlayer.NewCountIndex("state_count", recordlayer.GroupAll(recordlayer.Field("price"))),
					recordlayer.NewVectorIndex("state_vector", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2))
				insertOrders(t, seed, &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(20)})
				orderType := PositionalTypeForRecordLayout((&gen.Order{}).ProtoReflect().Descriptor(), false)
				name := "order_price_idx"
				var plan plans.RecordQueryPlan
				switch route {
				case "value":
					plan = mustExecutorConstruct(plans.NewRecordQueryIndexPlan(name, nil, []string{"Order"}, orderType, false))
				case "aggregate":
					name = "state_count"
					ordinal, ok := orderType.FieldIndexUnique("price")
					if !ok {
						t.Fatal("price field absent")
					}
					rowType := exactTestRowType(values.Field{Name: "price", FieldType: orderType.Fields[ordinal].FieldType}, values.Field{Name: "COUNT(*)", FieldType: values.NotNullLong})
					indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(name, nil, []string{"Order"}, orderType, false))
					plan = mustExecutorConstruct(plans.NewRecordQueryAggregateIndexPlan(indexPlan, "Order", rowType, "COUNT")).WithGroupColumns([]string{"price"}, "")
				case "vector":
					name = "state_vector"
					plan = mustExecutorConstruct(plans.NewRecordQueryVectorIndexPlan(name, nil,
						&values.ConstantValue{Value: []float64{10, 20}, Typ: values.NewArrayType(false, values.NotNullDouble)},
						&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, predicates.ComparisonDistanceRankLessThanOrEq,
						nil, nil, []string{"Order"}, orderType))
				}
				open := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
					return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(seed.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				}
				if conflictCheck {
					_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						store, err := open(rtx)
						if err != nil {
							return nil, err
						}
						_, err = store.MarkIndexDisabled(name)
						return nil, err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				tx, err := testDB.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Cancel()
				rtx := testDB.NewRecordContext(tx)
				store, err := open(rtx)
				if err != nil {
					t.Fatal(err)
				}
				execute := func() ([]QueryResult, error) {
					cursor, err := ExecutePlan(ctx, plan, store, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
					if err != nil {
						return nil, err
					}
					defer cursor.Close()
					return CollectAll(ctx, cursor)
				}
				wantState := recordlayer.IndexStateDisabled
				if !conflictCheck {
					rows, err := execute()
					if err != nil || len(rows) != 1 {
						t.Fatalf("positive route control: rows=%v err=%v", rows, err)
					}
					other, err := open(rtx)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := other.ClearAndMarkIndexWriteOnly(name); err != nil {
						t.Fatal(err)
					}
					wantState = recordlayer.IndexStateWriteOnly
				}
				_, err = execute()
				var unreadable *recordlayer.IndexNotReadableError
				if !errors.As(err, &unreadable) || unreadable.IndexName != name || unreadable.CurrentState != wantState {
					t.Fatalf("state gate error=%v, want %s/%s", err, name, wantState)
				}
				if conflictCheck {
					_, err = testDB.Run(ctx, func(builder *recordlayer.FDBRecordContext) (any, error) {
						other, err := open(builder)
						if err != nil {
							return nil, err
						}
						return nil, other.RebuildIndex(other.GetMetaData().GetIndex(name))
					})
					if err != nil {
						t.Fatal(err)
					}
					tx.Set(testSubspace(t).Pack(tuple.Tuple{"executor-state-sentinel"}), []byte("validate refused-scan conflict"))
					var conflict fdb.Error
					if err := rtx.Commit(); !errors.As(err, &conflict) || conflict.Code != 1020 {
						t.Fatalf("commit=%v, want 1020", err)
					}
				} else {
					tx.Cancel()
					_, err := execute()
					var canceled fdb.Error
					if !errors.As(err, &canceled) || canceled.Code != 1025 {
						t.Fatalf("canceled scan=%v, want 1025", err)
					}
				}
			})
		}
	}
}
