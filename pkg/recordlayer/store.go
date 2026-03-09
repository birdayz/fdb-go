package recordlayer

import (
	"context"
	"fmt"
	"errors"
	"time"
	"bytes"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	
	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Record Layer format versions - should match Java FormatVersion
const (
	FormatVersionCurrent = 9 // Current format version for compatibility
)

// StoreIsLockedForRecordUpdatesError is thrown when attempting to modify records in a locked store
// Matches Java's com.apple.foundationdb.record.StoreIsLockedForRecordUpdates
type StoreIsLockedForRecordUpdatesError struct {
	Reason    string
	Timestamp int64
}

func (e *StoreIsLockedForRecordUpdatesError) Error() string {
	return fmt.Sprintf("Record Store is locked for record updates: %s (timestamp: %d)", e.Reason, e.Timestamp)
}

// ErrRecordStoreStateNotLoaded indicates that the record store state needs to be loaded before operations
var ErrRecordStoreStateNotLoaded = errors.New("record store state not loaded")

// Store creation/existence errors
var (
	ErrRecordStoreAlreadyExists = errors.New("record store already exists")
	ErrRecordStoreDoesNotExist  = errors.New("record store does not exist")
	ErrRecordStoreNoInfoButNotEmpty = errors.New("record store has no info but is not empty")
)

// FDBRecordStore provides record storage operations within a transaction context.
// This is the main struct for storing and retrieving records.
type FDBRecordStore struct {
	context            *FDBRecordContext
	metaData           *RecordMetaData
	subspace           subspace.Subspace
	storeHeader        *gen.DataStoreInfo    // Cached store header, loaded on Open/Create
	indexStates        map[string]IndexState  // Cached index states, loaded on Open/Create
	indexRebuildPolicy IndexRebuildPolicy     // Policy for rebuilding indexes on metadata version change
}

// validateRecordUpdateAllowed checks if the store allows record mutations.
// Returns StoreIsLockedForRecordUpdatesError if the store header has
// StoreLockState set to FORBID_RECORD_UPDATE.
// Matches Java's FDBRecordStore.validateRecordUpdateAllowed().
func (store *FDBRecordStore) validateRecordUpdateAllowed() error {
	if store.storeHeader == nil {
		return nil
	}
	lockState := store.storeHeader.GetStoreLockState()
	if lockState == nil {
		return nil
	}
	if lockState.GetLockState() == gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE {
		return &StoreIsLockedForRecordUpdatesError{
			Reason:    lockState.GetReason(),
			Timestamp: lockState.GetTimestamp(),
		}
	}
	return nil
}

// LoadRecord loads a record by its primary key.
// Handles both unsplit (suffix 0) and split (suffixes 1, 2, ...) records
// via SplitHelper, matching Java's FDBRecordStore.loadRecordAsync().
func (store *FDBRecordStore) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[proto.Message], error) {
	recordsSubspace := store.subspace.Sub(RecordKey)

	var sizeInfo SizeInfo
	value, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		store.metaData.IsSplitLongRecords(),
		&sizeInfo,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load record %v: %w", primaryKey, err)
	}
	if value == nil {
		return nil, nil // Record not found
	}

	// Discover which record type is stored by inspecting the UnionDescriptor
	recordType, protoMessage, err := store.deserializeAndDiscover(value)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize record: %w", err)
	}

	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     protoMessage,
		KeyCount:   sizeInfo.KeyCount,
		ValueSize:  sizeInfo.ValueSize,
		KeySize:    sizeInfo.KeySize,
		Split:      sizeInfo.IsSplit,
	}, nil
}

// SaveRecord saves a protobuf record to the store.
// Equivalent to SaveRecordWithOptions(record, RecordExistenceCheckNone).
// Java equivalent: FDBRecordStore.saveRecord(Message rec)
func (store *FDBRecordStore) SaveRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckNone)
}

// DeleteRecord deletes a record by its primary key, following Java's deleteRecordAsync semantics.
// Returns true if a record was deleted, false if no record existed with that key.
// Handles both split and unsplit records via SplitHelper.
// Matches Java's FDBRecordStore.deleteRecordAsync(Tuple primaryKey)
func (store *FDBRecordStore) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return false, err
	}

	recordsSubspace := store.subspace.Sub(RecordKey)
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Load existing record to get size info and record data (for counting)
	var oldSizeInfo SizeInfo
	value, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		&oldSizeInfo,
	)
	if err != nil {
		return false, fmt.Errorf("failed to load record for deletion %v: %w", primaryKey, err)
	}
	if value == nil {
		return false, nil // Record not found
	}

	// Check for inline version
	if store.metaData.IsStoreRecordVersions() {
		oldSizeInfo.VersionedInline = true
	}

	// Delete all KV pairs for this record
	deleteSplit(store.context.Transaction(), recordsSubspace, primaryKey, splitEnabled, &oldSizeInfo)

	// Clean up version mutations (incomplete versionstamp + local version cache).
	// deleteSplit clears the FDB key, but we also need to dequeue any pending
	// incomplete version mutation from the context. Matches Java's deleteTypedRecord
	// which calls context.removeVersionMutation().
	if store.metaData.IsStoreRecordVersions() {
		versionKey := store.versionKey(primaryKey)
		store.context.RemoveLocalVersion(versionKey)
		store.context.RemoveVersionMutation(versionKey)
	}

	// Deserialize old record if needed for counting or index updates
	needDeserialize := store.metaData.GetRecordCountKey() != nil || store.metaData.HasIndexes()
	var oldRecordType *RecordType
	var oldMsg proto.Message
	if needDeserialize {
		var deserErr error
		oldRecordType, oldMsg, deserErr = store.deserializeAndDiscover(value)
		if deserErr != nil {
			return false, fmt.Errorf("failed to deserialize record for deletion: %w", deserErr)
		}
	}

	// Decrement record count
	if store.metaData.GetRecordCountKey() != nil && oldMsg != nil {
		store.addRecordCount(oldMsg, littleEndianInt64MinusOne)
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() && oldMsg != nil {
		oldStoredRecord := &FDBStoredRecord[proto.Message]{
			PrimaryKey: primaryKey,
			RecordType: oldRecordType,
			Record:     oldMsg,
		}
		if err := store.updateSecondaryIndexes(oldStoredRecord, nil); err != nil {
			return false, err
		}
	}

	return true, nil
}

// RecordExists checks if a record exists with the given primary key.
// Handles both split and unsplit records via SplitHelper.
//
// Java equivalent: FDBRecordStore.recordExistsAsync(Tuple primaryKey, IsolationLevel isolationLevel)
func (store *FDBRecordStore) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	recordsSubspace := store.subspace.Sub(RecordKey)

	var tx fdb.ReadTransaction
	if isolationLevel.IsSnapshot() {
		tx = store.context.Transaction().Snapshot()
	} else {
		tx = store.context.Transaction()
	}

	return recordExistsWithSplit(tx, recordsSubspace, primaryKey, store.metaData.IsSplitLongRecords())
}

// SaveRecordWithOptions saves a protobuf record with existence checking.
// This is the full-featured save method that supports all RecordExistenceCheck modes.
//
// Java equivalent: FDBRecordStore.saveRecordAsync(Message rec, RecordExistenceCheck existenceCheck, FDBRecordVersion version, VersionstampSaveBehavior behavior)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:496
//
// Parameters:
//   - record: The protobuf message to save
//   - existenceCheck: Validation to perform (NONE, ERROR_IF_EXISTS, etc.)
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: ErrRecordAlreadyExists, ErrRecordDoesNotExist, or ErrRecordTypeChanged based on existenceCheck
//
// Note: Version and versionstamp support will be added in Phase 2
func (store *FDBRecordStore) SaveRecordWithOptions(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return nil, err
	}

	// Extract the primary key from the record
	recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, fmt.Errorf("unknown record type: %s", recordTypeName)
	}

	if recordType.PrimaryKey == nil {
		return nil, fmt.Errorf("no primary key defined for record type: %s", recordTypeName)
	}

	// Extract primary key values using the key expression.
	// Primary keys must evaluate to exactly one tuple.
	keyTuples, err := recordType.PrimaryKey.Evaluate(record)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}
	if len(keyTuples) != 1 {
		return nil, fmt.Errorf("primary key expression must evaluate to exactly one tuple, got %d", len(keyTuples))
	}

	// Create primary key tuple
	keyValues := keyTuples[0]
	primaryKey := make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		primaryKey[i] = v
	}

	recordsSubspace := store.subspace.Sub(RecordKey)
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Always load the existing record (matching Java's saveRecordAsync behavior).
	// This is needed for: existence checks, record counting, and future
	// index updates / version management.
	var oldSizeInfo SizeInfo
	oldValue, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		&oldSizeInfo,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load existing record: %w", err)
	}
	oldRecordExists := oldValue != nil

	// Perform existence checks
	if existenceCheck != RecordExistenceCheckNone {
		if existenceCheck.ErrorIfExists() && oldRecordExists {
			return nil, &RecordAlreadyExistsError{
				Message:    "record already exists",
				PrimaryKey: primaryKey,
			}
		}

		if existenceCheck.ErrorIfNotExists() && !oldRecordExists {
			return nil, &RecordDoesNotExistError{
				Message:    "record does not exist",
				PrimaryKey: primaryKey,
			}
		}

		if existenceCheck.ErrorIfTypeChanged() && oldRecordExists {
			_, oldMsg, deserErr := store.deserializeAndDiscover(oldValue)
			if deserErr == nil {
				existingTypeName := string(oldMsg.ProtoReflect().Descriptor().Name())
				if existingTypeName != recordTypeName {
					return nil, &RecordTypeChangedError{
						Message:      "record type changed",
						PrimaryKey:   primaryKey,
						ActualType:   existingTypeName,
						ExpectedType: recordTypeName,
					}
				}
			}
		}
	}

	// Wrap the record in a UnionDescriptor like Java Record Layer does
	unionRecord, err := store.wrapInUnion(record, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap record in union: %w", err)
	}

	// Serialize the union message
	data, err := proto.Marshal(unionRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal union record: %w", err)
	}

	// If versioning is enabled, mark old record as having inline version for proper cleanup
	if store.metaData.IsStoreRecordVersions() && oldRecordExists {
		oldSizeInfo.VersionedInline = true
	}

	// Save using split helper (handles both split and unsplit data)
	var oldSizeInfoPtr *SizeInfo
	if oldRecordExists {
		oldSizeInfoPtr = &oldSizeInfo
	}

	var newSizeInfo SizeInfo
	if err := saveWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		data,
		splitEnabled,
		oldSizeInfoPtr,
		&newSizeInfo,
	); err != nil {
		return nil, fmt.Errorf("failed to save record: %w", err)
	}

	// Save version if versioning is enabled (handled separately from split helper
	// because Go FDB bindings need context-level AddVersionMutation for versionstamps)
	if store.metaData.IsStoreRecordVersions() {
		localVer := store.context.ClaimLocalVersion()
		version, verErr := IncompleteVersion(localVer)
		if verErr != nil {
			return nil, fmt.Errorf("failed to create incomplete version: %w", verErr)
		}
		if err := store.saveRecordVersion(primaryKey, version); err != nil {
			return nil, err
		}
	}

	// Only increment record count for new inserts (not updates).
	if !oldRecordExists {
		store.addRecordCount(record, littleEndianInt64One)
	}

	newStoredRecord := &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     record,
		KeyCount:   newSizeInfo.KeyCount,
		ValueSize:  newSizeInfo.ValueSize,
		KeySize:    newSizeInfo.KeySize,
		Split:      newSizeInfo.IsSplit,
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() {
		var oldStoredRecord *FDBStoredRecord[proto.Message]
		if oldRecordExists {
			oldRT, oldMsg, deserErr := store.deserializeAndDiscover(oldValue)
			if deserErr != nil {
				return nil, fmt.Errorf("failed to deserialize old record for index update: %w", deserErr)
			}
			oldStoredRecord = &FDBStoredRecord[proto.Message]{
				PrimaryKey: primaryKey,
				RecordType: oldRT,
				Record:     oldMsg,
			}
		}
		if err := store.updateSecondaryIndexes(oldStoredRecord, newStoredRecord); err != nil {
			return nil, err
		}
	}

	return newStoredRecord, nil
}

// InsertRecord saves a record and throws an error if it already exists.
// This is equivalent to SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists).
//
// Java equivalent: FDBRecordStore.insertRecordAsync(Message rec)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreBase.java:629
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: ErrRecordAlreadyExists if a record with the same primary key already exists
func (store *FDBRecordStore) InsertRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists)
}

// UpdateRecord saves a record and throws an error if it does not already exist or if its type has changed.
// This is equivalent to SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged).
//
// Java equivalent: FDBRecordStore.updateRecordAsync(Message rec)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreBase.java:649
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: ErrRecordDoesNotExist if no record exists with this primary key
//   - error: ErrRecordTypeChanged if an existing record has a different type
func (store *FDBRecordStore) UpdateRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
}

// AddRecordReadConflict adds a read conflict range for the given primary key.
// This ensures that if another transaction modifies this record before this transaction commits,
// this transaction will fail with a conflict error.
//
// Java equivalent: FDBRecordStore.addRecordReadConflict(Tuple primaryKey)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:1222
func (store *FDBRecordStore) AddRecordReadConflict(primaryKey tuple.Tuple) {
	recordRange := store.getRangeForRecord(primaryKey)
	_ = store.context.Transaction().AddReadConflictRange(recordRange)
}

// AddRecordWriteConflict adds a write conflict range for the given primary key.
// This ensures that if another transaction reads this record before this transaction commits,
// that transaction will fail with a conflict error.
//
// Java equivalent: FDBRecordStore.addRecordWriteConflict(Tuple primaryKey)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:1228
func (store *FDBRecordStore) AddRecordWriteConflict(primaryKey tuple.Tuple) {
	recordRange := store.getRangeForRecord(primaryKey)
	_ = store.context.Transaction().AddWriteConflictRange(recordRange)
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
	recordsSubspace := store.subspace.Sub(RecordKey)

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

// Context returns the record context this store is using
func (store *FDBRecordStore) Context() *FDBRecordContext {
	return store.context
}

// Subspace returns the subspace this store is using
func (store *FDBRecordStore) Subspace() subspace.Subspace {
	return store.subspace
}

// GetMetaData returns the metadata for this store.
// Matches Java's FDBRecordStore.getRecordMetaData().
func (store *FDBRecordStore) GetMetaData() *RecordMetaData {
	return store.metaData
}

// GetIndexMaintainer returns the index maintainer for the given index.
// Matches Java's FDBRecordStore.getIndexMaintainer().
func (store *FDBRecordStore) GetIndexMaintainer(index *Index) IndexMaintainer {
	return store.getIndexMaintainer(index)
}

// DeleteIndexEntries clears all entries for the given index.
// Matches Java's StandardIndexMaintainer.deleteWhere() with no predicate.
func (store *FDBRecordStore) DeleteIndexEntries(index *Index) {
	indexSub := store.indexSubspace(index)
	store.context.Transaction().ClearRange(indexSub)
}

// DeleteIndexEntriesInRange clears index entries matching the given tuple prefix.
// For example, passing tuple.Tuple{"alice"} clears all entries where the first
// indexed value is "alice".
func (store *FDBRecordStore) DeleteIndexEntriesInRange(index *Index, prefix tuple.Tuple) {
	indexSub := store.indexSubspace(index)
	prefixKey := indexSub.Pack(prefix)
	r, err := fdb.PrefixRange(prefixKey)
	if err != nil {
		return // Invalid prefix, nothing to clear
	}
	store.context.Transaction().ClearRange(r)
}

// DeleteAllRecords deletes all records from the store.
// Clears all data subspaces except StoreInfoKey (0) and IndexStateSpaceKey (5).
// Matches Java's FDBRecordStore.deleteAllRecords() which clears everything
// from records through index state begin, then from index state end through store end.
func (store *FDBRecordStore) DeleteAllRecords() error {
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return err
	}

	tx := store.context.Transaction()

	// Clear all subspaces except StoreInfoKey (0) and IndexStateSpaceKey (5).
	// Java does two range clears: [records, indexState) and (indexState, storeEnd).
	// We clear individual subspaces for clarity.
	for _, key := range []int{
		RecordKey,                   // 1 - records
		IndexKey,                    // 2 - index data
		IndexSecondarySpaceKey,      // 3 - secondary index data
		RecordCountKey,              // 4 - record counts
		IndexRangeSpaceKey,          // 6 - index ranges
		IndexUniquenessViolationsKey, // 7 - uniqueness violations
		RecordVersionKey,            // 8 - record versions
		IndexBuildSpaceKey,          // 9 - index build state
	} {
		tx.ClearRange(store.subspace.Sub(key))
	}

	// Reset record count to 0. ClearRange alone doesn't override pending atomic
	// Add mutations within the same transaction, so we also explicitly Set
	// the count key to 0 to ensure reads in the same tx see the reset.
	// Skip when count state is DISABLED (no count data should exist).
	countKey := store.metaData.GetRecordCountKey()
	if countKey != nil && !store.isRecordCountDisabled() {
		countSubspace := store.subspace.Sub(RecordCountKey)
		fdbKey := countSubspace.Pack(tuple.Tuple{})
		tx.Set(fdbKey, encodeRecordCount(0))
	}

	return nil
}

// updateSecondaryIndexes updates all relevant indexes for a record change.
// oldRecord is nil for inserts, newRecord is nil for deletes.
// Matches Java's FDBRecordStore.updateSecondaryIndexes().
func (store *FDBRecordStore) updateSecondaryIndexes(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}

	var recordType *RecordType
	if newRecord != nil {
		recordType = newRecord.RecordType
	} else {
		recordType = oldRecord.RecordType
	}

	// Type-specific indexes
	for _, index := range store.metaData.GetIndexesForRecordType(recordType.Name) {
		if !store.shouldMaintainIndex(index.Name) {
			continue
		}
		if err := store.updateOneIndex(index, oldRecord, newRecord); err != nil {
			return err
		}
	}

	// Universal indexes (apply to all record types)
	for _, index := range store.metaData.GetUniversalIndexes() {
		if !store.shouldMaintainIndex(index.Name) {
			continue
		}
		if err := store.updateOneIndex(index, oldRecord, newRecord); err != nil {
			return err
		}
	}

	return nil
}

// updateOneIndex routes to UpdateWhileWriteOnly or Update based on index state.
// Matches Java's FDBRecordStore.updateIndexes() per-index dispatch.
func (store *FDBRecordStore) updateOneIndex(index *Index, oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	maintainer := store.getIndexMaintainer(index)
	if store.IsIndexWriteOnly(index.Name) {
		return maintainer.UpdateWhileWriteOnly(oldRecord, newRecord)
	}
	return maintainer.Update(oldRecord, newRecord)
}

// RebuildIndex rebuilds an index within the current transaction.
// Clears existing index data, scans all records, and re-indexes them.
// Upon completion, the index is marked READABLE.
//
// Because this runs in a single transaction, it is limited by FDB's
// 5-second time limit and 10MB transaction size. For large stores,
// use OnlineIndexer.BuildIndex() which splits work across transactions.
//
// Matches Java's FDBRecordStore.rebuildIndex() which delegates to
// IndexingBase.rebuildIndexAsync() for the in-transaction path.
func (store *FDBRecordStore) RebuildIndex(index *Index) error {
	// Step 1: Clear index data and mark WRITE_ONLY.
	// Matches Java: clearAndMarkIndexWriteOnly(index)
	if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: clear and mark write-only: %w", index.Name, err)
	}

	// Step 2: Pre-mark the full range as built in the RangeSet.
	// Java does this BEFORE scanning records so that even if marking readable
	// fails (e.g. uniqueness violations), the range set records that all data
	// was scanned, preventing re-scanning on future builds.
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	if _, err := rangeSet.InsertRange(store.context.Transaction(), nil, nil, true); err != nil {
		return fmt.Errorf("rebuild index %q: insert full range: %w", index.Name, err)
	}

	// Step 3: Scan all records and build index entries.
	scanProps := ForwardScan()
	cursor := store.ScanRecords(nil, scanProps)
	maintainer := store.getIndexMaintainer(index)

	for rec, err := range cursor.Seq2(store.context.ctx) {
		if err != nil {
			return fmt.Errorf("rebuild index %q: scan records: %w", index.Name, err)
		}

		if !store.shouldIndexRecordForIndex(rec, index) {
			continue
		}

		if err := maintainer.Update(nil, rec); err != nil {
			return fmt.Errorf("rebuild index %q: index record pk=%v: %w", index.Name, rec.PrimaryKey, err)
		}
	}

	// Step 4: Mark index READABLE (or READABLE_UNIQUE_PENDING if violations exist).
	// Matches Java: uses markIndexReadable which checks violations for unique indexes.
	if _, err := store.MarkIndexReadableOrUniquePending(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: mark readable: %w", index.Name, err)
	}

	return nil
}

// validateFormatVersion checks that the stored format version is supported.
// Matches Java's FormatVersion.validateFormatVersion().
func (store *FDBRecordStore) validateFormatVersion(storeHeader *gen.DataStoreInfo) error {
	storedVersion := storeHeader.GetFormatVersion()
	if storedVersion > FormatVersionCurrent {
		return fmt.Errorf("unsupported format version %d (max supported: %d)", storedVersion, FormatVersionCurrent)
	}
	return nil
}

// checkPossiblyRebuild compares the stored metadata version with the current
// metadata version. If the current metadata has a higher version, indexes added
// since the old version are rebuilt or marked according to the IndexRebuildPolicy.
// Matches Java's FDBRecordStore.checkPossiblyRebuild() / checkRebuild() /
// getStatesForRebuildIndexes().
func (store *FDBRecordStore) checkPossiblyRebuild(storeHeader *gen.DataStoreInfo) error {
	oldMetaDataVersion := int(storeHeader.GetMetaDataversion())
	newMetaDataVersion := store.metaData.Version()

	if newMetaDataVersion <= oldMetaDataVersion {
		return nil
	}

	// Find indexes added since the old version.
	indexesToBuild := store.metaData.GetIndexesToBuildSince(oldMetaDataVersion)
	if len(indexesToBuild) > 0 {
		// Get record count for the policy decision (lazy in Java, eager here).
		recordCount, err := store.getRecordCountForRebuildPolicy()
		if err != nil {
			return fmt.Errorf("check record count for rebuild: %w", err)
		}

		for _, index := range indexesToBuild {
			// TODO: detect indexOnNewRecordTypes (index covers only record types
			// added in this same version bump). For now, conservatively false.
			desiredState := store.indexRebuildPolicy(index, recordCount, false)

			switch desiredState {
			case IndexStateReadable:
				if err := store.RebuildIndex(index); err != nil {
					return fmt.Errorf("auto-rebuild index %q on metadata version change (%d -> %d): %w",
						index.Name, oldMetaDataVersion, newMetaDataVersion, err)
				}
			case IndexStateWriteOnly:
				if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
					return fmt.Errorf("mark index %q write-only: %w", index.Name, err)
				}
			case IndexStateDisabled:
				if _, err := store.MarkIndexDisabled(index.Name); err != nil {
					return fmt.Errorf("mark index %q disabled: %w", index.Name, err)
				}
			}
		}
	}

	// Update store header with new metadata version and format version.
	// Matches Java's checkRebuild() which sets info.setFormatVersion(formatVersion).
	newVersion := int32(newMetaDataVersion)
	storeHeader.MetaDataversion = &newVersion
	fmtVersion := int32(FormatVersionCurrent)
	storeHeader.FormatVersion = &fmtVersion
	lastUpdateTime := uint64(time.Now().UnixMilli())
	storeHeader.LastUpdateTime = &lastUpdateTime
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return fmt.Errorf("update store header after rebuild: %w", err)
	}

	return nil
}

// getRecordCountForRebuildPolicy returns the approximate record count
// for the IndexRebuildPolicy decision. Uses GetRecordCount if available,
// falls back to 0 (which triggers inline rebuild — safe default for stores
// without counting enabled).
func (store *FDBRecordStore) getRecordCountForRebuildPolicy() (int64, error) {
	if store.metaData.GetRecordCountKey() != nil {
		count, err := store.GetRecordCount()
		if err != nil {
			return 0, err
		}
		return count, nil
	}
	// Without counting, we can't know the count cheaply.
	// Return 0 to trigger inline rebuild (safe for small stores,
	// matches Java's lazy evaluation where count is only fetched
	// if the checker explicitly requests it).
	return 0, nil
}

// shouldIndexRecordForIndex checks if a record matches the given index's record types.
// Returns true if the record's type has this index defined (either per-type or universal).
func (store *FDBRecordStore) shouldIndexRecordForIndex(rec *FDBStoredRecord[proto.Message], index *Index) bool {
	for _, idx := range store.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		if idx.Name == index.Name {
			return true
		}
	}
	for _, idx := range store.metaData.GetUniversalIndexes() {
		if idx.Name == index.Name {
			return true
		}
	}
	return false
}

// indexSubspace returns the FDB subspace for a specific index.
// Layout: [storeSubspace][IndexKey=2][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSubspace(Index).
func (store *FDBRecordStore) indexSubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
}

// getIndexMaintainer returns the appropriate IndexMaintainer for the given index.
// Dispatches to CountIndexMaintainer for COUNT indexes, StandardIndexMaintainer otherwise.
// Matches Java's FDBRecordStore.getIndexMaintainer() dispatch.
func (store *FDBRecordStore) getIndexMaintainer(index *Index) IndexMaintainer {
	idxSubspace := store.indexSubspace(index)
	tx := store.context.Transaction()
	switch index.Type {
	case IndexTypeCount:
		return newCountIndexMaintainer(index, idxSubspace, tx, store)
	default:
		return newStandardIndexMaintainer(index, idxSubspace, tx, store)
	}
}

// indexStoreContext interface implementation for FDBRecordStore.
func (store *FDBRecordStore) isIndexWriteOnly(index *Index) bool {
	return store.IsIndexWriteOnly(index.Name)
}

func (store *FDBRecordStore) isIndexReadableUniquePending(index *Index) bool {
	return store.GetIndexState(index.Name) == IndexStateReadableUniquePending
}

func (store *FDBRecordStore) addUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) {
	store.AddUniquenessViolation(index, indexKey, primaryKey)
}

func (store *FDBRecordStore) removeUniquenessViolations(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) {
	store.ResolveUniquenessViolation(index, indexKey, primaryKey)
}

// saveRecordVersion stores the version for a record using the new inline format.
// Version is stored adjacent to the record at recordsSubspace.pack(primaryKey, -1),
// matching Java's SplitHelper.RECORD_VERSION for format version >= 6.
// The value is a packed Tuple containing a Versionstamp, matching Java's
// SplitHelper.packVersion(). For incomplete versions, queues a SET_VERSIONSTAMPED_VALUE.
func (store *FDBRecordStore) saveRecordVersion(primaryKey tuple.Tuple, version *FDBRecordVersion) error {
	versionKey := store.versionKey(primaryKey)

	if version.IsComplete() {
		// Direct set for complete versions (rare — only when explicitly provided)
		// Pack as Tuple{Versionstamp} matching Java's SplitHelper.packVersion()
		store.context.Transaction().Set(versionKey, packVersion(version))
	} else {
		// Queue SET_VERSIONSTAMPED_VALUE for incomplete versions
		store.context.AddToLocalVersionCache(versionKey, version.GetLocalVersion())
		packed, err := buildVersionstampedValue(version)
		if err != nil {
			return fmt.Errorf("failed to build versionstamped value: %w", err)
		}
		store.context.AddVersionMutation(versionKey, packed)
	}
	return nil
}

// packVersion packs a complete FDBRecordVersion as a Tuple containing a Versionstamp.
// Matches Java's SplitHelper.packVersion().
func packVersion(version *FDBRecordVersion) []byte {
	var txVer [10]byte
	copy(txVer[:], version.GetGlobalVersion())
	vs := tuple.Versionstamp{
		TransactionVersion: txVer,
		UserVersion:        uint16(version.GetLocalVersion()),
	}
	return tuple.Tuple{vs}.Pack()
}

// unpackVersion unpacks a stored version value (a packed Tuple with a Versionstamp)
// into an FDBRecordVersion. Matches Java's SplitHelper.unpackVersion().
func unpackVersion(value []byte) (*FDBRecordVersion, error) {
	t, err := tuple.Unpack(fdb.Key(value))
	if err != nil {
		return nil, fmt.Errorf("failed to unpack version tuple: %w", err)
	}
	if len(t) < 1 {
		return nil, fmt.Errorf("version tuple is empty")
	}
	vs, ok := t[0].(tuple.Versionstamp)
	if !ok {
		return nil, fmt.Errorf("version tuple element is not a Versionstamp: %T", t[0])
	}
	return NewCompleteVersion(vs.TransactionVersion[:], int(vs.UserVersion))
}

// LoadRecordVersion loads the version associated with a record.
// Returns nil if no version is stored or versioning is not enabled.
// Matches Java's FDBRecordStore.loadRecordVersionAsync().
func (store *FDBRecordStore) LoadRecordVersion(primaryKey tuple.Tuple, snapshot bool) (*FDBRecordVersion, error) {
	versionKey := store.versionKey(primaryKey)

	// Check local cache first (for versions saved in the current transaction)
	if localVer, ok := store.context.GetLocalVersion(versionKey); ok {
		v, err := IncompleteVersion(localVer)
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	// Read from FDB
	var value []byte
	var getErr error
	if snapshot {
		value, getErr = store.context.Transaction().Snapshot().Get(fdb.Key(versionKey)).Get()
	} else {
		value, getErr = store.context.Transaction().Get(fdb.Key(versionKey)).Get()
	}
	if getErr != nil {
		return nil, fmt.Errorf("failed to load record version: %w", getErr)
	}

	if value == nil {
		return nil, nil
	}

	// Value is a packed Tuple containing a Versionstamp (matching Java's SplitHelper.unpackVersion())
	return unpackVersion(value)
}

// deleteRecordVersion clears the version key for a record.
func (store *FDBRecordStore) deleteRecordVersion(primaryKey tuple.Tuple) {
	versionKey := store.versionKey(primaryKey)
	store.context.Transaction().Clear(fdb.Key(versionKey))
	store.context.RemoveLocalVersion(versionKey)
	store.context.RemoveVersionMutation(versionKey)
}

// versionKey returns the FDB key for storing a record's version.
// Uses the new inline format: recordsSubspace.pack(primaryKey, RecordVersionSuffix).
// Matches Java's SplitHelper.RECORD_VERSION = -1L for format version >= 6.
func (store *FDBRecordStore) versionKey(primaryKey tuple.Tuple) fdb.Key {
	recordsSubspace := store.subspace.Sub(RecordKey)
	keyTuple := make(tuple.Tuple, len(primaryKey)+1)
	copy(keyTuple, primaryKey)
	keyTuple[len(primaryKey)] = RecordVersionSuffix
	return recordsSubspace.Pack(keyTuple)
}

// ScanRecords scans all records in the store.
// For forward scans, continuation sets the low endpoint (start after last returned key).
// For reverse scans, continuation sets the high endpoint (end before last returned key).
// Matches Java's KeyValueCursorBase behavior.
func (store *FDBRecordStore) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	lowEndpoint := EndpointTypeTreeStart
	highEndpoint := EndpointTypeTreeEnd
	if continuation != nil {
		if scanProperties.IsReverse() {
			highEndpoint = EndpointTypeContinuation
		} else {
			lowEndpoint = EndpointTypeContinuation
		}
	}
	return store.ScanRecordsInRange(nil, nil, lowEndpoint, highEndpoint, continuation, scanProperties)
}

// ScanRecordsByType scans records filtered to a specific record type.
// This is a convenience method that wraps ScanRecords with a type filter.
// Matches Java's RecordQuery with RecordTypeFilter.
func (store *FDBRecordStore) ScanRecordsByType(recordTypeName string, continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	inner := store.ScanRecords(continuation, scanProperties)
	return &filterCursor[*FDBStoredRecord[proto.Message]]{
		inner: inner,
		predicate: func(rec *FDBStoredRecord[proto.Message]) bool {
			return rec.RecordType.Name == recordTypeName
		},
	}
}

// ScanRecordsInRange scans records in a key range
func (store *FDBRecordStore) ScanRecordsInRange(
	low, high tuple.Tuple,
	lowEndpoint, highEndpoint EndpointType,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*FDBStoredRecord[proto.Message]] {
	// Calculate the prefix length for proper continuation handling
	// This is the length of the records subspace prefix
	recordsSubspace := store.subspace.Sub(RecordKey)
	prefixLength := len(recordsSubspace.FDBKey())
	
	return &keyValueCursor{
		store:          store,
		low:            low,
		high:           high,
		lowEndpoint:    lowEndpoint,
		highEndpoint:   highEndpoint,
		continuation:   continuation,
		scanProperties: scanProperties,
		prefixLength:   prefixLength,
		startTime:      time.Now(),
	}
}

// CountRecords counts records in a range by scanning the records subspace.
// Unlike GetRecordCount() which uses atomic counters, this actually scans records.
// Matches Java's FDBRecordStore.countRecords().
func (store *FDBRecordStore) CountRecords(
	ctx context.Context,
	low, high tuple.Tuple,
	lowEndpoint, highEndpoint EndpointType,
	continuation []byte,
	scanProperties ScanProperties,
) (int, error) {
	cursor := store.ScanRecordsInRange(low, high, lowEndpoint, highEndpoint, continuation, scanProperties)
	return GetCount(ctx, cursor)
}

// GetTypedRecordStore creates a type-safe wrapper for a specific record type
// This follows Java's FDBRecordStore.getTypedRecordStore() pattern
func GetTypedRecordStore[T proto.Message](store *FDBRecordStore, recordTypeName string) (*TypedFDBRecordStore[T], error) {
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, fmt.Errorf("record type '%s' not found in metadata", recordTypeName)
	}

	// Use reflection to create the wrap/unwrap functions automatically
	return NewTypedRecordStore[T](
		store,
		recordType,
		createUnwrapFunc[T](recordType),
		createWrapFunc[T](recordType),
	), nil
}

// createUnwrapFunc creates an unwrap function using reflection
func createUnwrapFunc[T proto.Message](recordType *RecordType) func(*gen.UnionDescriptor) (T, error) {
	return func(union *gen.UnionDescriptor) (T, error) {
		var zero T
		
		if recordType.UnionFieldDescriptor == nil {
			return zero, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
		}
		
		// Get the union message reflection
		unionReflect := union.ProtoReflect()
		
		// Get the field value using reflection
		fieldValue := unionReflect.Get(recordType.UnionFieldDescriptor)
		if !fieldValue.IsValid() || !fieldValue.Message().IsValid() {
			return zero, fmt.Errorf("union descriptor does not contain %s record", recordType.Name)
		}
		
		// Type assert to T
		result, ok := fieldValue.Message().Interface().(T)
		if !ok {
			return zero, fmt.Errorf("union field type mismatch: expected %T, got %T", zero, fieldValue.Message().Interface())
		}
		
		return result, nil
	}
}

// createWrapFunc creates a wrap function using reflection
func createWrapFunc[T proto.Message](recordType *RecordType) func(T) (*gen.UnionDescriptor, error) {
	return func(record T) (*gen.UnionDescriptor, error) {
		if recordType.UnionFieldDescriptor == nil {
			return nil, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
		}
		
		// Create a UnionDescriptor
		union := &gen.UnionDescriptor{}
		
		// Get the union message reflection
		unionReflect := union.ProtoReflect()
		
		// Set the field using reflection
		unionReflect.Set(recordType.UnionFieldDescriptor, protoreflect.ValueOfMessage(record.ProtoReflect()))
		
		return union, nil
	}
}

// FDBStoredRecord represents a record that has been stored in or loaded from FDB
// This is generic to match Java's FDBStoredRecord<M extends Message>
type FDBStoredRecord[M proto.Message] struct {
	// PrimaryKey is the record's primary key
	PrimaryKey tuple.Tuple

	// RecordType is the type of this record
	RecordType *RecordType

	// Record is the actual record data
	Record M

	// Storage size information
	KeyCount  int
	KeySize   int
	ValueSize int

	// Whether the record is split across multiple keys
	Split bool
}



// GetFormatVersion returns the store format version.
// Matches Java's FDBRecordStore.getFormatVersion().
func (store *FDBRecordStore) GetFormatVersion() int32 {
	if store.storeHeader != nil && store.storeHeader.FormatVersion != nil {
		return *store.storeHeader.FormatVersion
	}
	return 0
}

// GetUserVersion returns the user-defined store version.
// Matches Java's FDBRecordStore.getUserVersion().
func (store *FDBRecordStore) GetUserVersion() int32 {
	if store.storeHeader != nil && store.storeHeader.UserVersion != nil {
		return *store.storeHeader.UserVersion
	}
	return 0
}

// SetUserVersion updates the user-defined store version and writes it to FDB.
// Matches Java's FDBRecordStore.setUserVersion().
func (store *FDBRecordStore) SetUserVersion(version int32) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	store.storeHeader.UserVersion = &version
	lastUpdateTime := uint64(time.Now().UnixMilli())
	store.storeHeader.LastUpdateTime = &lastUpdateTime
	return store.writeStoreHeader(store.storeHeader)
}

// GetMetaDataVersion returns the metadata version stored in the header.
func (store *FDBRecordStore) GetMetaDataVersion() int32 {
	if store.storeHeader != nil && store.storeHeader.MetaDataversion != nil {
		return *store.storeHeader.MetaDataversion
	}
	return 0
}

// RecordStoreState captures the mutable state of a record store at a point in time.
// Matches Java's RecordStoreState.
type RecordStoreState struct {
	StoreHeader *gen.DataStoreInfo
	IndexStates map[string]IndexState
}

// GetRecordStoreState returns the current state of the store (header + index states).
// Matches Java's FDBRecordStore.getRecordStoreState().
func (store *FDBRecordStore) GetRecordStoreState() *RecordStoreState {
	states := make(map[string]IndexState, len(store.indexStates))
	for k, v := range store.indexStates {
		states[k] = v
	}
	return &RecordStoreState{
		StoreHeader: store.storeHeader,
		IndexStates: states,
	}
}

// SetStoreLockState sets the store lock state in the header and persists it.
// Use FORBID_RECORD_UPDATE to prevent record mutations.
// Matches Java's FDBRecordStore.setStoreLockStateAsync().
func (store *FDBRecordStore) SetStoreLockState(lockState *gen.DataStoreInfo_StoreLockState) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	store.storeHeader.StoreLockState = lockState
	return store.writeStoreHeader(store.storeHeader)
}

// ReloadRecordStoreState forces a reload of the store state from FDB.
// Useful when another transaction may have changed the state.
// Matches Java's FDBRecordStore.loadRecordStoreStateAsync() force reload path.
func (store *FDBRecordStore) ReloadRecordStoreState() error {
	exists, header, err := store.checkStoreExists()
	if err != nil {
		return err
	}
	if !exists {
		return ErrRecordStoreDoesNotExist
	}
	store.storeHeader = header
	return store.loadIndexStates()
}

// EstimateStoreSize returns the estimated byte size of the entire store subspace.
// Uses FDB's GetEstimatedRangeSizeBytes which provides an approximation.
// Matches Java's FDBRecordStore.estimateStoreSizeAsync().
func (store *FDBRecordStore) EstimateStoreSize() (int64, error) {
	begin, end := store.subspace.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	return store.context.Transaction().GetEstimatedRangeSizeBytes(kr).Get()
}

// EstimateRecordsSize returns the estimated byte size of the records subspace only.
// Matches Java's FDBRecordStore.estimateRecordsSizeAsync().
func (store *FDBRecordStore) EstimateRecordsSize() (int64, error) {
	recordsSub := store.subspace.Sub(RecordKey)
	begin, end := recordsSub.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	return store.context.Transaction().GetEstimatedRangeSizeBytes(kr).Get()
}

// UniquenessViolation represents a recorded uniqueness violation entry.
// Matches Java's RecordIndexUniquenessViolation.
type UniquenessViolation struct {
	IndexName  string
	IndexKey   tuple.Tuple
	PrimaryKey tuple.Tuple
}

// ScanUniquenessViolations returns all uniqueness violations recorded for the given index.
// Violations are stored in the IndexUniquenessViolationsKey (7) subspace.
// Matches Java's StandardIndexMaintainer.scanUniquenessViolations().
func (store *FDBRecordStore) ScanUniquenessViolations(index *Index) ([]UniquenessViolation, error) {
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	begin, end := violationSubspace.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(kr, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan uniqueness violations for index %q: %w", index.Name, err)
	}

	var violations []UniquenessViolation
	for _, kv := range kvs {
		t, err := violationSubspace.Unpack(kv.Key)
		if err != nil {
			return nil, fmt.Errorf("unpack violation key: %w", err)
		}
		// Key format: [indexKey..., primaryKey...]
		colCount := keyExpressionColumnSize(index.RootExpression)
		if len(t) > colCount {
			violations = append(violations, UniquenessViolation{
				IndexName:  index.Name,
				IndexKey:   tuple.Tuple(t[:colCount]),
				PrimaryKey: tuple.Tuple(t[colCount:]),
			})
		}
	}
	return violations, nil
}

// ResolveUniquenessViolation removes a single uniqueness violation entry.
// Call this after manually resolving the conflict (e.g., deleting the duplicate record).
// Matches Java's StandardIndexMaintainer.resolveUniquenessViolation().
func (store *FDBRecordStore) ResolveUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) {
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	entryKey := indexEntryKey(index, indexKey, primaryKey)
	store.context.Transaction().Clear(fdb.Key(violationSubspace.Pack(entryKey)))
}

// AddUniquenessViolation records a uniqueness violation for the given index.
// Used during WRITE_ONLY index builds when a uniqueness conflict is detected.
func (store *FDBRecordStore) AddUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) {
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	entryKey := indexEntryKey(index, indexKey, primaryKey)
	store.context.Transaction().Set(fdb.Key(violationSubspace.Pack(entryKey)), tuple.Tuple{}.Pack())
}

// createStoreHeader creates a DataStoreInfo header for a new record store
func createStoreHeader(metaDataVersion int32) *gen.DataStoreInfo {
	formatVersion := int32(FormatVersionCurrent)
	userVersion := int32(0) // Default user version
	lastUpdateTime := uint64(time.Now().UnixMilli())
	
	return &gen.DataStoreInfo{
		FormatVersion:   &formatVersion,
		MetaDataversion: &metaDataVersion, 
		UserVersion:     &userVersion,
		LastUpdateTime:  &lastUpdateTime,
	}
}

// checkStoreExists checks if a store exists and returns its state
func (store *FDBRecordStore) checkStoreExists() (bool, *gen.DataStoreInfo, error) {
	// Check if the first key in the subspace exists
	begin, end := store.subspace.FDBRangeKeys()
	storeRange := fdb.KeyRange{Begin: begin, End: end}
	
	kvs, err := store.context.Transaction().GetRange(storeRange, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return false, nil, fmt.Errorf("failed to read store range: %v", err)
	}
	if len(kvs) == 0 {
		// Store is completely empty
		return false, nil, nil
	}
	
	// Check if the first key is the store info header
	firstKV := kvs[0]
	expectedStoreInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})
	
	if !bytes.Equal(firstKV.Key, expectedStoreInfoKey) {
		// Store has data but no proper header - matches Java error
		return false, nil, ErrRecordStoreNoInfoButNotEmpty
	}
	
	// Parse the store header
	storeInfo := &gen.DataStoreInfo{}
	if err := proto.Unmarshal(firstKV.Value, storeInfo); err != nil {
		return false, nil, fmt.Errorf("failed to parse store header: %v", err)
	}
	
	return true, storeInfo, nil
}

// writeStoreHeader writes the store header to FDB
func (store *FDBRecordStore) writeStoreHeader(storeInfo *gen.DataStoreInfo) error {
	headerBytes, err := proto.Marshal(storeInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal store header: %v", err)
	}
	
	storeInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})
	store.context.Transaction().Set(storeInfoKey, headerBytes)
	return nil
}

// IndexRebuildPolicy determines what state a new/changed index should be put in
// when the store is opened with updated metadata.
// Matches Java's FDBRecordStoreBase.UserVersionChecker.needRebuildIndex().
type IndexRebuildPolicy func(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState

// DefaultIndexRebuildPolicy matches Java's default behavior:
// inline rebuild (READABLE) for stores with ≤200 records or indexes on new record types,
// DISABLED otherwise (requires OnlineIndexer).
// Java constant: FDBRecordStoreBase.MAX_RECORDS_FOR_REBUILD = 200.
func DefaultIndexRebuildPolicy(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState {
	const maxRecordsForRebuild = 200
	if indexOnNewRecordTypes || recordCount <= maxRecordsForRebuild {
		return IndexStateReadable
	}
	return IndexStateDisabled
}

// AlwaysRebuildPolicy always rebuilds indexes inline.
// Matches Java's ALWAYS_READABLE_CHECKER behavior.
func AlwaysRebuildPolicy(_ *Index, _ int64, _ bool) IndexState {
	return IndexStateReadable
}

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java exactly.
type StoreBuilder struct {
	context            *FDBRecordContext
	metaData           *RecordMetaData
	subspace           subspace.Subspace
	indexRebuildPolicy IndexRebuildPolicy
}

// NewStoreBuilder creates a new store builder
func NewStoreBuilder() *StoreBuilder {
	return &StoreBuilder{}
}

// SetContext sets the record context
func (b *StoreBuilder) SetContext(ctx *FDBRecordContext) *StoreBuilder {
	b.context = ctx
	return b
}

// SetMetaDataProvider sets the metadata
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder {
	b.metaData = metaData
	return b
}

// SetSubspace sets the subspace for this store
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder {
	b.subspace = subspace
	return b
}

// SetIndexRebuildPolicy sets the policy for rebuilding indexes during store open
// when the metadata version changes. If not set, DefaultIndexRebuildPolicy is used
// (inline rebuild for ≤200 records, DISABLED otherwise).
// Matches Java's FDBRecordStore.newBuilder().setUserVersionChecker().
func (b *StoreBuilder) SetIndexRebuildPolicy(policy IndexRebuildPolicy) *StoreBuilder {
	b.indexRebuildPolicy = policy
	return b
}

// newStore creates an FDBRecordStore from the builder's settings.
func (b *StoreBuilder) newStore() *FDBRecordStore {
	policy := b.indexRebuildPolicy
	if policy == nil {
		policy = DefaultIndexRebuildPolicy
	}
	return &FDBRecordStore{
		context:            b.context,
		metaData:           b.metaData,
		subspace:           b.subspace,
		indexRebuildPolicy: policy,
	}
}

// validateBuilder checks that all required fields are set
func (b *StoreBuilder) validateBuilder() error {
	if b.context == nil {
		return fmt.Errorf("context is required")
	}
	if b.metaData == nil {
		return fmt.Errorf("metadata is required")
	}
	if b.subspace == nil || b.subspace.Bytes() == nil {
		return fmt.Errorf("subspace is required")
	}
	return nil
}

// Create creates a new record store, fails if store already exists
func (b *StoreBuilder) Create() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Check if store already exists
	exists, _, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrRecordStoreAlreadyExists
	}

	// Create and write store header
	storeHeader := createStoreHeader(int32(b.metaData.Version()))
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return nil, err
	}
	store.storeHeader = storeHeader
	store.indexStates = make(map[string]IndexState)

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist.
// When the current metadata version is higher than the stored version,
// new indexes are automatically rebuilt inline (matching Java's checkVersion flow).
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Verify store exists and has proper header
	exists, storeHeader, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrRecordStoreDoesNotExist
	}
	store.storeHeader = storeHeader

	// Validate format version is supported.
	// Matches Java's FormatVersion.validateFormatVersion().
	if err := store.validateFormatVersion(storeHeader); err != nil {
		return nil, err
	}

	if err := store.loadIndexStates(); err != nil {
		return nil, err
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if err := store.checkPossiblyRebuild(storeHeader); err != nil {
		return nil, err
	}

	return store, nil
}

// CreateOrOpen creates store if it doesn't exist, opens if it does (like Java).
// When opening an existing store whose metadata version is older than the
// current metadata, new indexes are automatically rebuilt inline.
// Matches Java's FDBRecordStore.checkPossiblyRebuild().
func (b *StoreBuilder) CreateOrOpen() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Check if store exists
	exists, storeHeader, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}

	if !exists {
		// Create store header if it doesn't exist
		storeHeader = createStoreHeader(int32(b.metaData.Version()))
		if err := store.writeStoreHeader(storeHeader); err != nil {
			return nil, err
		}
		store.indexStates = make(map[string]IndexState)
	} else {
		// Validate format version is supported.
		if err := store.validateFormatVersion(storeHeader); err != nil {
			return nil, err
		}
		if err := store.loadIndexStates(); err != nil {
			return nil, err
		}
	}
	store.storeHeader = storeHeader

	// Check if metadata has evolved — rebuild new indexes if needed.
	if exists {
		if err := store.checkPossiblyRebuild(storeHeader); err != nil {
			return nil, err
		}
	}

	return store, nil
}

// Build returns a store without checking database state (advanced use case)
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	return b.newStore(), nil
}

// wrapInUnion wraps a record message in a UnionDescriptor for storage compatibility with Java
func (store *FDBRecordStore) wrapInUnion(record proto.Message, recordType *RecordType) (proto.Message, error) {
	// Create a UnionDescriptor
	union := &gen.UnionDescriptor{}
	
	// Use reflection to set the appropriate field in the union
	if recordType.UnionFieldDescriptor == nil {
		return nil, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
	}
	
	// Get the union message reflection
	unionReflect := union.ProtoReflect()
	
	// Set the field using reflection
	unionReflect.Set(recordType.UnionFieldDescriptor, protoreflect.ValueOfMessage(record.ProtoReflect()))
	
	return union, nil
}

// deserializeAndDiscover unmarshals a UnionDescriptor and discovers which record type
// is set by inspecting which field is populated. This is needed because with UnsplitRecord
// the key suffix is always 0 and doesn't encode the record type.
func (store *FDBRecordStore) deserializeAndDiscover(data []byte) (*RecordType, proto.Message, error) {
	union := &gen.UnionDescriptor{}
	if err := proto.Unmarshal(data, union); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal union descriptor: %w", err)
	}

	unionReflect := union.ProtoReflect()

	// Check each record type's union field to find which one is populated
	for _, rt := range store.metaData.RecordTypes() {
		if rt.UnionFieldDescriptor == nil {
			continue
		}
		if unionReflect.Has(rt.UnionFieldDescriptor) {
			fieldValue := unionReflect.Get(rt.UnionFieldDescriptor)
			if fieldValue.IsValid() && fieldValue.Message().IsValid() {
				return rt, fieldValue.Message().Interface(), nil
			}
		}
	}

	return nil, nil, fmt.Errorf("union descriptor does not contain any known record type")
}

// deserializeRecord unwraps a UnionDescriptor and extracts the actual record
func (store *FDBRecordStore) deserializeRecord(data []byte, recordType *RecordType) (proto.Message, error) {
	// First, deserialize the UnionDescriptor
	union := &gen.UnionDescriptor{}
	if err := proto.Unmarshal(data, union); err != nil {
		return nil, fmt.Errorf("failed to unmarshal union descriptor: %w", err)
	}
	
	// Use reflection to extract the specific record type from the union
	if recordType.UnionFieldDescriptor == nil {
		return nil, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
	}
	
	// Get the union message reflection
	unionReflect := union.ProtoReflect()
	
	// Get the field value using reflection
	fieldValue := unionReflect.Get(recordType.UnionFieldDescriptor)
	if !fieldValue.IsValid() || !fieldValue.Message().IsValid() {
		return nil, fmt.Errorf("union descriptor does not contain %s record", recordType.Name)
	}
	
	// Return the message interface
	return fieldValue.Message().Interface(), nil
}


// TypedFDBRecordStore provides type-safe record operations with generics
type TypedFDBRecordStore[T proto.Message] struct {
	baseStore  *FDBRecordStore
	recordType *RecordType
	unwrapFunc func(*gen.UnionDescriptor) (T, error)
	wrapFunc   func(T) (*gen.UnionDescriptor, error)
}

// NewTypedRecordStore creates a new typed record store that wraps the base store
func NewTypedRecordStore[T proto.Message](
	baseStore *FDBRecordStore,
	recordType *RecordType,
	unwrapFunc func(*gen.UnionDescriptor) (T, error),
	wrapFunc func(T) (*gen.UnionDescriptor, error),
) *TypedFDBRecordStore[T] {
	return &TypedFDBRecordStore[T]{
		baseStore:  baseStore,
		recordType: recordType,
		unwrapFunc: unwrapFunc,
		wrapFunc:   wrapFunc,
	}
}

// LoadRecord loads a record by its primary key with compile-time type safety
func (ts *TypedFDBRecordStore[T]) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[T], error) {
	// Use base store to load the raw record
	storedRecord, err := ts.baseStore.LoadRecord(primaryKey)
	if err != nil || storedRecord == nil {
		return nil, err
	}

	// The base store returns the unwrapped record (e.g., *gen.Order)
	// We need to type-assert it to our generic type T
	typedRecord, ok := storedRecord.Record.(T)
	if !ok {
		return nil, fmt.Errorf("loaded record type %T does not match expected type %T", storedRecord.Record, *new(T))
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     typedRecord,
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
}

// SaveRecord saves a typed record to the store
func (ts *TypedFDBRecordStore[T]) SaveRecord(record T) (*FDBStoredRecord[T], error) {
	// Use base store to save - it will handle UnionDescriptor wrapping
	storedRecord, err := ts.baseStore.SaveRecord(record)
	if err != nil {
		return nil, err
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     record, // Return the original typed record
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
}

// DeleteRecord deletes a typed record by its primary key
func (ts *TypedFDBRecordStore[T]) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	// Delegate to the base store for the actual deletion logic
	return ts.baseStore.DeleteRecord(primaryKey)
}

// RecordExists checks if a record exists with the given primary key
func (ts *TypedFDBRecordStore[T]) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	return ts.baseStore.RecordExists(primaryKey, isolationLevel)
}

// SaveRecordWithOptions saves a typed record with existence checking
func (ts *TypedFDBRecordStore[T]) SaveRecordWithOptions(
	record T,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[T], error) {
	// Use base store to save with options
	storedRecord, err := ts.baseStore.SaveRecordWithOptions(record, existenceCheck)
	if err != nil {
		return nil, err
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     record, // Return the original typed record
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
}

// InsertRecord saves a typed record and throws an error if it already exists
func (ts *TypedFDBRecordStore[T]) InsertRecord(record T) (*FDBStoredRecord[T], error) {
	return ts.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists)
}

// UpdateRecord saves a typed record and throws an error if it does not already exist or if its type has changed
func (ts *TypedFDBRecordStore[T]) UpdateRecord(record T) (*FDBStoredRecord[T], error) {
	return ts.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
}

// AddRecordReadConflict adds a read conflict range for the given primary key
func (ts *TypedFDBRecordStore[T]) AddRecordReadConflict(primaryKey tuple.Tuple) {
	ts.baseStore.AddRecordReadConflict(primaryKey)
}

// AddRecordWriteConflict adds a write conflict range for the given primary key
func (ts *TypedFDBRecordStore[T]) AddRecordWriteConflict(primaryKey tuple.Tuple) {
	ts.baseStore.AddRecordWriteConflict(primaryKey)
}

// Context returns the record context this store is using
func (ts *TypedFDBRecordStore[T]) Context() *FDBRecordContext {
	return ts.baseStore.Context()
}

// Subspace returns the subspace this store is using
func (ts *TypedFDBRecordStore[T]) Subspace() subspace.Subspace {
	return ts.baseStore.Subspace()
}

// DeleteAllRecords deletes all records from the store
func (ts *TypedFDBRecordStore[T]) DeleteAllRecords() error {
	return ts.baseStore.DeleteAllRecords()
}

// GetRecordCount returns the total record count
func (ts *TypedFDBRecordStore[T]) GetRecordCount() (int64, error) {
	return ts.baseStore.GetRecordCount()
}

// GetSnapshotRecordCount returns the count for a specific count key
func (ts *TypedFDBRecordStore[T]) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	return ts.baseStore.GetSnapshotRecordCount(countKey)
}

// ScanRecords returns a typed cursor scanning all records of this store's type
func (ts *TypedFDBRecordStore[T]) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	return ts.baseStore.ScanRecordsByType(ts.recordType.Name, continuation, scanProperties)
}

// Users of the library should create their own typed stores for their types
// Example usage in user code:
//
// orderStore := NewTypedRecordStore[*myapp.Order](
//     baseStore,
//     recordType,
//     myUnwrapFunc,
//     myWrapFunc,
// )