package executor

import (
	"context"
	"reflect"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// orderPriceReader reads an order_price_idx entry, KEY (price, order_id),
// into an Order record.
func orderPriceReader(t *testing.T) *values.RecordConstructorValue {
	t.Helper()
	rowType := integrationOrderType()
	fields := make([]values.RecordConstructorField, len(rowType.Fields))
	for i, field := range rowType.Fields {
		var column values.Value = values.NewNullValue(field.FieldType)
		ordinal := -1
		switch field.Name {
		case "price":
			ordinal = 0
		case "order_id":
			ordinal = 1
		}
		if ordinal >= 0 {
			leaf, err := values.NewIndexEntryObjectValue(values.CurrentCorrelation(), values.TupleSourceKey, []int{ordinal}, field.FieldType)
			if err != nil {
				t.Fatalf("leaf %s: %v", field.Name, err)
			}
			column = leaf
		}
		fields[i] = values.RecordConstructorField{Name: field.Name, Value: column}
	}
	return values.NewRecordConstructorValue(fields...)
}

func orderPriceAtLeast(t *testing.T, price int64) *plans.RecordQueryIndexPlan {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(price),
	})
	if !res.Complete() {
		t.Fatal("merge failed")
	}
	return mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
		"order_price_idx", []*predicates.ComparisonRange{res.Range}, []string{"Order"}, integrationOrderType(), false,
	)).WithKeyComponentTypes([]values.Type{values.NullableInt}).WithIndexMetadata([]string{"price"}, []string{"order_id"}, false)
}

func slotsOf(results []QueryResult) [][]any {
	out := make([][]any, len(results))
	for i, r := range results {
		out[i] = append([]any(nil), r.Positional.Slots...)
	}
	return out
}

// TestIntegration_CoveringIndexValuePlan ports FDBCoveringIndexValuePlanTest's
// execution cases: the value plan reads the same records as the covering plan
// over one real index scan, resumes from its own continuations page by page,
// and refuses a covering plan's continuation, its salt being its own kind.
func TestIntegration_CoveringIndexValuePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(250), Quantity: proto.Int32(9)},
		&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(100)},
	)
	reader := orderPriceReader(t)
	index := orderPriceAtLeast(t, 100)
	covering := mustExecutorConstruct(plans.NewRecordQueryCoveringIndexPlan(index.WithEntryReader(reader)))
	valuePlan := mustExecutorConstruct(plans.NewRecordQueryCoveringIndexValuePlan(index, "Order", reader))

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		run := func(plan plans.RecordQueryPlan, continuation []byte, props recordlayer.ExecuteProperties) ([]QueryResult, []byte, error) {
			cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation, props)
			if err != nil {
				return nil, nil, err
			}
			defer cursor.Close()
			var rows []QueryResult
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, nil, err
				}
				if !result.HasNext() {
					next, _ := result.GetContinuation().ToBytes()
					return rows, next, nil
				}
				rows = append(rows, result.GetValue())
			}
		}

		byCovering, _, err := run(covering, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("covering plan: %v", err)
		}
		byValue, _, err := run(valuePlan, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("value plan: %v", err)
		}
		if len(byCovering) != 3 {
			t.Fatalf("covering plan read %d records above the bound, want 3", len(byCovering))
		}
		if !reflect.DeepEqual(slotsOf(byCovering), slotsOf(byValue)) {
			t.Fatalf("value plan rows %v, covering plan rows %v", slotsOf(byValue), slotsOf(byCovering))
		}

		props := recordlayer.DefaultExecuteProperties()
		props.ReturnedRowLimit = 1
		var paged []QueryResult
		var continuation []byte
		for page := 0; page < 10; page++ {
			rows, next, err := run(valuePlan, continuation, props)
			if err != nil {
				t.Fatalf("value plan page %d: %v", page, err)
			}
			paged = append(paged, rows...)
			if len(rows) == 0 || next == nil {
				break
			}
			continuation = next
		}
		if !reflect.DeepEqual(slotsOf(paged), slotsOf(byValue)) {
			t.Fatalf("paged value plan rows %v, want %v", slotsOf(paged), slotsOf(byValue))
		}

		_, coveringContinuation, err := run(covering, nil, props)
		if err != nil || coveringContinuation == nil {
			t.Fatalf("covering first page: continuation %x, err %v", coveringContinuation, err)
		}
		if _, _, err := run(valuePlan, coveringContinuation, props); err == nil {
			t.Fatal("the value plan resumed from a covering plan's continuation")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
