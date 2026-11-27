package executor

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_StreamingAggregation_CountAndSum tests streaming aggregation with
// COUNT and SUM against real FDB data.
func TestIntegration_StreamingAggregation_CountAndSum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(12001), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&demopb.Order{OrderId: proto.Int64(12002), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&demopb.Order{OrderId: proto.Int64(12003), Price: proto.Int32(200), Quantity: proto.Int32(1)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{integrationField(t, scan, 2)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
			},
		))

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
			datum, _ := rowMapOK(r)
			price := datum["price"].(int64)
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
		&demopb.Order{OrderId: proto.Int64(13001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(13002), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(13003), Price: proto.Int32(30)},
		&demopb.Order{OrderId: proto.Int64(13004), Price: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

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

		datum, _ := rowMapOK(results[0])
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

// TestIntegration_Aggregation_MinMaxAvg tests MIN, MAX, and AVG aggregate functions.
func TestIntegration_Aggregation_MinMaxAvg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15201), Price: proto.Int32(100), Quantity: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(15202), Price: proto.Int32(200), Quantity: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(15203), Price: proto.Int32(300), Quantity: proto.Int32(30)},
		&demopb.Order{OrderId: proto.Int64(15204), Price: proto.Int32(400), Quantity: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil, // no grouping keys — aggregate over all
			[]expressions.AggregateSpec{
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggAvg, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

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

		d, _ := rowMapOK(results[0])
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

// TestIntegration_Aggregation_GroupBy_MultiFunc tests grouped aggregation with multiple functions.
func TestIntegration_Aggregation_GroupBy_MultiFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15501), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&demopb.Order{OrderId: proto.Int64(15502), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&demopb.Order{OrderId: proto.Int64(15503), Price: proto.Int32(200), Quantity: proto.Int32(3)},
		&demopb.Order{OrderId: proto.Int64(15504), Price: proto.Int32(200), Quantity: proto.Int32(7)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{integrationField(t, scan, 2)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
			},
		))

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
			d, _ := rowMapOK(r)
			p := d["price"].(int64)
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

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

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
		d, _ := rowMapOK(results[0])
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

// TestIntegration_Aggregation_AVG verifies AVG computation including
// floating-point result.
func TestIntegration_Aggregation_AVG(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(18301), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(18302), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(18303), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggAvg, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

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
		d, _ := rowMapOK(results[0])
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
		&demopb.Order{OrderId: proto.Int64(19001), Quantity: proto.Int32(1), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(19002), Quantity: proto.Int32(1), Price: proto.Int32(200)},
		&demopb.Order{OrderId: proto.Int64(19003), Quantity: proto.Int32(2), Price: proto.Int32(300)},
		&demopb.Order{OrderId: proto.Int64(19004), Quantity: proto.Int32(2), Price: proto.Int32(400)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sort := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{
			{Field: "quantity", ValueExpr: integrationField(t, scan, 4), Desc: false},
		}))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			sort,
			[]values.Value{integrationField(t, scan, 4)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}, OperandName: "CONSTANT"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

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
			d, _ := rowMapOK(r)
			qty, ok := d["quantity"].(int64)
			if !ok {
				t.Fatalf("QUANTITY type = %T (value = %v), datum keys = %v", d["quantity"], d["quantity"], d)
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

// An ARRAY_AGG group straddling scan-limited pages resumes with its collected
// elements.
func TestIntegration_ArrayAggStraddle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(4), Price: proto.Int32(150)},
		&demopb.Order{OrderId: proto.Int64(5), Price: proto.Int32(150)},
	)
	var got []string
	var continuation []byte
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatal("resume loop did not converge")
		}
		exhausted, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
				SetSubspace(testSubspace(t)).Open()
			if err != nil {
				return nil, err
			}
			scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			plan := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(scan,
				[]values.Value{integrationField(t, scan, 2)},
				[]expressions.AggregateSpec{{
					Function: expressions.AggArrayAgg, Operand: integrationField(t, scan, 0), Limit: values.ArrayAggNoLimit,
				}}))
			props := recordlayer.DefaultExecuteProperties().WithScannedRecordsLimit(1) // two records a page: the scan finishes the crossing record
			cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation, props)
			if err != nil {
				return nil, err
			}
			defer cursor.Close()
			for {
				res, oerr := cursor.OnNext(ctx)
				if oerr != nil {
					return nil, oerr
				}
				if res.HasNext() {
					k, _ := res.GetValue().Positional.Get(0)
					v, _ := res.GetValue().Positional.Get(1)
					got = append(got, fmt.Sprint(k, "=", v))
					continue
				}
				if res.GetNoNextReason().IsSourceExhausted() {
					return true, nil
				}
				cb, cerr := res.GetContinuation().ToBytes()
				if cerr != nil {
					return nil, cerr
				}
				continuation = cb
				return false, nil
			}
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if exhausted.(bool) {
			break
		}
	}
	if pages < 3 {
		t.Fatalf("finished in %d pages; the scan limit did not force a straddle", pages)
	}
	if fmt.Sprint(got) != "[50=[1 2 3] 150=[4 5]]" {
		t.Fatalf("ARRAY_AGG across pages = %v", got)
	}
}
