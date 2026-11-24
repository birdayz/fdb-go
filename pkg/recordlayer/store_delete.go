// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// DeleteRecord deletes a record by its primary key, following Java's deleteRecordAsync semantics.
// Returns true if a record was deleted, false if no record existed with that key.
// Handles both split and unsplit records via SplitHelper.
// Matches Java's FDBRecordStore.deleteRecordAsync(Tuple primaryKey)
func (store *FDBRecordStore) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	return store.deleteRecord(primaryKey, nil)
}

// deleteRecord is Java's deleteTypedRecord: typed, when not nil, is the typed
// store's serializer, through which the existing record is read.
func (store *FDBRecordStore) deleteRecord(primaryKey tuple.Tuple, typed *typedRecordSerializer) (bool, error) {
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return false, err
	}
	defer finishWrite()
	startTime := time.Now()
	defer store.context.Timer().RecordSince(EventDeleteRecord, startTime)
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Load existing record to get size info and record data (for counting)
	var oldsizeInfo sizeInfo
	loadTimer := recordLoadTimer{timer: store.context.Timer(), start: time.Now()}
	defer loadTimer.finish()
	load := store.startAuthoritativeRecordLoad(store.context.Transaction(), primaryKey)
	value, oldRecordVersion, err := load.finishAuthoritative(&oldsizeInfo)
	if err != nil {
		return false, fmt.Errorf("failed to load record for deletion %v: %w", primaryKey, err)
	}
	if value == nil {
		if err := load.finishRetiredVersionErrors(); err != nil {
			return false, fmt.Errorf("load old record version for deletion: %w", err)
		}
		return false, nil // Record not found
	}

	// Deserialize the old record first, always: Java's deleteTypedRecord loads it
	// through the serializer (loadTypedRecord, FDBRecordStore.java:1766), so a
	// record the store cannot read (encrypted under a key it lacks, or corrupt)
	// fails the delete, ahead of the lock check, and is never removed, whether or
	// not an index or the record count needs it.
	oldRecordType, oldMsg, oldWire, deserErr := store.deserializeAndDiscover(value)
	if deserErr != nil {
		return false, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
	}
	if typed != nil {
		if err := typed.checkDeserialized(primaryKey, oldRecordType); err != nil {
			return false, err
		}
	}

	oldRecordVersion, err = load.finishVersion(oldRecordVersion)
	if err != nil {
		return false, fmt.Errorf("load old record version for deletion: %w", err)
	}

	loadTimer.finish()

	// Then the lock, before any write (validateRecordUpdateAllowed inside the
	// load's continuation, :1770-1771).
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return false, err
	}

	if err := store.checkRecordWriteCollation(true, oldRecordType); err != nil {
		return false, err
	}

	physicalPresent, finishPhysical, err := load.beginPhysicalMutation(true, &oldsizeInfo)
	if err != nil {
		return false, err
	}
	err = func() error {
		defer finishPhysical()
		if physicalPresent {
			if _, err := deleteSplit(store.context.Transaction(), load.physical.records, primaryKey, splitEnabled, store.omitUnsplitRecordSuffix(), &oldsizeInfo); err != nil {
				return err
			}
		}
		load.physical.clearVersion()
		return nil
	}()
	if err != nil {
		return false, err
	}

	oldStoredRecord := &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: oldRecordType,
		Record:     oldMsg,
		wire:       oldWire,
		Version:    oldRecordVersion,
		Store:      store,
	}
	// Decrement record count
	if store.metaData.GetRecordCountKey() != nil && oldMsg != nil {
		if err := store.addRecordCount(oldStoredRecord, littleEndianInt64MinusOne); err != nil {
			return false, fmt.Errorf("failed to decrement record count: %w", err)
		}
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() && oldMsg != nil {
		if err := store.updateSecondaryIndexes(oldStoredRecord, nil, nil); err != nil {
			return false, err
		}
	}

	// Record instrumentation
	timer := store.context.Timer()
	timer.Increment(CountDeleteRecordKey)
	timer.IncrementBy(CountDeleteRecordKeyBytes, int64(oldsizeInfo.KeySize))

	if err := store.context.CheckTransactionSize(); err != nil {
		return true, err
	}

	return true, nil
}

// AddRecordReadConflict adds a read conflict range for the given primary key.
// This ensures that if another transaction modifies this record before this transaction commits,
// this transaction will fail with a conflict error.
//
// Java equivalent: FDBRecordStore.addRecordReadConflict(Tuple primaryKey)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:1222
func (store *FDBRecordStore) AddRecordReadConflict(primaryKey tuple.Tuple) error {
	recordRange := store.getRangeForRecord(primaryKey)
	return store.context.Transaction().AddReadConflictRange(recordRange)
}

// AddRecordWriteConflict adds a write conflict range for the given primary key.
// This ensures that if another transaction reads this record before this transaction commits,
// that transaction will fail with a conflict error.
//
// Java equivalent: FDBRecordStore.addRecordWriteConflict(Tuple primaryKey)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:1228
func (store *FDBRecordStore) AddRecordWriteConflict(primaryKey tuple.Tuple) error {
	recordRange := store.getRangeForRecord(primaryKey)
	return store.context.Transaction().AddWriteConflictRange(recordRange)
}

// getRangeForRecord calculates the key range that covers all possible record type variants
// for the given primary key. This matches Java's TupleRange.allOf(primaryKey).toRange(recordsSubspace())
//
// Java equivalent: TupleRange.allOf(primaryKey).toRange(recordsSubspace())
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/TupleRange.java:371-377, 449-456
//
// Java's TupleRange.allOf() creates a range with RANGE_INCLUSIVE endpoints:
//   - Begin: recordsSubspace.pack(primaryKey)
//   - End: recordsSubspace.pack(primaryKey) + 0xFF
//
// This is different from using Sub(), which would add an extra 0x00 byte to the begin key.
// The range covers all keys that start with the primary key tuple (e.g., {orderID, UNSPLIT_RECORD}).
func (store *FDBRecordStore) getRangeForRecord(primaryKey tuple.Tuple) fdb.ExactRange {
	recordsSubspace := store.recordsSubspace

	// Pack the primary key directly (Java's TupleRange.allOf approach)
	// This gives us recordsSubspace.pack(primaryKey)
	beginKey := recordsSubspace.Pack(primaryKey)

	// For the end key, append 0xFF to create the inclusive range
	// This matches Java's RANGE_INCLUSIVE endpoint handling
	endKey := append(recordsSubspace.Pack(primaryKey), 0xFF)

	return fdb.KeyRange{
		Begin: fdb.Key(beginKey),
		End:   fdb.Key(endKey),
	}
}

// DeleteIndexEntries clears all entries for the given index.
// Matches Java's StandardIndexMaintainer.deleteWhere() with no predicate.
func (store *FDBRecordStore) DeleteIndexEntries(index *Index) {
	if index == nil {
		return
	}
	indexSub := store.indexSubspace(index)
	store.context.Transaction().ClearRange(indexSub)
}

// DeleteIndexEntriesInRange clears index entries matching the given tuple prefix.
// For example, passing tuple.Tuple{"alice"} clears all entries where the first
// indexed value is "alice".
func (store *FDBRecordStore) DeleteIndexEntriesInRange(index *Index, prefix tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	// [pack, pack+ff) is the prefix's tuple range plus the key at pack itself
	// (a grouped aggregate); strinc(pack) would also clear "alice\x00…".
	prefixKey := store.indexSubspace(index).Pack(prefix)
	store.context.Transaction().ClearRange(fdb.KeyRange{Begin: prefixKey, End: append(prefixKey, 0xff)})
	return nil
}

// DeleteAllRecords deletes all records from the store.
// Clears all data subspaces except StoreInfoKey (0) and IndexStateSpaceKey (5).
// Matches Java's FDBRecordStore.deleteAllRecords() which clears everything
// from records through index state begin, then from index state end through store end.
func (store *FDBRecordStore) DeleteAllRecords() error {
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return err
	}
	defer finishWrite()
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return err
	}

	resetCount := store.metaData.GetRecordCountKey() != nil && !store.isRecordCountDisabled()
	flatSubspace := subspace.FromBytes(store.subspace.Bytes())
	finishPhysical, err := store.beginPhysicalBulk()
	if err != nil {
		return err
	}
	defer finishPhysical()
	tx := store.context.Transaction()

	// Clear all subspaces except StoreInfoKey (0) and IndexStateSpaceKey (5).
	// Java does two range clears: [records, indexState) and (indexState, storeEnd).
	// We clear individual subspaces for clarity.
	//
	// The enumeration is the risk that trade buys: Java's second range runs to
	// the END of the store, so a keyspace added to FDBRecordStoreKeyspace is
	// cleared there automatically and has to be added HERE by hand. Every new
	// prefix must appear below, or its data survives a delete-all — which for
	// the sliding window meant the next save reading a full window over an
	// emptied graph, and evicting against a boundary naming a record that is
	// gone.
	// Use PrefixRange to include the exact prefix key — ungrouped aggregate
	// data (e.g. record counts) is stored at the subspace prefix itself,
	// which subspace.FDBRangeKeys() excludes.
	for _, key := range []int{
		RecordKey,                    // 1 - records
		IndexKey,                     // 2 - index data
		IndexSecondarySpaceKey,       // 3 - secondary index data
		RecordCountKey,               // 4 - record counts
		IndexRangeSpaceKey,           // 6 - index ranges
		IndexUniquenessViolationsKey, // 7 - uniqueness violations
		RecordVersionKey,             // 8 - record versions
		IndexBuildSpaceKey,           // 9 - index build state
		IndexSlidingWindowSpaceKey,   // 10 - sliding-window bookkeeping
	} {
		sub := flatSubspace.Sub(key)
		pr, err := fdb.PrefixRange(sub.Bytes())
		if err != nil {
			return fmt.Errorf("delete all records prefix range for key %d: %w", key, err)
		}
		tx.ClearRange(pr)
	}

	// Remove pending version mutations and local version cache entries for
	// the cleared subspaces. Without this, orphaned SET_VERSIONSTAMPED_VALUE
	// mutations (for record versions) and SET_VERSIONSTAMPED_KEY mutations
	// (for VERSION index entries) would be flushed at commit, writing version
	// data for records that no longer exist.
	// Matches Java's context.clear(Range) → removeVersionMutationRange() + removeLocalVersionRange().
	for _, key := range []int{
		RecordKey,              // inline record versions live under records subspace
		IndexKey,               // VERSION index entries live under index subspace
		IndexSecondarySpaceKey, // secondary index data
		RecordVersionKey,       // explicit record version subspace
		IndexBuildSpaceKey,     // pending index queue entries
	} {
		sub := flatSubspace.Sub(key)
		pr, err := fdb.PrefixRange(sub.Bytes())
		if err != nil {
			return fmt.Errorf("DeleteAllRecords: PrefixRange for subspace %d: %w", key, err)
		}
		begin, end := pr.FDBRangeKeys()
		store.context.RemoveVersionMutationsInRange(begin.FDBKey(), end.FDBKey())
		store.context.RemoveLocalVersionsInRange(begin.FDBKey(), end.FDBKey())
	}

	// Reset record count to 0. ClearRange alone doesn't override pending atomic
	// Add mutations within the same transaction, so we also explicitly Set
	// the count key to 0 to ensure reads in the same tx see the reset.
	// Skip when count state is DISABLED (no count data should exist).
	if resetCount {
		countSubspace := flatSubspace.Sub(RecordCountKey)
		fdbKey := countSubspace.Pack(tuple.Tuple{})
		tx.Set(fdbKey, encodeRecordCount(0))
	}

	return nil
}
