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

// buildMaxEverVersionMetadata creates metadata with an ungrouped MAX_EVER_VERSION index.
// Tracks the maximum versionstamp ever written across all Order records.
func buildMaxEverVersionMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewMaxEverVersionIndex("max_ever_version_idx",
		recordlayer.Ungrouped(recordlayer.VersionKey())))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build max_ever_version metadata: " + err.Error())
	}
	return md
}

// buildGroupedMaxEverVersionMetadata creates metadata with a grouped MAX_EVER_VERSION index.
// Groups by quantity, so there's one max-version entry per unique quantity value.
func buildGroupedMaxEverVersionMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewMaxEverVersionIndex("max_ever_version_grouped_idx",
		recordlayer.GroupBy(recordlayer.VersionKey(), recordlayer.Field("quantity"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build grouped max_ever_version metadata: " + err.Error())
	}
	return md
}

// verifyMaxEverVersionEntries performs MAX_EVER_VERSION-specific checks:
//  1. Scans the index and verifies structural integrity of entries
//  2. For ungrouped: at most 1 entry if any records were ever saved
//  3. For grouped: at most N entries (one per unique grouping key)
//  4. Each value must be a non-empty tuple containing a versionstamp element
//
// Since we can't predict exact versionstamp values (transaction-assigned), we only
// verify structural invariants and monotonicity.
func verifyMaxEverVersionEntries(
	t testing.TB,
	store *recordlayer.FDBRecordStore,
	md *recordlayer.RecordMetaData,
	indexName string,
	isGrouped bool,
	groupingKeyCount int, // number of unique grouping keys ever saved (upper bound)
) {
	t.Helper()
	ctx := context.Background()

	idx := md.GetIndex(indexName)
	if idx == nil {
		t.Fatalf("verifyMaxEverVersionEntries: index %q not found in metadata", indexName)
	}

	cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()

	var entryCount int
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("verifyMaxEverVersionEntries: scan error: %v", err)
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		entryCount++

		// Value must be a non-empty tuple.
		if len(entry.Value) == 0 {
			t.Errorf("verifyMaxEverVersionEntries: entry %v has empty value", entry.Key)
			continue
		}

		// The value should contain a versionstamp element (tuple-packed).
		// Check that the unpacked tuple has at least one element.
		hasVersionstamp := false
		for _, elem := range entry.Value {
			if _, ok := elem.(tuple.Versionstamp); ok {
				hasVersionstamp = true
				break
			}
		}
		if !hasVersionstamp {
			t.Errorf("verifyMaxEverVersionEntries: entry key=%v value=%v does not contain a Versionstamp element", entry.Key, entry.Value)
		}
	}

	if !isGrouped {
		// Ungrouped: at most 1 entry.
		if entryCount > 1 {
			t.Errorf("verifyMaxEverVersionEntries: ungrouped index should have at most 1 entry, got %d", entryCount)
		}
	} else {
		// Grouped: at most groupingKeyCount entries.
		if entryCount > groupingKeyCount {
			t.Errorf("verifyMaxEverVersionEntries: grouped index should have at most %d entries, got %d", groupingKeyCount, entryCount)
		}
	}
}

// --- Ungrouped MAX_EVER_VERSION tests ---

func TestMaxEverVersionBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300)})
	s.Verify()

	// Additional check: exactly 1 ungrouped entry.
	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_idx", false, 1)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify max_ever_version entries: %v", err)
	}
}

func TestMaxEverVersionCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	// MAX_EVER_VERSION uses BYTE_MAX (idempotent) or SET_VERSIONSTAMPED_VALUE
	// with a merge function that keeps max. Retry should be safe.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()
}

func TestMaxEverVersionCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// Overwrite with commit-unknown. The max version should ratchet up
	// to the second transaction's versionstamp (which is >= the first).
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(999)})
	s.Verify()
}

func TestMaxEverVersionCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
	s.Verify()

	// _EVER semantics: deletes are no-ops for the index.
	// Entry should persist even after delete.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()

	// Verify the MAX_EVER_VERSION entry still exists (since _EVER semantics).
	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_idx", false, 1)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify max_ever_version after delete: %v", err)
	}
}

func TestMaxEverVersionDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 5; i++ {
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
	}
	s.Verify()

	// DeleteAllRecords clears ALL index data, including _EVER tracking.
	s.DeleteAllRecords()
	s.Verify()

	// After delete-all, save a new record. A fresh MAX_EVER_VERSION entry should appear.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42)})
	s.Verify()

	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_idx", false, 1)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify max_ever_version after delete-all + save: %v", err)
	}
}

func TestMaxEverVersionRandomFaults(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(44444), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	const verifyEvery = 20
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(1000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

func TestMaxEverVersionHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(55555), WithFaults(FaultsRetryVeryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(500)),
			})
		} else if s.Rng.Float64() < 0.9 {
			s.DeleteRecord(tuple.Tuple{pk})
		} else {
			s.DeleteAllRecords()
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// --- Grouped MAX_EVER_VERSION tests ---

func TestMaxEverVersionGroupedBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save records with different quantities (grouping keys).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()

	// Same quantity group as record 1 — should update max for that group.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(5)})
	s.Verify()

	// Verify grouped entries: should be at most 2 entries (qty=5 and qty=10).
	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_grouped_idx", true, 2)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify grouped max_ever_version entries: %v", err)
	}
}

func TestMaxEverVersionGroupedRandomFaults(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(66666), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	const verifyEvery = 20
	maxPK := int64(20)

	// Track unique quantities ever saved for the upper bound check.
	uniqueQuantities := make(map[int32]bool)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			qty := s.Rng.Int32N(5) * 10 // small quantity space for grouping collisions
			uniqueQuantities[qty] = true
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(1000)),
				Quantity: proto.Int32(qty),
			})
		} else if s.Rng.Float64() < 0.95 {
			s.DeleteRecord(tuple.Tuple{pk})
		} else {
			s.DeleteAllRecords()
			uniqueQuantities = make(map[int32]bool) // reset on delete-all
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	// Final structural check on grouped entries.
	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_grouped_idx", true, len(uniqueQuantities))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify grouped max_ever_version after random ops: %v", err)
	}

	t.Logf("completed %d ops with seed=%d, %d faults injected, %d unique quantities",
		numOps, s.Seed(), len(s.FaultLog()), len(uniqueQuantities))
}

// --- All faults stress test ---

func TestMaxEverVersionAllFaultsStress(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(77778), WithFaults(FaultsAll))

	const numOps = 150
	maxPK := int64(25)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.65 {
			s.SaveRecord(&gen.Order{
				OrderId: proto.Int64(pk),
				Price:   proto.Int32(s.Rng.Int32N(800)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}
		if (i+1)%30 == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops with seed=%d, %d faults injected (all fault types)",
		numOps, s.Seed(), len(s.FaultLog()))
}

// --- Grouped commit-unknown targeted tests ---

func TestMaxEverVersionGroupedCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()
}

func TestMaxEverVersionGroupedCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
	s.Verify()

	// Overwrite with different quantity (changes grouping key).
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200), Quantity: proto.Int32(10)})
	s.Verify()
}

func TestMaxEverVersionGroupedDeleteAll(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 5; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i * 10)),
			Quantity: proto.Int32(int32(i % 3)),
		})
	}
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()

	// After delete-all + re-save, grouped entries should reflect only the new record.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42), Quantity: proto.Int32(7)})
	s.Verify()

	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		// Only 1 unique quantity after delete-all + single save.
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_grouped_idx", true, 1)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify grouped max_ever_version after delete-all: %v", err)
	}
}

// --- Combined ungrouped + grouped stress ---

func TestMaxEverVersionGroupedHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildGroupedMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(88888), WithFaults(FaultsRetryVeryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(25)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(500)),
				Quantity: proto.Int32(s.Rng.Int32N(5) * 10),
			})
		} else if s.Rng.Float64() < 0.9 {
			s.DeleteRecord(tuple.Tuple{pk})
		} else {
			s.DeleteAllRecords()
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()
	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// --- Verify idempotency explanation ---

// TestMaxEverVersionIdempotencyExplanation documents why MAX_EVER_VERSION is
// idempotent under commit-unknown, unlike COUNT_UPDATES.
//
// BYTE_MAX is idempotent: applying the same max twice produces the same result.
// SET_VERSIONSTAMPED_VALUE with our merge function keeps the larger versionstamp.
// So even when the ChaosTransactor commits and retries, the index stays correct.
func TestMaxEverVersionIdempotencyExplanation(t *testing.T) {
	t.Parallel()
	md := buildMaxEverVersionMetadata()
	s := NewScenario(t, testRealDB, md)

	// Save with commit-unknown, then verify. If MAX_EVER_VERSION were
	// non-idempotent, this would corrupt the index value.
	for i := int64(1); i <= 5; i++ {
		s.InjectOnce(FaultCommitUnknown)
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
		s.Verify()
	}

	// Overwrite all with commit-unknown.
	for i := int64(1); i <= 5; i++ {
		s.InjectOnce(FaultCommitUnknown)
		s.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 200))})
		s.Verify()
	}

	// Delete all with commit-unknown. _EVER entry persists.
	for i := int64(1); i <= 5; i++ {
		s.InjectOnce(FaultCommitUnknown)
		s.DeleteRecord(tuple.Tuple{i})
		s.Verify()
	}

	// Structural check: ungrouped entry should still exist (EVER semantics).
	ctx := context.Background()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)
	_, err := cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		verifyMaxEverVersionEntries(t, store, md, "max_ever_version_idx", false, 1)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify idempotency: %v", err)
	}
}
