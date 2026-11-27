package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_DeletePlan tests deleting a record via the executor.
func TestIntegration_DeletePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(500),
		Price:   proto.Int32(42),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(scan, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("delete returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(500)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec != nil {
			t.Error("record still exists after delete")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DeletePlan_WithFilter tests DELETE with a filter predicate.
func TestIntegration_DeletePlan_WithFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15301), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(15302), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(15303), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(75)),
					},
				),
			},
			scan,
		))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(filter, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		deleted, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(deleted) != 2 {
			t.Fatalf("delete returned %d results, want 2 (price > 75)", len(deleted))
		}

		// Verify only the low-price order remains.
		scanAll := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor2, err := ExecutePlan(ctx, scanAll, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("verify scan: %v", err)
		}
		defer cursor2.Close()
		remaining, err := CollectAll(ctx, cursor2)
		if err != nil {
			t.Fatalf("verify CollectAll: %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("remaining = %d, want 1", len(remaining))
		}
		price, _ := rowMap(remaining[0])["price"].(int64)
		if price != 50 {
			t.Errorf("remaining order price = %d, want 50", price)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_Delete tests DELETE WHERE with a parameterized predicate.
func TestIntegration_ParameterBinding_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15401), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(15402), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(15403), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 0),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ParameterValue{Ordinal: 1},
					},
				),
			},
			scan,
		))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(filter, "Order"))

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(15402)})
		cursor, err := ExecutePlan(ctx, del, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		deleted, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(deleted) != 1 {
			t.Fatalf("delete returned %d results, want 1", len(deleted))
		}

		// Verify 2 orders remain.
		scanAll := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor2, err := ExecutePlan(ctx, scanAll, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("verify scan: %v", err)
		}
		defer cursor2.Close()
		remaining, err := CollectAll(ctx, cursor2)
		if err != nil {
			t.Fatalf("verify CollectAll: %v", err)
		}
		if len(remaining) != 2 {
			t.Fatalf("remaining = %d, want 2", len(remaining))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_InsertPlan_DuplicateError mirrors Java's
// testInsertExistingRecordThrowsException: inserting a record
// that already exists must return RecordAlreadyExistsError.
func TestIntegration_InsertPlan_DuplicateError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(17101), Price: proto.Int32(42),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		ins := mustExecutorConstruct(plans.NewRecordQueryInsertPlan(scan, "Order", integrationOrderType()))

		_, err = ExecutePlan(ctx, ins, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err == nil {
			t.Fatal("expected error on duplicate insert, got nil")
		}
		var alreadyExists *recordlayer.RecordAlreadyExistsError
		if !errors.As(err, &alreadyExists) {
			t.Fatalf("expected RecordAlreadyExistsError, got %T: %v", err, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_InsertPlan_ValuesExplode proves the INSERT VALUES
// Cascades shape: RecordQueryInsertPlan over a RecordQueryExplodePlan
// of an array of literal RecordConstructorValues. The explode streams
// computed-row datums (no stored Record), and executeInsert's
// Datum→message bridge materializes each as a target-type record. This
// is the executor end of the INSERT VALUES path (RFC-035, Gap C).
func TestIntegration_InsertPlan_ValuesExplode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	// VALUES (7001, 42), (7002, 43) — order_id is a BIGINT, price an INTEGER
	// (its value carried as int64), every other column a typed NULL: the row
	// has the table's type, as the insert plan admits only a row whose every
	// column promotes to the table's (RecordQueryInsertPlan.insertPlan).
	mkRow := func(id, price int64) *values.RecordConstructorValue {
		var fields []values.RecordConstructorField
		for _, f := range integrationOrderType().Fields {
			var v values.Value
			switch f.Name {
			case "order_id":
				v = &values.ConstantValue{Value: id, Typ: values.NotNullLong}
			case "price":
				v = &values.ConstantValue{Value: price, Typ: values.NotNullInt}
			default:
				v = values.NewNullValue(f.FieldType)
			}
			fields = append(fields, values.RecordConstructorField{Name: f.Name, Value: v})
		}
		return values.NewRecordConstructorValue(fields...)
	}
	firstRow := mkRow(7001, 42)
	arr := values.NewArrayConstructorValue(firstRow.Type(), []values.Value{firstRow, mkRow(7002, 43)})
	explode := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(arr))
	ins := mustExecutorConstruct(plans.NewRecordQueryInsertPlan(explode, "Order", integrationOrderType()))
	probe, err := ExecutePlan(ctx, explode, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute VALUES explode: %v", err)
	}
	probeRows, err := CollectAll(ctx, probe)
	_ = probe.Close()
	if err != nil {
		t.Fatalf("collect VALUES explode: %v", err)
	}
	if len(probeRows) != 2 {
		t.Fatalf("VALUES explode emitted %d rows, want 2", len(probeRows))
	}
	for i, row := range probeRows {
		if row.Positional == nil || !row.Positional.Type.Equals(explode.GetResultType()) {
			t.Fatalf("VALUES explode row %d type = %v, want declared %v", i, row.Positional, explode.GetResultType())
		}
	}

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		cursor, err := ExecutePlan(ctx, ins, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		inserted, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if len(inserted) != 2 {
			t.Fatalf("INSERT VALUES emitted %d rows, want 2", len(inserted))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute INSERT VALUES: %v", err)
	}

	// Verify both rows persisted with correct values via a scan.
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		rows, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		got := map[int64]int64{}
		for _, r := range rows {
			o, ok := r.Record.Record.(*demopb.Order)
			if !ok {
				t.Fatalf("scanned record type = %T, want *demopb.Order", r.Record.Record)
			}
			got[o.GetOrderId()] = int64(o.GetPrice())
		}
		if got[7001] != 42 || got[7002] != 43 {
			t.Fatalf("persisted rows = %v, want {7001:42, 7002:43}", got)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify scan: %v", err)
	}
}

// TestIntegration_DeletePlan_AllRecords tests deleting all records
// via an unfiltered scan→delete pipeline.
func TestIntegration_DeletePlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(17701), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(17702), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(17703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(scan, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("delete returned %d results, want 3", len(results))
		}

		for _, pk := range []int64{17701, 17702, 17703} {
			rec, err := s.LoadRecord(tuple.Tuple{pk})
			if err != nil {
				t.Fatalf("LoadRecord(%d): %v", pk, err)
			}
			if rec != nil {
				t.Errorf("record %d still exists after delete", pk)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
