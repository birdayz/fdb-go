package executor

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
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
