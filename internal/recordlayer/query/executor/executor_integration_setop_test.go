package executor

import (
	"context"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_DistinctPlan tests deduplication by primary key.
func TestIntegration_DistinctPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(7001), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(7002), Price: proto.Int32(200)},
		&demopb.Order{OrderId: proto.Int64(7003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		distinct := mustExecutorConstruct(plans.NewRecordQueryDistinctPlan(scan))

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

// TestIntegration_UnionPlan tests UNION ALL of two type-compatible scans
// against real FDB. A union of Order and Customer used to construct only
// because both inputs hid behind UnknownType; exact set-plan admission now
// correctly rejects that malformed shape before execution.
func TestIntegration_UnionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(10001), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		firstScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		secondScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		union := mustExecutorConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{firstScan, secondScan}))

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
			t.Fatalf("union returned %d results, want 2 (the one Order from each UNION ALL arm)", len(results))
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
		&demopb.Order{OrderId: proto.Int64(11001), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(11002), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(11003), Price: proto.Int32(90)},
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
		intersection := mustExecutorConstruct(plans.NewRecordQueryIntersectionPlan(
			[]plans.RecordQueryPlan{scan1, scan2},
			nil,
		))

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

// TestIntegration_UnionPlan_DisjointLegs tests UNION of two
// non-overlapping filter legs — output is the bag union.
func TestIntegration_UnionPlan_DisjointLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(17501), Price: proto.Int32(50)},
		&demopb.Order{OrderId: proto.Int64(17502), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(17503), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scanA := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filterLow := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scanA, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonLessThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanA,
		))

		scanB := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filterHigh := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scanB, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanB,
		))

		union := mustExecutorConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{filterLow, filterHigh}))

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
			d, _ := rowMapOK(r)
			prices[d["price"].(int64)] = true
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
