package executor

import (
	"context"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_IndexScan tests index scan via ComparisonRange.
func TestIntegration_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(301), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(302), Price: proto.Int32(150)},
		&demopb.Order{OrderId: proto.Int64(303), Price: proto.Int32(250)},
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
		if !res.Complete() {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			price, _ := rowMap(r)["price"].(int64)
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

// TestIntegration_IndexScan_Equality tests index scan with equality match.
func TestIntegration_IndexScan_Equality(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(901), Price: proto.Int32(77)},
		&demopb.Order{OrderId: proto.Int64(902), Price: proto.Int32(88)},
		&demopb.Order{OrderId: proto.Int64(903), Price: proto.Int32(77)},
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
		if !res.Complete() {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			price, _ := rowMap(r)["price"].(int64)
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
		&demopb.Order{OrderId: proto.Int64(1001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(1002), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(1003), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(1004), Price: proto.Int32(150)},
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
		if !lowRes.Complete() {
			t.Fatal("merge low failed")
		}
		highRes := lowRes.Range.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonLessThan,
			Operand: values.LiteralValue(int64(150)),
		})
		if !highRes.Complete() {
			t.Fatal("merge high failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{highRes.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			price, _ := rowMap(r)["price"].(int64)
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

// TestIntegration_ParameterBinding_IndexScan tests index scan with a parameter
// in the comparison range.
func TestIntegration_ParameterBinding_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(8101), Price: proto.Int32(25)},
		&demopb.Order{OrderId: proto.Int64(8102), Price: proto.Int32(75)},
		&demopb.Order{OrderId: proto.Int64(8103), Price: proto.Int32(125)},
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
		if !res.Complete() {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			price, _ := rowMap(r)["price"].(int64)
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

// TestIntegration_IndexScan_Reverse tests reverse-order index scan.
func TestIntegration_IndexScan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15001), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(15002), Price: proto.Int32(200)},
		&demopb.Order{OrderId: proto.Int64(15003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			integrationOrderType(),
			true, // reverse
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			prices[i] = rowMap(r)["price"].(int64)
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

// TestIntegration_IndexScan_EqualityRange tests an index scan with
// an exact equality comparison range.
func TestIntegration_IndexScan_EqualityRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(17301), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(17302), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(17303), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(17304), Price: proto.Int32(200)},
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
		if !res.Complete() {
			t.Fatal("merge failed")
		}

		idxScan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			d, _ := rowMapOK(r)
			if d["price"] != int64(100) {
				t.Errorf("PRICE = %v, want 100", d["price"])
			}
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
		&demopb.Order{OrderId: proto.Int64(19701), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(19702), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(19703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		idxPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

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
			d, _ := rowMapOK(r)
			prices = append(prices, d["price"].(int64))
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
