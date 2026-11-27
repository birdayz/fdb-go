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

// TestIntegration_NestedLoopJoin_CrossJoin tests a cross join between
// Orders and Customers (no join predicate).
func TestIntegration_NestedLoopJoin_CrossJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(9001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(9002), Price: proto.Int32(20)},
	)
	insertCustomers(t, store,
		&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")},
		&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob")},
		&demopb.Customer{CustomerId: proto.Int64(3), Name: proto.String("Carol")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")
		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			nil,
			plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinInner),
		))

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
			datum, _ := rowMapOK(r)
			if datum["order_id"] == nil {
				t.Error("ORDER_ID missing from joined row")
			}
			if datum["customer_id"] == nil {
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
		&demopb.Order{OrderId: proto.Int64(9101), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&demopb.Order{OrderId: proto.Int64(9102), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&demopb.Customer{CustomerId: proto.Int64(19101), Name: proto.String("Dan")},
		&demopb.Customer{CustomerId: proto.Int64(19102), Name: proto.String("Eve")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")

		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, outerScan, 4),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinInner),
		))

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
			datum, _ := rowMapOK(r)
			if datum["order_id"] != int64(9101) {
				t.Errorf("ORDER_ID = %v, want 9101 (quantity=5)", datum["order_id"])
			}
			if datum["customer_id"] == nil {
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
		&demopb.Order{OrderId: proto.Int64(9201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&demopb.Order{OrderId: proto.Int64(9202), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&demopb.Customer{CustomerId: proto.Int64(19201), Name: proto.String("Frank")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")

		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, outerScan, 4),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinLeftOuter,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinLeftOuter),
		))

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
			datum, _ := rowMapOK(r)
			orderID := datum["order_id"].(int64)
			if orderID == 9201 {
				if datum["customer_id"] == nil {
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

func TestIntegration_ProjectionOverJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(19101), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(19102), Price: proto.Int32(75)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan1 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		scan2 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		outerAlias := values.NamedCorrelationIdentifier("OUTER_ORDER")
		innerAlias := values.NamedCorrelationIdentifier("INNER_ORDER")
		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			scan1, scan2, nil, plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, scan1, scan2, outerAlias, innerAlias, plans.JoinInner),
		))
		proj := mustExecutorConstruct(newProjectionMapOverForTest(
			[]values.Value{
				integrationField(t, nlj, 0),
				integrationField(t, nlj, 2),
			},
			nlj,
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
		if len(results) != 4 {
			t.Fatalf("projection over cross join: %d rows, want 4 (2×2)", len(results))
		}
		for _, r := range results {
			d, _ := rowMapOK(r)
			if _, ok := d["order_id"]; !ok {
				t.Fatalf("missing ORDER_ID in projected datum: %v", d)
			}
			if _, ok := d["price"]; !ok {
				t.Fatalf("missing PRICE in projected datum: %v", d)
			}
			if _, ok := d["quantity"]; ok {
				t.Fatalf("QUANTITY should be projected out: %v", d)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
