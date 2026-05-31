package executor

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
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
			predicates.ComparisonDistanceRankLessThanOrEq,
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

// TestIntegration_VectorIndexScan_RankLessThan proves codex Finding 2's <-vs-<=
// semantics end-to-end: ROW_NUMBER() < 3 returns the top 2 (Java's
// getAdjustedLimit: k-1), NOT 3. Same data + query as the <= K test, only the
// rank operator differs.
func TestIntegration_VectorIndexScan_RankLessThan(t *testing.T) {
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
		// ROW_NUMBER() < 3 → top 2 (k-1), not 3.
		plan := plans.NewRecordQueryVectorIndexPlan(
			"vec_pq", nil,
			values.LiteralValue([]float64{12, 12}),
			values.LiteralValue(3),
			predicates.ComparisonDistanceRankLessThan,
			nil, nil, []string{"Order"}, nil,
		)
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
			t.Fatalf("ROW_NUMBER() < 3 returned %d rows, want 2 (k-1 per Java getAdjustedLimit)", len(results))
		}
		ids := []int64{
			results[0].Datum.(map[string]any)["ORDER_ID"].(int64),
			results[1].Datum.(map[string]any)["ORDER_ID"].(int64),
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if ids[0] != 1 || ids[1] != 2 {
			t.Errorf("< 3 result ids = %v, want [1 2] (the 2 nearest)", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// TestIntegration_VectorIndexScan_ContinuationPK pins codex Finding 3: a resumed
// vector scan page must carry the correct primary key. Before the fix,
// parseVectorScanContinuation rebuilt entries without Index/primaryKey, so
// IndexEntry.PrimaryKey() returned an empty tuple on resume — loading the wrong
// record / skipping rows. Read entry 1, resume from its continuation, and assert
// the resumed entry's PrimaryKey() is non-empty and equals the 2nd entry of a
// fresh full scan.
func TestIntegration_VectorIndexScan_ContinuationPK(t *testing.T) {
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
		idx := s.GetMetaData().GetIndex("vec_pq")
		scanRange := recordlayer.VectorDistanceScanRangeWithPrefix([]float64{12, 12}, 3, 200, nil)
		props := recordlayer.ScanProperties{
			ExecuteProperties:   recordlayer.DefaultExecuteProperties(),
			CursorStreamingMode: recordlayer.StreamingModeIterator,
		}

		// Fresh full scan → expected ordered PKs.
		var freshPKs [][]byte
		c0 := s.ScanIndexByType(idx, recordlayer.IndexScanByDistance, scanRange, nil, props)
		for {
			r, e := c0.OnNext(ctx)
			if e != nil {
				t.Fatalf("fresh scan: %v", e)
			}
			if !r.HasNext() {
				break
			}
			pk := r.GetValue().PrimaryKey()
			if len(pk) == 0 {
				t.Fatal("fresh entry has empty PrimaryKey()")
			}
			freshPKs = append(freshPKs, pk.Pack())
		}
		c0.Close()
		if len(freshPKs) < 2 {
			t.Fatalf("fresh scan returned %d entries, want >= 2", len(freshPKs))
		}

		// Read entry 1, capture its continuation.
		c1 := s.ScanIndexByType(idx, recordlayer.IndexScanByDistance, scanRange, nil, props)
		r1, e := c1.OnNext(ctx)
		if e != nil || !r1.HasNext() {
			t.Fatalf("first entry: err=%v hasNext=%v", e, r1.HasNext())
		}
		cont, e := r1.GetContinuation().ToBytes()
		if e != nil {
			t.Fatalf("continuation ToBytes: %v", e)
		}
		c1.Close()

		// Resume from the continuation → entry 2, with a CORRECT primary key.
		c2 := s.ScanIndexByType(idx, recordlayer.IndexScanByDistance, scanRange, cont, props)
		r2, e := c2.OnNext(ctx)
		if e != nil || !r2.HasNext() {
			t.Fatalf("resumed entry: err=%v hasNext=%v", e, r2.HasNext())
		}
		defer c2.Close()
		resumedPK := r2.GetValue().PrimaryKey()
		if len(resumedPK) == 0 {
			t.Fatal("resumed entry has empty PrimaryKey() — Finding 3 regression")
		}
		if !bytes.Equal(resumedPK.Pack(), freshPKs[1]) {
			t.Errorf("resumed PK = %x, want %x (2nd entry of fresh scan)", resumedPK.Pack(), freshPKs[1])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}
