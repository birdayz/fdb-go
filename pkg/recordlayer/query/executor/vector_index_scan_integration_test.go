package executor

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// setupVectorStore builds a store whose Order type has a 2-d VECTOR (HNSW)
// index over (price, quantity).
func setupVectorStore(t *testing.T) *recordlayer.FDBRecordStore {
	t.Helper()
	ctx := context.Background()
	ks := testSubspace(t)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewVectorIndex(
		"vec_pq", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2))
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

// TestIntegration_VectorIndexScan_KNN proves the RecordQueryVectorIndexPlan
// executes a BY_DISTANCE K-NN scan end-to-end against real FDB: it dispatches
// to the HNSW maintainer's ScanByDistance and returns the k nearest records,
// ordered by distance. This is the standalone execution proof for 9.3c — the
// plan is constructed directly (the SQL→plan match wiring is 9.3a/b).
func TestIntegration_VectorIndexScan_KNN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupVectorStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30), Quantity: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		// Query vector (12,12): nearest two are (10,10)=id1 then (20,20)=id2.
		plan := plans.NewRecordQueryVectorIndexPlan(
			"vec_pq",
			nil, // unpartitioned
			values.LiteralValue([]float64{12, 12}),
			values.LiteralValue(2),
			nil, nil,
			[]string{"Order"},
			nil,
		)

		// EXPLAIN-pin: the plan must be a BY_DISTANCE vector scan, not a
		// generic value index scan.
		if exp := plan.Explain(); !strings.Contains(exp, "VectorIndexScan(vec_pq, BY_DISTANCE") {
			t.Errorf("explain = %q, want a VectorIndexScan BY_DISTANCE plan", exp)
		}

		cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("vector scan returned %d results, want 2 (top-2 KNN)", len(results))
		}

		gotIDs := make([]int64, len(results))
		for i, r := range results {
			row, ok := r.Datum.(map[string]any)
			if !ok {
				t.Fatalf("result %d datum is %T, want map[string]any", i, r.Datum)
			}
			gotIDs[i] = row["ORDER_ID"].(int64)
		}
		sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
		if gotIDs[0] != 1 || gotIDs[1] != 2 {
			t.Errorf("KNN result ids = %v, want [1 2] (nearest to (12,12))", gotIDs)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}
