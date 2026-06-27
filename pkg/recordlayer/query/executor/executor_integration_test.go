package executor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var testDB *recordlayer.FDBDatabase

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		panic("failed to start FDB container: " + err.Error())
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("failed to get cluster file: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "fdb_executor_test_*.txt")
	if err != nil {
		panic(err.Error())
	}
	_, _ = tmpFile.WriteString(clusterFile)
	_ = tmpFile.Close()

	fdb.MustAPIVersion(720)
	db, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("failed to open FDB: " + err.Error())
	}
	testDB = recordlayer.NewFDBDatabase(db)

	code := m.Run()

	_ = container.Terminate(context.Background())
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

func testSubspace(t *testing.T) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
}

func setupStore(t *testing.T) *recordlayer.FDBRecordStore {
	t.Helper()
	ctx := context.Background()
	ks := testSubspace(t)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("order_price_idx", recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}

	var store *recordlayer.FDBRecordStore
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		var err error
		store, err = recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return store
}

func insertOrders(t *testing.T, store *recordlayer.FDBRecordStore, orders ...*gen.Order) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		for _, o := range orders {
			if _, err := s.SaveRecord(o); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert orders: %v", err)
	}
}

// TestIntegration_ScanPlan_AllRecords tests scanning all records from a real FDB store.
func TestIntegration_ScanPlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	}, &gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	}, &gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan(nil, nil, false)
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) < 3 {
			t.Fatalf("scan returned %d results, want >= 3", len(results))
		}

		for _, r := range results {
			if r.Record == nil {
				t.Fatal("result has nil Record")
			}
			if r.PrimaryKey == nil {
				t.Fatal("result has nil PrimaryKey")
			}
			datum, ok := r.Datum.(map[string]any)
			if !ok {
				t.Fatalf("datum type = %T, want map[string]any", r.Datum)
			}
			if datum["ORDER_ID"] == nil {
				t.Error("ORDER_ID is nil in datum")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ScanPlan_TypeFilter tests scanning with a type filter.
func TestIntegration_ScanPlan_TypeFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(10),
		Price:   proto.Int32(999),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan(nil, nil, false)
		typeFilter := plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan)

		cursor, err := ExecutePlan(ctx, typeFilter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("type filter returned 0 results, want >= 1")
		}
		for _, r := range results {
			if r.Record.RecordType.Name != "Order" {
				t.Errorf("record type = %q, want Order", r.Record.RecordType.Name)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan tests filtering records by a predicate.
func TestIntegration_FilterPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(100),
		Price:   proto.Int32(50),
	}, &gen.Order{
		OrderId: proto.Int64(101),
		Price:   proto.Int32(500),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("filter returned %d results, want 1 (price > 100)", len(results))
		}
		price := results[0].Datum.(map[string]any)["PRICE"]
		if price != int64(500) {
			t.Errorf("price = %v, want 500", price)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SortLimitPlan tests sort + limit against real records.
func TestIntegration_SortLimitPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(201), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(202), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(203), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "PRICE"}, Reverse: false}},
			scan,
		)
		limited := plans.NewRecordQueryLimitPlan(sorted, 2, 0)

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("got %d results, want 2", len(results))
		}

		p1 := results[0].Datum.(map[string]any)["PRICE"].(int64)
		p2 := results[1].Datum.(map[string]any)["PRICE"].(int64)
		if p1 > p2 {
			t.Errorf("results not sorted ASC: price[0]=%d > price[1]=%d", p1, p2)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DeletePlan tests deleting a record via the executor.
func TestIntegration_DeletePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
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

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		del := plans.NewRecordQueryDeletePlan(scan, "Order")

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

// TestIntegration_IndexScan tests index scan via ComparisonRange.
func TestIntegration_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(302), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(303), Price: proto.Int32(250)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.LiteralValue(int64(100)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			nil,
			false,
		)

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("index scan returned %d results, want 2 (price >= 100)", len(results))
		}
		for _, r := range results {
			price := r.Datum.(map[string]any)["PRICE"].(int64)
			if price < 100 {
				t.Errorf("index scan returned price=%d, should be >= 100", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan tests updating records via the executor.
func TestIntegration_UpdatePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(601), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(602), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "ORDER_ID"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(601)),
					},
				),
			},
			scan,
		)
		update := plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "PRICE", NewValue: values.LiteralValue(int64(999))},
		})

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(601)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 999 {
			t.Errorf("price after update = %d, want 999", updated.GetPrice())
		}

		untouched, err := s.LoadRecord(tuple.Tuple{int64(602)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if untouched == nil {
			t.Fatal("untouched record not found")
		}
		other := untouched.Record.(*gen.Order)
		if other.GetPrice() != 200 {
			t.Errorf("untouched order price = %d, want 200", other.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ScanDatum_Shape verifies that scan datum maps contain
// the correct keys and values from proto deserialization.
func TestIntegration_ScanDatum_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(701), Price: proto.Int32(42)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)

		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("scan returned %d results, want 1", len(results))
		}

		datum := results[0].Datum.(map[string]any)
		if datum["ORDER_ID"] != int64(701) {
			t.Errorf("ORDER_ID = %v, want 701", datum["ORDER_ID"])
		}
		if datum["PRICE"] != int64(42) {
			t.Errorf("PRICE = %v, want 42", datum["PRICE"])
		}
		if _, exists := datum["FLOWER"]; exists {
			t.Errorf("FLOWER should not be in datum for unset field")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_Equality tests index scan with equality match.
func TestIntegration_IndexScan_Equality(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(901), Price: proto.Int32(77)},
		&gen.Order{OrderId: proto.Int64(902), Price: proto.Int32(88)},
		&gen.Order{OrderId: proto.Int64(903), Price: proto.Int32(77)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue(int64(77)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			nil,
			false,
		)

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("index equality scan returned %d results, want 2 (price == 77)", len(results))
		}
		for _, r := range results {
			price := r.Datum.(map[string]any)["PRICE"].(int64)
			if price != 77 {
				t.Errorf("index scan returned price=%d, want 77", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_BoundedRange tests index scan with both lower and upper bounds.
func TestIntegration_IndexScan_BoundedRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(1002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(1003), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(1004), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cr := predicates.EmptyComparisonRange()
		lowRes := cr.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.LiteralValue(int64(50)),
		})
		if !lowRes.Ok {
			t.Fatal("merge low failed")
		}
		highRes := lowRes.Range.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonLessThan,
			Operand: values.LiteralValue(int64(150)),
		})
		if !highRes.Ok {
			t.Fatal("merge high failed")
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{highRes.Range},
			[]string{"Order"},
			nil,
			false,
		)

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("bounded range scan returned %d results, want 2 (50 <= price < 150)", len(results))
		}
		for _, r := range results {
			price := r.Datum.(map[string]any)["PRICE"].(int64)
			if price < 50 || price >= 150 {
				t.Errorf("price=%d outside [50, 150)", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterSortLimit_Pipeline tests a realistic pipeline:
// scan → filter → sort → limit.
func TestIntegration_FilterSortLimit_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1101), Price: proto.Int32(500)},
		&gen.Order{OrderId: proto.Int64(1102), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(1103), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(1104), Price: proto.Int32(400)},
		&gen.Order{OrderId: proto.Int64(1105), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(150)),
					},
				),
			},
			scan,
		)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "PRICE"}, Reverse: true}},
			filter,
		)
		limited := plans.NewRecordQueryLimitPlan(sorted, 2, 0)

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("pipeline returned %d results, want 2 (top-2 by price DESC where price > 150)", len(results))
		}

		p1 := results[0].Datum.(map[string]any)["PRICE"].(int64)
		p2 := results[1].Datum.(map[string]any)["PRICE"].(int64)
		if p1 != 500 || p2 != 400 {
			t.Errorf("prices = [%d, %d], want [500, 400]", p1, p2)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- ResultSet integration tests ----------

// TestIntegration_ResultSet_TypedAccess executes a scan plan against real FDB,
// wraps the cursor in RecordLayerResultSet, and verifies typed column access.
func TestIntegration_ResultSet_TypedAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(2001), Price: proto.Int32(77)},
		&gen.Order{OrderId: proto.Int64(2002), Price: proto.Int32(88)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "ORDER_ID", TypeName: "BIGINT", Nullable: api.ColumnNoNulls},
			{Name: "PRICE", TypeName: "BIGINT", Nullable: api.ColumnNullable},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		md := rs.MetaData()
		if md.ColumnCount() != 2 {
			t.Fatalf("ColumnCount = %d, want 2", md.ColumnCount())
		}
		name, _ := md.ColumnName(1)
		if name != "ORDER_ID" {
			t.Errorf("ColumnName(1) = %q, want ORDER_ID", name)
		}

		var ids []int64
		for rs.Next() {
			id, err := rs.Long(1)
			if err != nil {
				t.Fatalf("Long(1): %v", err)
			}
			if rs.WasNull() {
				t.Error("ORDER_ID should not be null")
			}

			price, err := rs.Long(2)
			if err != nil {
				t.Fatalf("Long(2): %v", err)
			}
			if rs.WasNull() {
				t.Error("PRICE should not be null for inserted orders")
			}
			_ = price
			ids = append(ids, id)
		}
		if rs.Err() != nil {
			t.Fatalf("Err: %v", rs.Err())
		}
		if len(ids) < 2 {
			t.Fatalf("got %d rows, want >= 2", len(ids))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_StringCoercion verifies String() works on
// int64 values from real FDB records.
func TestIntegration_ResultSet_StringCoercion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(3001), Price: proto.Int32(42)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "PRICE", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		if !rs.Next() {
			t.Fatal("expected a row")
		}

		s2, err := rs.String(1)
		if err != nil {
			t.Fatalf("String(1): %v", err)
		}
		if s2 != "42" {
			t.Errorf("String(1) = %q, want '42'", s2)
		}

		d, err := rs.Double(1)
		if err != nil {
			t.Fatalf("Double(1): %v", err)
		}
		if d != 42.0 {
			t.Errorf("Double(1) = %v, want 42.0", d)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_FilterPipeline tests the full executor→ResultSet
// pipeline with a filter + sort + limit plan.
func TestIntegration_ResultSet_FilterPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(4001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(4002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(4003), Price: proto.Int32(90)},
		&gen.Order{OrderId: proto.Int64(4004), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThanEq,
						Operand: values.LiteralValue(int64(30)),
					},
				),
			},
			scan,
		)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "PRICE"}, Reverse: false}},
			filter,
		)

		cursor, err := ExecutePlan(ctx, sorted, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "PRICE", TypeName: "BIGINT"},
			{Name: "ORDER_ID", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		var prices []int64
		for rs.Next() {
			p, err := rs.Long(1)
			if err != nil {
				t.Fatalf("Long(1): %v", err)
			}
			prices = append(prices, p)
		}
		if rs.Err() != nil {
			t.Fatalf("Err: %v", rs.Err())
		}
		if len(prices) != 3 {
			t.Fatalf("got %d rows, want 3 (prices >= 30)", len(prices))
		}
		for i := 1; i < len(prices); i++ {
			if prices[i] < prices[i-1] {
				t.Errorf("prices not ascending: %v", prices)
				break
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_ByName verifies column-by-name access with
// real FDB data.
func TestIntegration_ResultSet_ByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(5001), Price: proto.Int32(99)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "ORDER_ID", TypeName: "BIGINT"},
			{Name: "PRICE", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		if !rs.Next() {
			t.Fatal("expected a row")
		}

		id, err := rs.LongByName("ORDER_ID")
		if err != nil {
			t.Fatalf("LongByName ORDER_ID: %v", err)
		}
		if id != 5001 {
			t.Errorf("ORDER_ID = %d, want 5001", id)
		}

		price, err := rs.LongByName("PRICE")
		if err != nil {
			t.Fatalf("LongByName PRICE: %v", err)
		}
		if price != 99 {
			t.Errorf("PRICE = %d, want 99", price)
		}

		_, err = rs.LongByName("NONEXISTENT")
		if err == nil {
			t.Fatal("expected error for nonexistent column")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insertCustomers(t *testing.T, store *recordlayer.FDBRecordStore, customers ...*gen.Customer) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		for _, c := range customers {
			if _, err := s.SaveRecord(c); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert customers: %v", err)
	}
}

// TestIntegration_ProjectionPlan tests projecting specific columns from scan results.
func TestIntegration_ProjectionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(6001), Price: proto.Int32(111)},
		&gen.Order{OrderId: proto.Int64(6002), Price: proto.Int32(222)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				&values.FieldValue{Field: "PRICE"},
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("projection returned %d results, want 2", len(results))
		}

		for _, r := range results {
			datum := r.Datum.(map[string]any)
			if _, exists := datum["PRICE"]; !exists {
				t.Error("projected datum should contain PRICE")
			}
			if _, exists := datum["ORDER_ID"]; exists {
				t.Error("projected datum should NOT contain ORDER_ID")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ProjectionPlan_MultiColumn tests multi-column projection.
func TestIntegration_ProjectionPlan_MultiColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(6101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				&values.FieldValue{Field: "ORDER_ID"},
				&values.FieldValue{Field: "PRICE"},
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}

		datum := results[0].Datum.(map[string]any)
		if datum["ORDER_ID"] != int64(6101) {
			t.Errorf("ORDER_ID = %v, want 6101", datum["ORDER_ID"])
		}
		if datum["PRICE"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", datum["PRICE"])
		}
		if len(datum) != 2 {
			t.Errorf("datum has %d keys, want exactly 2 (ORDER_ID, PRICE)", len(datum))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DistinctPlan tests deduplication by primary key.
func TestIntegration_DistinctPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(7001), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(7002), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(7003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		distinct := plans.NewRecordQueryDistinctPlan(scan)

		cursor, err := ExecutePlan(ctx, distinct, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("distinct returned %d results, want 3 (all unique PKs)", len(results))
		}

		seen := make(map[string]struct{})
		for _, r := range results {
			key := string(r.PrimaryKey.Pack())
			if _, dup := seen[key]; dup {
				t.Errorf("duplicate PK in distinct results: %v", r.PrimaryKey)
			}
			seen[key] = struct{}{}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_Filter tests filter with prepared-statement
// parameter against real FDB records.
func TestIntegration_ParameterBinding_Filter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(8002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(8003), Price: proto.Int32(90)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.NewParameterValue(1),
					},
				),
			},
			scan,
		)

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(40)})
		cursor, err := ExecutePlan(ctx, filter, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("parameter filter returned %d results, want 2 (price > 40)", len(results))
		}
		for _, r := range results {
			price := r.Datum.(map[string]any)["PRICE"].(int64)
			if price <= 40 {
				t.Errorf("price=%d should be > 40", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_IndexScan tests index scan with a parameter
// in the comparison range.
func TestIntegration_ParameterBinding_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8101), Price: proto.Int32(25)},
		&gen.Order{OrderId: proto.Int64(8102), Price: proto.Int32(75)},
		&gen.Order{OrderId: proto.Int64(8103), Price: proto.Int32(125)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.NewParameterValue(1),
		})
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			nil,
			false,
		)

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(50)})
		cursor, err := ExecutePlan(ctx, indexPlan, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("param index scan returned %d results, want 2 (price >= 50)", len(results))
		}
		for _, r := range results {
			price := r.Datum.(map[string]any)["PRICE"].(int64)
			if price < 50 {
				t.Errorf("price=%d, should be >= 50", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_CrossJoin tests a cross join between
// Orders and Customers (no join predicate).
func TestIntegration_NestedLoopJoin_CrossJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(9002), Price: proto.Int32(20)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")},
		&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob")},
		&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("Carol")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		innerScan := plans.NewRecordQueryScanPlan([]string{"Customer"}, nil, false)
		nlj := plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			nil,
			plans.JoinInner,
			"ORDER", "CUSTOMER",
			nil,
		)

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 6 {
			t.Fatalf("cross join returned %d results, want 6 (2 orders × 3 customers)", len(results))
		}

		for _, r := range results {
			datum := r.Datum.(map[string]any)
			if datum["ORDER_ID"] == nil {
				t.Error("ORDER_ID missing from joined row")
			}
			if datum["CUSTOMER_ID"] == nil {
				t.Error("CUSTOMER_ID missing from joined row")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_WithPredicate tests NLJ with a filter predicate
// on the outer table's PRICE column.
func TestIntegration_NestedLoopJoin_WithPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9101), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(9102), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(19101), Name: proto.String("Dan")},
		&gen.Customer{CustomerId: proto.Int64(19102), Name: proto.String("Eve")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		innerScan := plans.NewRecordQueryScanPlan([]string{"Customer"}, nil, false)

		nlj := plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "QUANTITY"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinInner,
			"ORDER", "CUSTOMER",
			nil,
		)

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("predicate join returned %d results, want 2 (quantity=5 order × 2 customers)", len(results))
		}
		for _, r := range results {
			datum := r.Datum.(map[string]any)
			if datum["ORDER_ID"] != int64(9101) {
				t.Errorf("ORDER_ID = %v, want 9101 (quantity=5)", datum["ORDER_ID"])
			}
			if datum["CUSTOMER_ID"] == nil {
				t.Error("CUSTOMER_ID missing from joined row")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_LeftOuter tests left outer join — unmatched
// outer rows are preserved.
func TestIntegration_NestedLoopJoin_LeftOuter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(9202), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(19201), Name: proto.String("Frank")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		innerScan := plans.NewRecordQueryScanPlan([]string{"Customer"}, nil, false)

		nlj := plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "QUANTITY"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinLeftOuter,
			"ORDER", "CUSTOMER",
			nil,
		)

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("left outer join returned %d results, want 2 (1 matched + 1 unmatched)", len(results))
		}

		matchedFound := false
		unmatchedFound := false
		for _, r := range results {
			datum := r.Datum.(map[string]any)
			orderID := datum["ORDER_ID"].(int64)
			if orderID == 9201 {
				if datum["CUSTOMER_ID"] == nil {
					t.Error("matched row should have CUSTOMER_ID from inner")
				}
				matchedFound = true
			} else if orderID == 9202 {
				unmatchedFound = true
			}
		}
		if !matchedFound {
			t.Error("expected matched row (order 9201 quantity=5)")
		}
		if !unmatchedFound {
			t.Error("expected unmatched outer row (order 9202 quantity=10)")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_WithParameter tests UPDATE with a parameterized
// SET value against real FDB.
func TestIntegration_UpdatePlan_WithParameter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8201), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "ORDER_ID"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(8201)),
					},
				),
			},
			scan,
		)
		update := plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "PRICE", NewValue: values.NewParameterValue(1)},
		})

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(777)})
		cursor, err := ExecutePlan(ctx, update, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(8201)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 777 {
			t.Errorf("price after param update = %d, want 777", updated.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UnionPlan tests UNION ALL of two scans against real FDB.
func TestIntegration_UnionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(10001), Price: proto.Int32(100)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(20001), Name: proto.String("Gina")},
		&gen.Customer{CustomerId: proto.Int64(20002), Name: proto.String("Hank")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		orderScan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		customerScan := plans.NewRecordQueryScanPlan([]string{"Customer"}, nil, false)
		union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{orderScan, customerScan})

		cursor, err := ExecutePlan(ctx, union, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("union returned %d results, want 3 (1 order + 2 customers)", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IntersectionPlan tests N-way intersection using PK overlap.
func TestIntegration_IntersectionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(11001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(11002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(11003), Price: proto.Int32(90)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan1 := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		scan2 := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		intersection := plans.NewRecordQueryIntersectionPlan(
			[]plans.RecordQueryPlan{scan1, scan2},
			nil,
		)

		cursor, err := ExecutePlan(ctx, intersection, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("intersection returned %d results, want 3 (same scan twice → all 3 overlap)", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_StreamingAggregation_CountAndSum tests streaming aggregation with
// COUNT and SUM against real FDB data.
func TestIntegration_StreamingAggregation_CountAndSum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(12001), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(12002), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(12003), Price: proto.Int32(200), Quantity: proto.Int32(1)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{&values.FieldValue{Field: "PRICE"}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "ORDER_ID"}},
				{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "QUANTITY"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("aggregation returned %d groups, want 2 (price=100, price=200)", len(results))
		}

		for _, r := range results {
			datum := r.Datum.(map[string]any)
			price := datum["PRICE"].(int64)
			count := datum["COUNT(ORDER_ID)"].(int64)
			sumQty := datum["SUM(QUANTITY)"].(int64)

			switch price {
			case 100:
				if count != 2 {
					t.Errorf("price=100 count=%d, want 2", count)
				}
				if sumQty != 5 {
					t.Errorf("price=100 sum(qty)=%v, want 5", sumQty)
				}
			case 200:
				if count != 1 {
					t.Errorf("price=200 count=%d, want 1", count)
				}
				if sumQty != 1 {
					t.Errorf("price=200 sum(qty)=%v, want 1", sumQty)
				}
			default:
				t.Errorf("unexpected price group: %d", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_NoGroupBy tests COUNT without GROUP BY.
func TestIntegration_Aggregation_NoGroupBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(13001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(13002), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(13003), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(13004), Price: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "ORDER_ID"}},
				{Function: expressions.AggMin, Operand: &values.FieldValue{Field: "PRICE"}},
				{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "PRICE"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("no-group agg returned %d results, want 1", len(results))
		}

		datum := results[0].Datum.(map[string]any)
		if datum["COUNT(ORDER_ID)"] != int64(4) {
			t.Errorf("COUNT = %v, want 4", datum["COUNT(ORDER_ID)"])
		}
		if datum["MIN(PRICE)"] != int64(10) {
			t.Errorf("MIN = %v, want 10", datum["MIN(PRICE)"])
		}
		if datum["MAX(PRICE)"] != int64(40) {
			t.Errorf("MAX = %v, want 40", datum["MAX(PRICE)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterSortProjection_Pipeline tests the full pipeline:
// scan → filter → sort → project → limit (all plan types chained).
// TestIntegration_IndexScan_Reverse tests reverse-order index scan.
func TestIntegration_IndexScan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15001), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(15002), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(15003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		indexPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			nil,
			true, // reverse
		)

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("reverse index scan returned %d results, want 3", len(results))
		}

		prices := make([]int64, len(results))
		for i, r := range results {
			prices[i] = r.Datum.(map[string]any)["PRICE"].(int64)
		}
		if prices[0] != 300 || prices[1] != 200 || prices[2] != 100 {
			t.Errorf("reverse scan prices = %v, want [300 200 100]", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_LimitWithOffset tests LIMIT with OFFSET > 0.
func TestIntegration_LimitWithOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15101), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15102), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15103), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(15104), Price: proto.Int32(40)},
		&gen.Order{OrderId: proto.Int64(15105), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "PRICE"}, Reverse: false}},
			scan,
		)
		// OFFSET 2, LIMIT 2 — skip first 2 (price=10,20), take next 2 (price=30,40)
		limited := plans.NewRecordQueryLimitPlan(sorted, 2, 2)

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("limit+offset returned %d results, want 2", len(results))
		}

		d0 := results[0].Datum.(map[string]any)
		d1 := results[1].Datum.(map[string]any)
		if d0["PRICE"] != int64(30) {
			t.Errorf("first result price = %v, want 30", d0["PRICE"])
		}
		if d1["PRICE"] != int64(40) {
			t.Errorf("second result price = %v, want 40", d1["PRICE"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_MinMaxAvg tests MIN, MAX, and AVG aggregate functions.
func TestIntegration_Aggregation_MinMaxAvg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15201), Price: proto.Int32(100), Quantity: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15202), Price: proto.Int32(200), Quantity: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15203), Price: proto.Int32(300), Quantity: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(15204), Price: proto.Int32(400), Quantity: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil, // no grouping keys — aggregate over all
			[]expressions.AggregateSpec{
				{Function: expressions.AggMin, Operand: &values.FieldValue{Field: "PRICE"}},
				{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "PRICE"}},
				{Function: expressions.AggAvg, Operand: &values.FieldValue{Field: "PRICE"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation returned %d groups, want 1", len(results))
		}

		d := results[0].Datum.(map[string]any)
		if d["MIN(PRICE)"] != int64(100) {
			t.Errorf("MIN(PRICE) = %v, want 100", d["MIN(PRICE)"])
		}
		if d["MAX(PRICE)"] != int64(400) {
			t.Errorf("MAX(PRICE) = %v, want 400", d["MAX(PRICE)"])
		}
		avg, ok := d["AVG(PRICE)"].(float64)
		if !ok || avg != 250.0 {
			t.Errorf("AVG(PRICE) = %v, want 250.0", d["AVG(PRICE)"])
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
		&gen.Order{OrderId: proto.Int64(15301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(15302), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(15303), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(75)),
					},
				),
			},
			scan,
		)
		del := plans.NewRecordQueryDeletePlan(filter, "Order")

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
		scanAll := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
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
		price := remaining[0].Datum.(map[string]any)["PRICE"].(int64)
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
		&gen.Order{OrderId: proto.Int64(15401), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15402), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15403), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "ORDER_ID"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ParameterValue{Ordinal: 1},
					},
				),
			},
			scan,
		)
		del := plans.NewRecordQueryDeletePlan(filter, "Order")

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
		scanAll := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
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

// TestIntegration_Aggregation_GroupBy_MultiFunc tests grouped aggregation with multiple functions.
func TestIntegration_Aggregation_GroupBy_MultiFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15501), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(15502), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(15503), Price: proto.Int32(200), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(15504), Price: proto.Int32(200), Quantity: proto.Int32(7)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{&values.FieldValue{Field: "PRICE"}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "ORDER_ID"}},
				{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "QUANTITY"}},
				{Function: expressions.AggMin, Operand: &values.FieldValue{Field: "QUANTITY"}},
				{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "QUANTITY"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("grouped agg returned %d groups, want 2", len(results))
		}

		byPrice := make(map[int64]map[string]any)
		for _, r := range results {
			d := r.Datum.(map[string]any)
			p := d["PRICE"].(int64)
			byPrice[p] = d
		}

		g100 := byPrice[100]
		if g100 == nil {
			t.Fatal("no group for PRICE=100")
		}
		if g100["COUNT(ORDER_ID)"] != int64(2) {
			t.Errorf("COUNT(ORDER_ID) for price=100: %v, want 2", g100["COUNT(ORDER_ID)"])
		}
		if g100["SUM(QUANTITY)"] != int64(3) {
			t.Errorf("SUM(QUANTITY) for price=100: %v, want 3", g100["SUM(QUANTITY)"])
		}
		if g100["MIN(QUANTITY)"] != int64(1) {
			t.Errorf("MIN(QUANTITY) for price=100: %v, want 1", g100["MIN(QUANTITY)"])
		}
		if g100["MAX(QUANTITY)"] != int64(2) {
			t.Errorf("MAX(QUANTITY) for price=100: %v, want 2", g100["MAX(QUANTITY)"])
		}

		g200 := byPrice[200]
		if g200 == nil {
			t.Fatal("no group for PRICE=200")
		}
		if g200["COUNT(ORDER_ID)"] != int64(2) {
			t.Errorf("COUNT(ORDER_ID) for price=200: %v, want 2", g200["COUNT(ORDER_ID)"])
		}
		if g200["SUM(QUANTITY)"] != int64(10) {
			t.Errorf("SUM(QUANTITY) for price=200: %v, want 10", g200["SUM(QUANTITY)"])
		}
		if g200["MIN(QUANTITY)"] != int64(3) {
			t.Errorf("MIN(QUANTITY) for price=200: %v, want 3", g200["MIN(QUANTITY)"])
		}
		if g200["MAX(QUANTITY)"] != int64(7) {
			t.Errorf("MAX(QUANTITY) for price=200: %v, want 7", g200["MAX(QUANTITY)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FilterSortProjection_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(14001), Price: proto.Int32(500), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(14002), Price: proto.Int32(300), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(14003), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(14004), Price: proto.Int32(400), Quantity: proto.Int32(4)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(200)),
					},
				),
			},
			scan,
		)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "PRICE"}, Reverse: false}},
			filter,
		)
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				&values.FieldValue{Field: "PRICE"},
				&values.FieldValue{Field: "QUANTITY"},
			},
			sorted,
		)
		limited := plans.NewRecordQueryLimitPlan(proj, 2, 0)

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("pipeline returned %d results, want 2", len(results))
		}

		d0 := results[0].Datum.(map[string]any)
		d1 := results[1].Datum.(map[string]any)
		if d0["PRICE"] != int64(300) || d0["QUANTITY"] != int64(3) {
			t.Errorf("first row = %v, want PRICE=300/QUANTITY=3", d0)
		}
		if d1["PRICE"] != int64(400) || d1["QUANTITY"] != int64(4) {
			t.Errorf("second row = %v, want PRICE=400/QUANTITY=4", d1)
		}
		if _, exists := d0["ORDER_ID"]; exists {
			t.Error("ORDER_ID should be excluded by projection")
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

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(17101), Price: proto.Int32(42),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		ins := plans.NewRecordQueryInsertPlan(scan, "Order", nil)

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

	// VALUES (7001, 42), (7002, 43) — order_id is int64, price is int32.
	// The int32 column is fed an int64 literal; goToProtoValue narrows it.
	mkRow := func(id, price int64) *values.RecordConstructorValue {
		return values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "order_id", Value: &values.ConstantValue{Value: id, Typ: values.NullableLong}},
			values.RecordConstructorField{Name: "price", Value: &values.ConstantValue{Value: price, Typ: values.NullableLong}},
		)
	}
	arr := values.NewArrayConstructorValue(values.UnknownType, []values.Value{mkRow(7001, 42), mkRow(7002, 43)})
	explode := plans.NewRecordQueryExplodePlan(arr)
	ins := plans.NewRecordQueryInsertPlan(explode, "Order", nil)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
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
			o, ok := r.Record.Record.(*gen.Order)
			if !ok {
				t.Fatalf("scanned record type = %T, want *gen.Order", r.Record.Record)
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

// TestIntegration_Aggregation_EmptyInput tests aggregation over an
// empty result set: COUNT→0, SUM→0, MIN/MAX→nil.
func TestIntegration_Aggregation_EmptyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "ORDER_ID"}},
				{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "PRICE"}},
				{Function: expressions.AggMin, Operand: &values.FieldValue{Field: "PRICE"}},
				{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "PRICE"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation over empty input returned %d rows, want 1", len(results))
		}
		d := results[0].Datum.(map[string]any)
		if d["COUNT(ORDER_ID)"] != int64(0) {
			t.Errorf("COUNT(ORDER_ID) = %v, want 0", d["COUNT(ORDER_ID)"])
		}
		if d["SUM(PRICE)"] != nil {
			t.Errorf("SUM(PRICE) = %v, want nil (SQL NULL for empty set)", d["SUM(PRICE)"])
		}
		if d["MIN(PRICE)"] != nil {
			t.Errorf("MIN(PRICE) = %v, want nil", d["MIN(PRICE)"])
		}
		if d["MAX(PRICE)"] != nil {
			t.Errorf("MAX(PRICE) = %v, want nil", d["MAX(PRICE)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_MultipleFields tests updating two
// fields in a single UPDATE plan.
func TestIntegration_UpdatePlan_MultipleFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "ORDER_ID"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(17201)),
					},
				),
			},
			scan,
		)
		update := plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "PRICE", NewValue: values.LiteralValue(int64(999))},
			{FieldPath: "QUANTITY", NewValue: values.LiteralValue(int64(42))},
		})

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(17201)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 999 {
			t.Errorf("price after update = %d, want 999", updated.GetPrice())
		}
		if updated.GetQuantity() != 42 {
			t.Errorf("quantity after update = %d, want 42", updated.GetQuantity())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_EqualityRange tests an index scan with
// an exact equality comparison range.
func TestIntegration_IndexScan_EqualityRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(17302), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17303), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17304), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue(int64(100)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		idxScan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			nil,
			false,
		)

		cursor, err := ExecutePlan(ctx, idxScan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("equality index scan returned %d results, want 2", len(results))
		}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			if d["PRICE"] != int64(100) {
				t.Errorf("PRICE = %v, want 100", d["PRICE"])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SortPlan_MultiKey tests sorting by two fields:
// primary sort by PRICE ascending, secondary sort by QUANTITY descending.
func TestIntegration_SortPlan_MultiKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17401), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(17402), Price: proto.Int32(200), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(17403), Price: proto.Int32(100), Quantity: proto.Int32(7)},
		&gen.Order{OrderId: proto.Int64(17404), Price: proto.Int32(200), Quantity: proto.Int32(9)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sorted := plans.NewRecordQuerySortPlan(
			[]expressions.SortKey{
				{Value: &values.FieldValue{Field: "PRICE"}, Reverse: false},
				{Value: &values.FieldValue{Field: "QUANTITY"}, Reverse: true},
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, sorted, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 4 {
			t.Fatalf("sort returned %d results, want 4", len(results))
		}

		d0 := results[0].Datum.(map[string]any)
		d1 := results[1].Datum.(map[string]any)
		d2 := results[2].Datum.(map[string]any)
		d3 := results[3].Datum.(map[string]any)
		if d0["PRICE"] != int64(100) || d0["QUANTITY"] != int64(7) {
			t.Errorf("row 0: PRICE=%v QUANTITY=%v, want 100/7", d0["PRICE"], d0["QUANTITY"])
		}
		if d1["PRICE"] != int64(100) || d1["QUANTITY"] != int64(3) {
			t.Errorf("row 1: PRICE=%v QUANTITY=%v, want 100/3", d1["PRICE"], d1["QUANTITY"])
		}
		if d2["PRICE"] != int64(200) || d2["QUANTITY"] != int64(9) {
			t.Errorf("row 2: PRICE=%v QUANTITY=%v, want 200/9", d2["PRICE"], d2["QUANTITY"])
		}
		if d3["PRICE"] != int64(200) || d3["QUANTITY"] != int64(1) {
			t.Errorf("row 3: PRICE=%v QUANTITY=%v, want 200/1", d3["PRICE"], d3["QUANTITY"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UnionPlan_DisjointLegs tests UNION of two
// non-overlapping filter legs — output is the bag union.
func TestIntegration_UnionPlan_DisjointLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17501), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(17502), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17503), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scanA := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filterLow := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonLessThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanA,
		)

		scanB := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filterHigh := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanB,
		)

		union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{filterLow, filterHigh})

		cursor, err := ExecutePlan(ctx, union, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("union returned %d results, want 2 (disjoint legs)", len(results))
		}

		prices := map[int64]bool{}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			prices[d["PRICE"].(int64)] = true
		}
		if !prices[50] {
			t.Error("expected PRICE=50 in union output")
		}
		if !prices[150] {
			t.Error("expected PRICE=150 in union output")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan_NoMatch tests that filtering with no
// matching records returns an empty cursor.
func TestIntegration_FilterPlan_NoMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17601), Price: proto.Int32(10)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "PRICE"},
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(99999)),
					},
				),
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("filter with no match returned %d results, want 0", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DeletePlan_AllRecords tests deleting all records
// via an unfiltered scan→delete pipeline.
func TestIntegration_DeletePlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17701), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(17702), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(17703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		del := plans.NewRecordQueryDeletePlan(scan, "Order")

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

// TestIntegration_TypeFilter_MixedRecordTypes stores both Order and
// TypedRecord in the same subspace and verifies that TypeFilter
// correctly separates them during scan.
func TestIntegration_TypeFilter_MixedRecordTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(18001), Price: proto.Int32(100)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(18002), Price: proto.Int32(200)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.TypedRecord{Id: proto.Int64(28001), ValString: proto.String("hello")}); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert mixed records: %v", err)
	}

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order", "TypedRecord"}, nil, false)
		typeFilter := plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan)

		cursor, err := ExecutePlan(ctx, typeFilter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("TypeFilter(Order) returned %d results, want 2", len(results))
		}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			if _, ok := d["ORDER_ID"]; !ok {
				t.Errorf("expected ORDER_ID in type-filtered result, got %v", d)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ScanPlan_UnsetFieldsOmitted verifies that proto
// fields not set on a record are omitted from the datum map
// (protoToMap only includes set fields).
func TestIntegration_ScanPlan_UnsetFieldsOmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)

		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("scan returned %d results, want 1", len(results))
		}

		d := results[0].Datum.(map[string]any)
		if d["ORDER_ID"] != int64(18101) {
			t.Errorf("ORDER_ID = %v, want 18101", d["ORDER_ID"])
		}
		if d["PRICE"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", d["PRICE"])
		}
		if _, ok := d["QUANTITY"]; ok {
			t.Error("QUANTITY was not set but appears in datum")
		}
		if _, ok := d["CUSTOMER_ID"]; ok {
			t.Error("CUSTOMER_ID was not set but appears in datum")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan_IsNull tests filtering for records where
// an optional field IS NULL (not set). A field missing from the datum
// map evaluates to nil; equality comparison with nil should not match.
func TestIntegration_FilterPlan_IsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(18202), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		filter := plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "QUANTITY"},
					predicates.Comparison{Type: predicates.ComparisonIsNull},
				),
			},
			scan,
		)

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("IS NULL filter returned %d results, want 1", len(results))
		}
		d := results[0].Datum.(map[string]any)
		if d["ORDER_ID"] != int64(18202) {
			t.Errorf("ORDER_ID = %v, want 18202", d["ORDER_ID"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_AVG verifies AVG computation including
// floating-point result.
func TestIntegration_Aggregation_AVG(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18301), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(18302), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(18303), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggAvg, Operand: &values.FieldValue{Field: "PRICE"}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation returned %d rows, want 1", len(results))
		}
		d := results[0].Datum.(map[string]any)
		avg, ok := d["AVG(PRICE)"].(float64)
		if !ok {
			t.Fatalf("AVG(PRICE) type = %T, want float64", d["AVG(PRICE)"])
		}
		if avg != 20.0 {
			t.Errorf("AVG(PRICE) = %v, want 20.0", avg)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_StreamingAggregation_SortedInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19001), Quantity: proto.Int32(1), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(19002), Quantity: proto.Int32(1), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(19003), Quantity: proto.Int32(2), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(19004), Quantity: proto.Int32(2), Price: proto.Int32(400)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sort := plans.NewRecordQuerySortPlan([]expressions.SortKey{
			{Value: &values.FieldValue{Field: "QUANTITY"}, Reverse: false},
		}, scan)
		agg := plans.NewRecordQueryStreamingAggregationPlan(
			sort,
			[]values.Value{&values.FieldValue{Field: "QUANTITY", Typ: values.TypeInt}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}},
				{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "PRICE", Typ: values.TypeInt}},
			},
		)

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("streaming agg returned %d rows, want 2", len(results))
		}

		qtyCounts := map[int64]int64{}
		qtySums := map[int64]int64{}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			qty, ok := d["QUANTITY"].(int64)
			if !ok {
				t.Fatalf("QUANTITY type = %T (value = %v), datum keys = %v", d["QUANTITY"], d["QUANTITY"], d)
			}
			cnt := d["COUNT(CONSTANT)"].(int64)
			sum := d["SUM(PRICE)"].(int64)
			qtyCounts[qty] = cnt
			qtySums[qty] = sum
		}
		if qtyCounts[1] != 2 || qtyCounts[2] != 2 {
			t.Errorf("counts: %v", qtyCounts)
		}
		if qtySums[1] != 300.0 || qtySums[2] != 700.0 {
			t.Errorf("sums: %v", qtySums)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_ProjectionOverJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19101), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(19102), Price: proto.Int32(75)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan1 := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		scan2 := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		nlj := plans.NewRecordQueryNestedLoopJoinPlan(scan1, scan2, nil, plans.JoinInner, "ORDER", "ORDER", nil)
		proj := plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				&values.FieldValue{Field: "ORDER_ID", Typ: values.TypeInt},
				&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
			},
			nlj,
		)

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 4 {
			t.Fatalf("projection over cross join: %d rows, want 4 (2×2)", len(results))
		}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			if _, ok := d["ORDER_ID"]; !ok {
				t.Fatalf("missing ORDER_ID in projected datum: %v", d)
			}
			if _, ok := d["PRICE"]; !ok {
				t.Fatalf("missing PRICE in projected datum: %v", d)
			}
			if _, ok := d["QUANTITY"]; ok {
				t.Fatalf("QUANTITY should be projected out: %v", d)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_SortPlan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19201), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19202), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(19203), Price: proto.Int32(20)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sort := plans.NewRecordQuerySortPlan([]expressions.SortKey{
			{Value: &values.FieldValue{Field: "PRICE"}, Reverse: true},
		}, scan)

		cursor, err := ExecutePlan(ctx, sort, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("sort returned %d rows, want 3", len(results))
		}
		prices := make([]int64, len(results))
		for i, r := range results {
			d := r.Datum.(map[string]any)
			prices[i] = d["PRICE"].(int64)
		}
		if prices[0] != int64(30) || prices[1] != int64(20) || prices[2] != int64(10) {
			t.Errorf("expected [30 20 10], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FilterPlan_CompoundPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19301), Price: proto.Int32(50), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(19302), Price: proto.Int32(150), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(19303), Price: proto.Int32(50), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(19304), Price: proto.Int32(150), Quantity: proto.Int32(2)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		pricePred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
		)
		qtyPred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "QUANTITY", Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
		andPred := predicates.NewAnd(pricePred, qtyPred)
		filter := plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{andPred}, scan)

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("compound filter returned %d rows, want 1 (PRICE>100 AND QUANTITY=1)", len(results))
		}
		d := results[0].Datum.(map[string]any)
		if d["ORDER_ID"] != int64(19302) {
			t.Errorf("ORDER_ID = %v, want 19302", d["ORDER_ID"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_LimitOverSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19401), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(19402), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(19403), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(19404), Price: proto.Int32(400)},
		&gen.Order{OrderId: proto.Int64(19405), Price: proto.Int32(500)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		sort := plans.NewRecordQuerySortPlan([]expressions.SortKey{
			{Value: &values.FieldValue{Field: "PRICE"}, Reverse: true},
		}, scan)
		limit := plans.NewRecordQueryLimitPlan(sort, int64(3), int64(0))

		cursor, err := ExecutePlan(ctx, limit, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("limit over sort returned %d rows, want 3", len(results))
		}
		prices := []int64{}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			prices = append(prices, d["PRICE"].(int64))
		}
		if prices[0] != 500 || prices[1] != 400 || prices[2] != 300 {
			t.Errorf("expected top-3 DESC [500 400 300], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_UpdatePlan_ClearField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19501), Price: proto.Int32(100), Quantity: proto.Int32(5)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		update := plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{
			{FieldPath: "QUANTITY", NewValue: values.LiteralValue(nil)},
		})

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(19501)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		updated := rec.Record.(*gen.Order)
		if updated.Quantity != nil {
			t.Errorf("quantity should be cleared (nil), got %v", updated.GetQuantity())
		}
		if updated.GetPrice() != 100 {
			t.Errorf("price should be unchanged at 100, got %d", updated.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FilterPlan_OrPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19601), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19602), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(19603), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
		lowPred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(20)),
		)
		highPred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "PRICE", Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(100)),
		)
		orPred := predicates.NewOr(lowPred, highPred)
		filter := plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{orPred}, scan)

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("OR filter returned %d rows, want 2 (PRICE<20 OR PRICE>=100)", len(results))
		}
		ids := map[int64]bool{}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			ids[d["ORDER_ID"].(int64)] = true
		}
		if !ids[19601] || !ids[19603] {
			t.Errorf("expected orders 19601 and 19603, got %v", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_IndexScan_FullRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19701), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19702), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(19703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		idxPlan := plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			values.UnknownType,
			false,
		)

		cursor, err := ExecutePlan(ctx, idxPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("full index scan returned %d rows, want 3", len(results))
		}
		prices := []int64{}
		for _, r := range results {
			d := r.Datum.(map[string]any)
			prices = append(prices, d["PRICE"].(int64))
		}
		if prices[0] != 10 || prices[1] != 20 || prices[2] != 30 {
			t.Errorf("expected ASC order [10 20 30], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
