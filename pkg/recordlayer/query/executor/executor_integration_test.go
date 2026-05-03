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
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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

// TestIntegration_HashAggregation_CountAndSum tests hash aggregation with
// COUNT and SUM against real FDB data.
func TestIntegration_HashAggregation_CountAndSum(t *testing.T) {
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
		agg := plans.NewRecordQueryHashAggregationPlan(
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
			sumQty := datum["SUM(QUANTITY)"].(float64)

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
		agg := plans.NewRecordQueryHashAggregationPlan(
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
