package executor

import (
	"context"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_FilterPlan tests filtering records by a predicate.
func TestIntegration_FilterPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(100),
		Price:   proto.Int32(50),
	}, &demopb.Order{
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

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scan,
		))

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
		price, _ := rowMap(results[0])["price"]
		if price != int64(500) {
			t.Errorf("price = %v, want 500", price)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ProjectionRowsAreOrdinal: a projection over a scan on real FDB
// returns rows carrying the projected columns in projection order.
func TestIntegration_ProjectionRowsAreOrdinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(2001), Price: proto.Int32(77)},
		&demopb.Order{OrderId: proto.Int64(2002), Price: proto.Int32(88)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{integrationField(t, scan, 0), integrationField(t, scan, 2)},
			scan,
		))
		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		rows := collectSlots(t, ctx, cursor)
		want := map[int64]int64{2001: 77, 2002: 88}
		if len(rows) != len(want) {
			t.Fatalf("got %d rows, want %d", len(rows), len(want))
		}
		for _, row := range rows {
			if len(row) != 2 {
				t.Fatalf("row %v has %d slots, want 2 (ORDER_ID, PRICE)", row, len(row))
			}
			id, _ := row[0].(int64)
			price, _ := row[1].(int64)
			if wantPrice, ok := want[id]; !ok || price != wantPrice {
				t.Errorf("row = %v, want ORDER_ID with its PRICE from %v", row, want)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterSortProjectionRows runs filter + sort + projection
// against real FDB and checks the ordinal rows' values and order.
func TestIntegration_FilterSortProjectionRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(4001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(4002), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(4003), Price: proto.Int32(90)},
		&demopb.Order{OrderId: proto.Int64(4004), Price: proto.Int32(30)},
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
						Type:    predicates.ComparisonGreaterThanEq,
						Operand: values.LiteralValue(int64(30)),
					},
				),
			},
			scan,
		))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			filter,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{integrationField(t, sorted, 2), integrationField(t, sorted, 0)},
			sorted,
		))
		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		rows := collectSlots(t, ctx, cursor)
		want := [][]any{{int64(30), int64(4004)}, {int64(50), int64(4002)}, {int64(90), int64(4003)}}
		if len(rows) != len(want) {
			t.Fatalf("got %d rows, want 3 (prices >= 30): %v", len(rows), rows)
		}
		for i := range want {
			if len(rows[i]) != 2 || rows[i][0] != want[i][0] || rows[i][1] != want[i][1] {
				t.Errorf("row %d = %v, want %v (PRICE, ORDER_ID ascending by PRICE)", i, rows[i], want[i])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ProjectionPlan tests projecting specific columns from scan results.
func TestIntegration_ProjectionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(6001), Price: proto.Int32(111)},
		&demopb.Order{OrderId: proto.Int64(6002), Price: proto.Int32(222)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{
				integrationField(t, scan, 2),
			},
			scan,
		))

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
			datum, _ := rowMapOK(r)
			if _, exists := datum["price"]; !exists {
				t.Error("projected datum should contain PRICE")
			}
			if _, exists := datum["order_id"]; exists {
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
		&demopb.Order{OrderId: proto.Int64(6101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{
				integrationField(t, scan, 0),
				integrationField(t, scan, 2),
			},
			scan,
		))

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

		datum, _ := rowMapOK(results[0])
		if datum["order_id"] != int64(6101) {
			t.Errorf("ORDER_ID = %v, want 6101", datum["order_id"])
		}
		if datum["price"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", datum["price"])
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

// TestIntegration_ParameterBinding_Filter tests filter with prepared-statement
// parameter against real FDB records.
func TestIntegration_ParameterBinding_Filter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(8001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(8002), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(8003), Price: proto.Int32(90)},
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
						Operand: values.NewParameterValue(1),
					},
				),
			},
			scan,
		))

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
			price, _ := rowMap(r)["price"].(int64)
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

// TestIntegration_FilterSortProjection_Pipeline tests the full pipeline:
// scan → filter → sort → project → limit (all plan types chained).
func TestIntegration_FilterSortProjection_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(14001), Price: proto.Int32(500), Quantity: proto.Int32(5)},
		&demopb.Order{OrderId: proto.Int64(14002), Price: proto.Int32(300), Quantity: proto.Int32(3)},
		&demopb.Order{OrderId: proto.Int64(14003), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&demopb.Order{OrderId: proto.Int64(14004), Price: proto.Int32(400), Quantity: proto.Int32(4)},
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
						Operand: values.LiteralValue(int64(200)),
					},
				),
			},
			scan,
		))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			filter,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{
				integrationField(t, sorted, 2),
				integrationField(t, sorted, 4),
			},
			sorted,
		))
		limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(proj, 2, 0))

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

		d0, _ := rowMapOK(results[0])
		d1, _ := rowMapOK(results[1])
		if d0["price"] != int64(300) || d0["quantity"] != int64(3) {
			t.Errorf("first row = %v, want PRICE=300/QUANTITY=3", d0)
		}
		if d1["price"] != int64(400) || d1["quantity"] != int64(4) {
			t.Errorf("second row = %v, want PRICE=400/QUANTITY=4", d1)
		}
		if _, exists := d0["order_id"]; exists {
			t.Error("ORDER_ID should be excluded by projection")
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
		&demopb.Order{OrderId: proto.Int64(17601), Price: proto.Int32(10)},
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
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(99999)),
					},
				),
			},
			scan,
		))

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

// TestIntegration_FilterPlan_IsNull tests filtering for records where
// an optional field IS NULL (not set). A field missing from the datum
// map evaluates to nil; equality comparison with nil should not match.
func TestIntegration_FilterPlan_IsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(18201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&demopb.Order{OrderId: proto.Int64(18202), Price: proto.Int32(200)},
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
					integrationField(t, scan, 4),
					predicates.Comparison{Type: predicates.ComparisonIsNull},
				),
			},
			scan,
		))

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
		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(18202) {
			t.Errorf("ORDER_ID = %v, want 18202", d["order_id"])
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
		&demopb.Order{OrderId: proto.Int64(19301), Price: proto.Int32(50), Quantity: proto.Int32(1)},
		&demopb.Order{OrderId: proto.Int64(19302), Price: proto.Int32(150), Quantity: proto.Int32(1)},
		&demopb.Order{OrderId: proto.Int64(19303), Price: proto.Int32(50), Quantity: proto.Int32(2)},
		&demopb.Order{OrderId: proto.Int64(19304), Price: proto.Int32(150), Quantity: proto.Int32(2)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		pricePred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
		)
		qtyPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 4),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
		andPred := predicates.NewAnd(pricePred, qtyPred)
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{andPred}, scan))

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
		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(19302) {
			t.Errorf("ORDER_ID = %v, want 19302", d["order_id"])
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
		&demopb.Order{OrderId: proto.Int64(19601), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(19602), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(19603), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		lowPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(20)),
		)
		highPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(100)),
		)
		orPred := predicates.NewOr(lowPred, highPred)
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{orPred}, scan))

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
			d, _ := rowMapOK(r)
			ids[d["order_id"].(int64)] = true
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
