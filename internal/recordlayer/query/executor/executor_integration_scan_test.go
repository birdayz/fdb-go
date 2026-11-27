package executor

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_ScanPlan_AllRecords tests scanning all records from a real FDB store.
func TestIntegration_ScanPlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	}, &demopb.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	}, &demopb.Order{
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

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, values.NewAnyRecordType(false), false))
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
			datum, ok := rowMapOK(r)
			if !ok {
				t.Fatalf("datum type = %T, want map[string]any", r.Positional)
			}
			if datum["order_id"] == nil {
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

	insertOrders(t, store, &demopb.Order{
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

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, values.NewAnyRecordType(false), false))
		typeFilter := mustExecutorConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan))

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

// TestIntegration_ScanDatum_Shape verifies that scan datum maps contain
// the correct keys and values from proto deserialization.
func TestIntegration_ScanDatum_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(701), Price: proto.Int32(42)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))

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

		datum, _ := rowMapOK(results[0])
		if datum["order_id"] != int64(701) {
			t.Errorf("ORDER_ID = %v, want 701", datum["order_id"])
		}
		if datum["price"] != int64(42) {
			t.Errorf("PRICE = %v, want 42", datum["price"])
		}
		// The ordinal row carries every descriptor field as a slot; an
		// unset field reads as nil (SQL NULL) — present-nil, not key-absent.
		if datum["flower"] != nil {
			t.Errorf("FLOWER (unset) = %v, want nil (SQL NULL)", datum["flower"])
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
		if _, err := s.SaveRecord(&demopb.Order{OrderId: proto.Int64(18001), Price: proto.Int32(100)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&demopb.Order{OrderId: proto.Int64(18002), Price: proto.Int32(200)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(28001), ValString: proto.String("hello")}); err != nil {
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

		orderRecordType := s.GetMetaData().GetRecordType("Order")
		if orderRecordType == nil || orderRecordType.Descriptor == nil {
			return nil, fmt.Errorf("missing Order record descriptor")
		}
		orderRowType := PositionalTypeForRecordLayout(
			orderRecordType.Descriptor, s.GetMetaData().IsStoreRecordVersions())
		// Deliberately scan the shared multi-type stream while publishing the
		// exact row contract the enclosing Order TypeFilter guarantees. The raw
		// scan encounters TypedRecord rows too; they must be discarded before the
		// Order layout is attached, while surviving Order rows remain exact.
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(
			[]string{"Order", "TypedRecord"}, orderRowType, false,
		))
		typeFilter := mustExecutorConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan))

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
			d, _ := rowMapOK(r)
			if _, ok := d["order_id"]; !ok {
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
		&demopb.Order{OrderId: proto.Int64(18101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))

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

		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(18101) {
			t.Errorf("ORDER_ID = %v, want 18101", d["order_id"])
		}
		if d["price"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", d["price"])
		}
		// An unset field is a present-nil slot (SQL NULL), not key-absent.
		if d["quantity"] != nil {
			t.Errorf("QUANTITY (unset) = %v, want nil (SQL NULL)", d["quantity"])
		}
		if d["customer_id"] != nil {
			t.Errorf("CUSTOMER_ID (unset) = %v, want nil (SQL NULL)", d["customer_id"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_LoadByKeys_Resume pins the resumable key-list shape of
// LoadByKeys (Java RecordQueryLoadByKeysPlan.executePlan resumes via
// RecordCursor.fromList(keys, continuation)): reading one row, then
// re-executing with the emitted continuation, must yield ONLY the
// remaining keys' records. The eager buffered form dropped the incoming
// continuation while emitting resumable-looking list tokens, so a
// resume replayed every row from 0 — duplicates. Missing keys are
// skipped like Java's .filter(Objects::nonNull).
func TestIntegration_LoadByKeys_Resume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &demopb.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	}, &demopb.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	}, &demopb.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})

	// Key 99 has no record — skipped without consuming an output slot.
	keys := []tuple.Tuple{{int64(1)}, {int64(99)}, {int64(2)}, {int64(3)}}
	p := mustExecutorConstruct(plans.NewRecordQueryLoadByKeysPlanFromKeys(keys, integrationOrderType()))

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cursor, err := ExecutePlan(ctx, p, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		first, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !first.HasNext() {
			t.Fatal("expected a first row")
		}
		if got := first.GetValue().PrimaryKey; len(got) != 1 || got[0] != int64(1) {
			t.Fatalf("first row pk = %v, want [1]", got)
		}
		contBytes, err := first.GetContinuation().ToBytes()
		if err != nil {
			t.Fatalf("continuation: %v", err)
		}
		if len(contBytes) == 0 {
			t.Fatal("expected a resumable continuation after row 1")
		}

		resumed, err := ExecutePlan(ctx, p, s, EmptyEvaluationContext(), contBytes, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan(resume): %v", err)
		}
		defer resumed.Close()
		rest, err := CollectAll(ctx, resumed)
		if err != nil {
			t.Fatalf("CollectAll(resume): %v", err)
		}
		var pks []int64
		for _, r := range rest {
			pks = append(pks, r.PrimaryKey[0].(int64))
		}
		// Resume must yield ONLY keys after the token (99 skipped): 2, 3.
		// The replay-from-0 behavior yielded 1, 2, 3 again.
		if len(pks) != 2 || pks[0] != int64(2) || pks[1] != int64(3) {
			t.Fatalf("resumed rows = %v, want [2 3]", pks)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
