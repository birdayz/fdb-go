package chaos

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildOnlineIndexerValueMetadata creates metadata WITH a VALUE index (price).
// Used for OnlineIndexer tests: create store with this metadata, populate records,
// then rebuild the index with fault injection.
func buildOnlineIndexerValueMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("oi_price_idx", recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build online indexer value metadata: " + err.Error())
	}
	return md
}

// buildOnlineIndexerCountMetadata creates metadata with a COUNT index (ungrouped).
func buildOnlineIndexerCountMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewCountIndex("oi_count_idx",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build online indexer count metadata: " + err.Error())
	}
	return md
}

// populateRecords inserts numRecords Order records into the store using a clean DB.
// Returns the records' PKs for verification.
func populateRecords(t testing.TB, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, sub subspace.Subspace, numRecords int) {
	t.Helper()
	ctx := context.Background()

	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		for i := 1; i <= numRecords; i++ {
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(int64(i)),
				Price:   proto.Int32(int32(i * 10)),
			})
			if err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("populateRecords: %v", err)
	}
}

// verifyValueIndexCompleteness scans the VALUE index and checks that all expected
// records are indexed. Returns the number of index entries.
func verifyValueIndexCompleteness(t testing.TB, store *recordlayer.FDBRecordStore, md *recordlayer.RecordMetaData, indexName string, expectedRecords int) {
	t.Helper()
	ctx := context.Background()

	idx := md.GetIndex(indexName)
	if idx == nil {
		t.Fatalf("verifyValueIndexCompleteness: index %q not found", indexName)
	}

	cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()

	var count int
	seenPKs := make(map[int64]bool)
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("verifyValueIndexCompleteness: scan error: %v", err)
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		pk := entry.PrimaryKey()
		if len(pk) > 0 {
			if pkVal, ok := pk[0].(int64); ok {
				seenPKs[pkVal] = true
			}
		}
		count++
	}

	if count != expectedRecords {
		t.Errorf("verifyValueIndexCompleteness: expected %d index entries, got %d", expectedRecords, count)
	}

	// Verify all expected PKs are present.
	for i := 1; i <= expectedRecords; i++ {
		if !seenPKs[int64(i)] {
			t.Errorf("verifyValueIndexCompleteness: PK %d missing from index", i)
		}
	}
}

// verifyCountIndexValue checks that a COUNT index has the expected total.
func verifyCountIndexValue(t testing.TB, store *recordlayer.FDBRecordStore, md *recordlayer.RecordMetaData, indexName string, expectedTotal int64) {
	t.Helper()
	ctx := context.Background()

	idx := md.GetIndex(indexName)
	if idx == nil {
		t.Fatalf("verifyCountIndexValue: index %q not found", indexName)
	}

	cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()

	var totalCount int64
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("verifyCountIndexValue: scan error: %v", err)
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		if len(entry.Value) > 0 {
			if v, ok := entry.Value[0].(int64); ok {
				totalCount += v
			}
		}
	}

	if totalCount != expectedTotal {
		t.Errorf("verifyCountIndexValue: expected total count %d, got %d", expectedTotal, totalCount)
	}
}

// TestOnlineIndexerCommitUnknown builds a VALUE index with commit-unknown faults
// injected into the chunk transactions. The RangeSet's InsertRange(requireEmpty=true)
// detects already-processed ranges, making retries safe for idempotent indexes.
func TestOnlineIndexerCommitUnknown(t *testing.T) {
	t.Parallel()

	md := buildOnlineIndexerValueMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numRecords = 20

	// Step 1: Populate records using clean DB.
	populateRecords(t, cleanDB, md, sub, numRecords)

	// Step 2: Create ChaosTransactor with 10% commit-unknown rate and build with it.
	chaosT := NewChaosTransactor(testRealDB, &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.10,
	}}, 42)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	idx := md.GetIndex("oi_price_idx")
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(chaosDB).
		SetMetaData(md).
		SetIndex(idx).
		SetSubspace(sub).
		SetLimit(5). // Small chunks to maximize the number of transactions (and faults).
		Build()
	if err != nil {
		t.Fatalf("build online indexer: %v", err)
	}

	total, err := indexer.BuildIndex(ctx)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	// Note: total may be < numRecords under faults because the ChaosTransactor
	// commits the first transaction (which indexes records), then retries in a
	// second transaction where the RangeSet shows those ranges as already done.
	// The returned count only reflects the "winning" transaction's tally.
	// The real correctness check is verifyValueIndexCompleteness below.

	// Step 3: Verify index completeness using clean DB.
	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyValueIndexCompleteness(t, store, md, "oi_price_idx", numRecords)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify after build: %v", err)
	}

	t.Logf("OnlineIndexer built %d records (reported) with %d faults injected", total, len(chaosT.Log))
}

// TestOnlineIndexerCommitUnknownSmallChunks uses very small chunks (limit=3) to
// maximize the number of transactions and fault injection opportunities.
func TestOnlineIndexerCommitUnknownSmallChunks(t *testing.T) {
	t.Parallel()

	md := buildOnlineIndexerValueMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numRecords = 15

	populateRecords(t, cleanDB, md, sub, numRecords)

	chaosT := NewChaosTransactor(testRealDB, &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.15,
	}}, 99)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	idx := md.GetIndex("oi_price_idx")
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(chaosDB).
		SetMetaData(md).
		SetIndex(idx).
		SetSubspace(sub).
		SetLimit(3). // Very small chunks.
		Build()
	if err != nil {
		t.Fatalf("build online indexer: %v", err)
	}

	total, err := indexer.BuildIndex(ctx)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	// total may be < numRecords under faults (see TestOnlineIndexerCommitUnknown comment).
	_ = total

	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyValueIndexCompleteness(t, store, md, "oi_price_idx", numRecords)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify after build: %v", err)
	}

	t.Logf("OnlineIndexer (limit=3) built %d records with %d faults injected", total, len(chaosT.Log))
}

// TestOnlineIndexerCommitUnknownCountIndex builds a COUNT index under faults.
// COUNT indexes are idempotent under OnlineIndexer because removeCommonGroupingKeys
// skips unchanged keys on retry.
func TestOnlineIndexerCommitUnknownCountIndex(t *testing.T) {
	t.Parallel()

	md := buildOnlineIndexerCountMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numRecords = 20

	populateRecords(t, cleanDB, md, sub, numRecords)

	chaosT := NewChaosTransactor(testRealDB, &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.10,
	}}, 123)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	idx := md.GetIndex("oi_count_idx")
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(chaosDB).
		SetMetaData(md).
		SetIndex(idx).
		SetSubspace(sub).
		SetLimit(5).
		Build()
	if err != nil {
		t.Fatalf("build online indexer: %v", err)
	}

	total, err := indexer.BuildIndex(ctx)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	// total may be < numRecords under faults (see TestOnlineIndexerCommitUnknown comment).
	_ = total

	// Verify: the total count across all groups should equal numRecords.
	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyCountIndexValue(t, store, md, "oi_count_idx", int64(numRecords))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify count index after build: %v", err)
	}

	t.Logf("OnlineIndexer COUNT built %d records with %d faults injected", total, len(chaosT.Log))
}

// TestOnlineIndexerHeavyFaults uses a 20% fault rate with a larger dataset.
// This maximizes the chance of hitting edge cases in the RangeSet and
// re-scanning logic.
func TestOnlineIndexerHeavyFaults(t *testing.T) {
	t.Parallel()

	md := buildOnlineIndexerValueMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numRecords = 50

	populateRecords(t, cleanDB, md, sub, numRecords)

	chaosT := NewChaosTransactor(testRealDB, &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown: 0.20,
	}}, 777)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	idx := md.GetIndex("oi_price_idx")
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(chaosDB).
		SetMetaData(md).
		SetIndex(idx).
		SetSubspace(sub).
		SetLimit(7). // Odd chunk size to avoid alignment with record PKs.
		Build()
	if err != nil {
		t.Fatalf("build online indexer: %v", err)
	}

	total, err := indexer.BuildIndex(ctx)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	// total may be < numRecords under faults (see TestOnlineIndexerCommitUnknown comment).
	_ = total

	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyValueIndexCompleteness(t, store, md, "oi_price_idx", numRecords)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify after heavy-fault build: %v", err)
	}

	t.Logf("OnlineIndexer (heavy faults) built %d records with %d faults injected", total, len(chaosT.Log))
}

// TestOnlineIndexerAllFaults combines all fault types at moderate rates.
func TestOnlineIndexerAllFaults(t *testing.T) {
	t.Parallel()

	md := buildOnlineIndexerValueMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	ctx := context.Background()

	const numRecords = 30

	populateRecords(t, cleanDB, md, sub, numRecords)

	chaosT := NewChaosTransactor(testRealDB, &FaultConfig{Rates: map[FaultType]float64{
		FaultCommitUnknown:     0.05,
		FaultConflict:          0.05,
		FaultTransactionTooOld: 0.03,
	}}, 456)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	idx := md.GetIndex("oi_price_idx")
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(chaosDB).
		SetMetaData(md).
		SetIndex(idx).
		SetSubspace(sub).
		SetLimit(5).
		Build()
	if err != nil {
		t.Fatalf("build online indexer: %v", err)
	}

	total, err := indexer.BuildIndex(ctx)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	// total may be < numRecords under faults (see TestOnlineIndexerCommitUnknown comment).
	_ = total

	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyValueIndexCompleteness(t, store, md, "oi_price_idx", numRecords)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify after all-faults build: %v", err)
	}

	t.Logf("OnlineIndexer (all faults) built %d records with %d faults injected", total, len(chaosT.Log))
}
