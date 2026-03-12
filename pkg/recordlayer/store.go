package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Record Layer format versions - should match Java FormatVersion enum.
// See FormatVersion.java for the full list.
const (
	FormatVersionHeaderUserFields          = 8  // User-defined key→bytes map in store header
	FormatVersionReadableUniquePending     = 9  // READABLE_UNIQUE_PENDING index state
	FormatVersionCheckIndexBuildType       = 10 // Non-idempotent index build-from-source validation
	FormatVersionRecordCountState          = 11 // RecordCountState enum (READABLE/WRITE_ONLY/DISABLED)
	FormatVersionStoreLockState            = 12 // StoreLockState with FORBID_RECORD_UPDATE + FULL_STORE
	FormatVersionIncarnation               = 13 // Incarnation counter for cross-cluster migration
	FormatVersionFullStoreLock             = 14 // Unknown lock states prevent store opening
	FormatVersionCurrent                   = FormatVersionFullStoreLock
)

// StoreIsLockedForRecordUpdatesError is returned when attempting to modify records
// in a store with FORBID_RECORD_UPDATE lock state.
// Matches Java's com.apple.foundationdb.record.StoreIsLockedForRecordUpdates.
type StoreIsLockedForRecordUpdatesError struct {
	Reason    string
	Timestamp int64
}

func (e *StoreIsLockedForRecordUpdatesError) Error() string {
	return fmt.Sprintf("Record Store is locked for record updates: %s (timestamp: %d)", e.Reason, e.Timestamp)
}

// StoreIsFullyLockedError is returned when attempting to open a store with
// FULL_STORE lock state without providing the correct bypass reason.
// Matches Java's com.apple.foundationdb.record.StoreIsFullyLockedException.
type StoreIsFullyLockedError struct {
	Reason    string
	Timestamp int64
}

func (e *StoreIsFullyLockedError) Error() string {
	return fmt.Sprintf("Record Store is fully locked and cannot be opened: %s (timestamp: %d)", e.Reason, e.Timestamp)
}

// UnknownStoreLockStateError is returned when a store has an unrecognized lock state
// at FormatVersion >= FULL_STORE_LOCK (14). This prevents opening stores with
// lock states added by newer versions that we don't understand.
// Matches Java's com.apple.foundationdb.record.UnknownStoreLockStateException.
type UnknownStoreLockStateError struct {
	LockStateValue int32
}

func (e *UnknownStoreLockStateError) Error() string {
	return fmt.Sprintf("Store has unknown lock state: %d", e.LockStateValue)
}

// ErrRecordStoreStateNotLoaded indicates that the record store state needs to be loaded before operations
var ErrRecordStoreStateNotLoaded = errors.New("record store state not loaded")

// Store creation/existence errors
var (
	ErrRecordStoreAlreadyExists    = errors.New("record store already exists")
	ErrRecordStoreDoesNotExist     = errors.New("record store does not exist")
	ErrRecordStoreNoInfoButNotEmpty = errors.New("record store has no info but is not empty")
)

// FormatVersionCacheableState is the minimum format version required for
// store state cacheability. Matches Java's FormatVersion.CACHEABLE_STATE.
const FormatVersionCacheableState = 7

// StaleMetaDataVersionError is returned when the stored metadata version is
// newer than the local metadata version, meaning another instance already
// evolved the schema. Matches Java's RecordStoreStaleMetaDataVersionException.
type StaleMetaDataVersionError struct {
	LocalVersion  int
	StoredVersion int
}

func (e *StaleMetaDataVersionError) Error() string {
	return fmt.Sprintf("local meta-data has stale version: local %d, stored %d", e.LocalVersion, e.StoredVersion)
}

// FDBRecordStore provides record storage operations within a transaction context.
// This is the main struct for storing and retrieving records.
type FDBRecordStore struct {
	context            *FDBRecordContext
	metaData           *RecordMetaData
	subspace           subspace.Subspace
	storeHeader        *gen.DataStoreInfo    // Cached store header, loaded on Open/Create
	indexStates        map[string]IndexState  // Cached index states, loaded on Open/Create
	indexRebuildPolicy IndexRebuildPolicy     // Policy for rebuilding indexes on metadata version change
	storeStateCache    FDBRecordStoreStateCache // Cache for store state across transactions
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

// validateStoreLockState checks lock state during store open.
// FULL_STORE prevents opening unless bypass reason matches exactly.
// At FormatVersion >= FULL_STORE_LOCK (14), unknown/unspecified states also prevent opening.
// Matches Java's FDBRecordStore.validateStoreLockState().
func validateStoreLockState(storeHeader *gen.DataStoreInfo, bypassFullStoreLockReason string) error {
	lockState := storeHeader.GetStoreLockState()
	if lockState == nil {
		return nil
	}

	state := lockState.GetLockState()

	// FULL_STORE: blocked unless bypass reason matches exactly
	if state == gen.DataStoreInfo_StoreLockState_FULL_STORE {
		if bypassFullStoreLockReason != "" && bypassFullStoreLockReason == lockState.GetReason() {
			return nil // bypass successful
		}
		return &StoreIsFullyLockedError{
			Reason:    lockState.GetReason(),
			Timestamp: lockState.GetTimestamp(),
		}
	}

	// At FormatVersion >= FULL_STORE_LOCK, reject unknown/unspecified states.
	// FORBID_RECORD_UPDATE is known and handled at mutation time, so skip it here.
	if storeHeader.GetFormatVersion() >= FormatVersionFullStoreLock {
		if state != gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE {
			return &UnknownStoreLockStateError{LockStateValue: int32(state)}
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

	stored := &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     protoMessage,
		Store:      store,
		KeyCount:   sizeInfo.KeyCount,
		ValueSize:  sizeInfo.ValueSize,
		KeySize:    sizeInfo.KeySize,
		Split:      sizeInfo.IsSplit,
	}

	// Load version if versioning is enabled.
	// Matches Java's loadTypedRecord which eagerly loads the version.
	if store.metaData.IsStoreRecordVersions() {
		ver, err := store.LoadRecordVersion(primaryKey, false)
		if err != nil {
			return nil, fmt.Errorf("load record version for %v: %w", primaryKey, err)
		}
		stored.Version = ver
	}

	return stored, nil
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

	// Check lock state AFTER load but BEFORE write, matching Java's
	// deleteTypedRecord() which loads first, then validates.
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return false, err
	}

	// Deserialize old record BEFORE deleting — Java deserializes first
	// (loadTypedRecord at line 1676), then deletes (deleteRecordSplits at line 1682).
	// Must deserialize before delete so corrupt data errors don't cause data loss.
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

	// Check for inline version
	if store.metaData.IsStoreRecordVersions() {
		oldSizeInfo.VersionedInline = true
	}

	// Load old record's version BEFORE deleteSplit clears the FDB keys and
	// BEFORE we clean up the local version cache. We need the old version to
	// remove the old VERSION index entry.
	// Matches Java's loadExistingRecord which returns the full record including version.
	var oldRecordVersion *FDBRecordVersion
	if store.metaData.IsStoreRecordVersions() && store.hasVersionIndex() {
		ver, verErr := store.LoadRecordVersion(primaryKey, false)
		if verErr != nil {
			return false, fmt.Errorf("load old record version for index update: %w", verErr)
		}
		oldRecordVersion = ver
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

	// Decrement record count
	if store.metaData.GetRecordCountKey() != nil && oldMsg != nil {
		if err := store.addRecordCount(oldMsg, littleEndianInt64MinusOne); err != nil {
			return false, fmt.Errorf("failed to decrement record count: %w", err)
		}
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() && oldMsg != nil {
		oldStoredRecord := &FDBStoredRecord[proto.Message]{
			PrimaryKey: primaryKey,
			RecordType: oldRecordType,
			Record:     oldMsg,
			Version:    oldRecordVersion,
			Store:      store,
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
	keyTuples, err := recordType.PrimaryKey.Evaluate(nil, record)
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
			if deserErr != nil {
				// Propagate deserialization error. Java's loadExistingRecord()
				// deserializes before the type check — if deser fails, error propagates.
				return nil, fmt.Errorf("failed to deserialize existing record for type check: %w", deserErr)
			}
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

	// Check lock state AFTER load and existence checks but BEFORE write,
	// matching Java's saveRecordAsync() error precedence: existence/type
	// errors take priority over lock errors.
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return nil, err
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

	// Load old record's version BEFORE saveWithSplit clears the version key
	// and BEFORE saveRecordVersion overwrites the local version cache. We need
	// the old version to remove the old VERSION index entry on update.
	// Matches Java's loadExistingRecord which returns the full record including version.
	var oldRecordVersion *FDBRecordVersion
	if oldRecordExists && store.metaData.IsStoreRecordVersions() && store.hasVersionIndex() {
		ver, verErr := store.LoadRecordVersion(primaryKey, false)
		if verErr != nil {
			return nil, fmt.Errorf("load old record version for index update: %w", verErr)
		}
		oldRecordVersion = ver
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
	var savedVersion *FDBRecordVersion
	if store.metaData.IsStoreRecordVersions() {
		localVer := store.context.ClaimLocalVersion()
		version, verErr := IncompleteVersion(localVer)
		if verErr != nil {
			return nil, fmt.Errorf("failed to create incomplete version: %w", verErr)
		}
		if err := store.saveRecordVersion(primaryKey, version); err != nil {
			return nil, err
		}
		savedVersion = version
	}

	// Only increment record count for new inserts (not updates).
	if !oldRecordExists {
		if err := store.addRecordCount(record, littleEndianInt64One); err != nil {
			return nil, fmt.Errorf("failed to increment record count: %w", err)
		}
	}

	newStoredRecord := &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     record,
		Version:    savedVersion,
		Store:      store,
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
				Version:    oldRecordVersion,
				Store:      store,
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
// hasVersionIndex returns true if any index uses IndexTypeVersion.
// Used to decide whether old record versions need to be loaded for index maintenance.
func (store *FDBRecordStore) hasVersionIndex() bool {
	for _, idx := range store.metaData.GetAllIndexes() {
		if idx.Type == IndexTypeVersion {
			return true
		}
	}
	return false
}

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
	} {
		sub := store.subspace.Sub(key)
		if pr, err := fdb.PrefixRange(sub.Bytes()); err == nil {
			tx.ClearRange(pr)
		} else {
			tx.ClearRange(sub)
		}
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
	} {
		sub := store.subspace.Sub(key)
		if pr, err := fdb.PrefixRange(sub.Bytes()); err == nil {
			begin, end := pr.FDBRangeKeys()
			store.context.RemoveVersionMutationsInRange(begin.FDBKey(), end.FDBKey())
			store.context.RemoveLocalVersionsInRange(begin.FDBKey(), end.FDBKey())
		}
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
//
// When old and new records have the same type (or one is nil), indexes are
// updated straightforwardly. When types differ (cross-type overwrite), indexes
// are partitioned into three sets: old-only (delete entries), new-only (insert
// entries), and common (update entries). Matches Java's FDBRecordStore.updateSecondaryIndexes().
func (store *FDBRecordStore) updateSecondaryIndexes(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}

	// Fast path: same type (or one side nil) — no three-way split needed.
	sameType := oldRecord == nil || newRecord == nil || oldRecord.RecordType.Name == newRecord.RecordType.Name
	if sameType {
		var recordType *RecordType
		if newRecord != nil {
			recordType = newRecord.RecordType
		} else {
			recordType = oldRecord.RecordType
		}

		for _, index := range store.metaData.GetIndexesForRecordType(recordType.Name) {
			if !store.shouldMaintainIndex(index.Name) {
				continue
			}
			if err := store.updateOneIndex(index, oldRecord, newRecord); err != nil {
				return err
			}
		}
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

	// Slow path: cross-type overwrite. Partition indexes into old-only,
	// new-only, and common sets. Matches Java's three-way split.
	oldIndexes := store.enabledIndexesForRecord(oldRecord)
	newIndexes := store.enabledIndexesForRecord(newRecord)

	commonIndexes, oldOnly, newOnly := partitionIndexes(oldIndexes, newIndexes)

	// Delete entries from indexes that only the old type had.
	for _, index := range oldOnly {
		if err := store.updateOneIndex(index, oldRecord, nil); err != nil {
			return err
		}
	}
	// Insert entries into indexes that only the new type has.
	for _, index := range newOnly {
		if err := store.updateOneIndex(index, nil, newRecord); err != nil {
			return err
		}
	}
	// Update entries in indexes shared by both types.
	for _, index := range commonIndexes {
		if err := store.updateOneIndex(index, oldRecord, newRecord); err != nil {
			return err
		}
	}

	return nil
}

// enabledIndexesForRecord returns all enabled indexes (type-specific + universal)
// that apply to the given record.
func (store *FDBRecordStore) enabledIndexesForRecord(rec *FDBStoredRecord[proto.Message]) []*Index {
	var result []*Index
	for _, index := range store.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		if store.shouldMaintainIndex(index.Name) {
			result = append(result, index)
		}
	}
	for _, index := range store.metaData.GetUniversalIndexes() {
		if store.shouldMaintainIndex(index.Name) {
			result = append(result, index)
		}
	}
	return result
}

// partitionIndexes splits old and new index lists into three disjoint sets:
// common (in both), oldOnly (only in old), newOnly (only in new).
// Identity is by index name.
func partitionIndexes(oldIndexes, newIndexes []*Index) (common, oldOnly, newOnly []*Index) {
	newSet := make(map[string]*Index, len(newIndexes))
	for _, idx := range newIndexes {
		newSet[idx.Name] = idx
	}

	oldSet := make(map[string]struct{}, len(oldIndexes))
	for _, idx := range oldIndexes {
		oldSet[idx.Name] = struct{}{}
		if _, ok := newSet[idx.Name]; ok {
			common = append(common, idx)
		} else {
			oldOnly = append(oldOnly, idx)
		}
	}

	for _, idx := range newIndexes {
		if _, ok := oldSet[idx.Name]; !ok {
			newOnly = append(newOnly, idx)
		}
	}
	return
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

// indexSecondarySubspace returns the FDB subspace for a RANK index's secondary data (ranked sets).
// Layout: [storeSubspace][IndexSecondarySpaceKey=3][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSecondarySubspace(Index).
func (store *FDBRecordStore) indexSecondarySubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexSecondarySpaceKey, index.SubspaceTupleKey())
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
	case IndexTypeCountNotNull:
		return newCountNotNullIndexMaintainer(index, idxSubspace, tx, store)
	case IndexTypeCountUpdates:
		return newCountUpdatesIndexMaintainer(index, idxSubspace, tx, store)
	case IndexTypeSum:
		return newSumIndexMaintainer(index, idxSubspace, tx, store)
	case IndexTypeMaxEverLong:
		return newMinMaxEverIndexMaintainer(index, idxSubspace, tx, store, true)
	case IndexTypeMinEverLong:
		return newMinMaxEverIndexMaintainer(index, idxSubspace, tx, store, false)
	case IndexTypeMaxEverTuple:
		return newMinMaxEverTupleIndexMaintainer(index, idxSubspace, tx, store, true)
	case IndexTypeMinEverTuple:
		return newMinMaxEverTupleIndexMaintainer(index, idxSubspace, tx, store, false)
	case IndexTypeRank:
		secSubspace := store.indexSecondarySubspace(index)
		return newRankIndexMaintainer(index, idxSubspace, secSubspace, tx, store)
	case IndexTypeVersion:
		return newVersionIndexMaintainer(index, idxSubspace, tx, store.context, store)
	case IndexTypeMaxEverVersion:
		return newMaxEverVersionIndexMaintainer(index, idxSubspace, tx, store.context, store)
	case IndexTypePermutedMin:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, false)
	case IndexTypePermutedMax:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, true)
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

func (store *FDBRecordStore) isKeyInIndexBuildRange(index *Index, primaryKey tuple.Tuple) (bool, error) {
	irs := NewIndexingRangeSet(store.subspace, index)
	return irs.ContainsKey(store.context.Transaction(), primaryKey.Pack())
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

// FDBStoredRecord represents a record that has been stored in or loaded from FDB
// This is generic to match Java's FDBStoredRecord<M extends Message>
type FDBStoredRecord[M proto.Message] struct {
	// PrimaryKey is the record's primary key
	PrimaryKey tuple.Tuple

	// RecordType is the type of this record
	RecordType *RecordType

	// Record is the actual record data
	Record M

	// Version is the record's version, if loaded.
	// Matches Java's FDBStoredRecord.getVersion().
	Version *FDBRecordVersion

	// Store is the record store this record belongs to.
	// Used by FunctionKeyExpression (e.g. get_versionstamp_incarnation) to access store state.
	// Matches Java's FDBRecord.getStore().
	Store *FDBRecordStore

	// Storage size information
	KeyCount  int
	KeySize   int
	ValueSize int

	// Whether the record is split across multiple keys
	Split bool
}

// HasVersion returns whether this stored record has a version.
// Matches Java's FDBStoredRecord.hasVersion().
func (r *FDBStoredRecord[M]) HasVersion() bool {
	return r.Version != nil
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

// GetIncarnation returns the incarnation counter from the store header.
// Used for cross-cluster data migration versioning.
// Matches Java's FDBRecordStore.getIncarnation().
func (store *FDBRecordStore) GetIncarnation() int32 {
	if store.storeHeader != nil {
		return store.storeHeader.GetIncarnation()
	}
	return 0
}

// UpdateIncarnation atomically updates the incarnation counter using the provided
// function. The new value must be strictly greater than the current value.
// Matches Java's FDBRecordStore.updateIncarnation().
func (store *FDBRecordStore) UpdateIncarnation(updater func(current int32) int32) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	current := store.storeHeader.GetIncarnation()
	newVal := updater(current)
	if newVal <= current {
		return fmt.Errorf("incarnation must increase: current %d, new %d", current, newVal)
	}
	store.storeHeader.Incarnation = &newVal
	return store.writeStoreHeader(store.storeHeader)
}

// GetHeaderUserField returns a user-defined field from the store header.
// Returns nil if the field is not set.
// Matches Java's FDBRecordStore.getHeaderUserField().
func (store *FDBRecordStore) GetHeaderUserField(key string) []byte {
	if store.storeHeader == nil {
		return nil
	}
	for _, entry := range store.storeHeader.GetUserField() {
		if entry.GetKey() == key {
			return entry.GetValue()
		}
	}
	return nil
}

// SetHeaderUserField sets a user-defined field in the store header.
// The value is persisted immediately. Keep values small since the entire
// header is loaded on every store open.
// Matches Java's FDBRecordStore.setHeaderUserField().
func (store *FDBRecordStore) SetHeaderUserField(key string, value []byte) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	// Update existing entry or append new one
	for _, entry := range store.storeHeader.UserField {
		if entry.GetKey() == key {
			entry.Value = value
			return store.writeStoreHeader(store.storeHeader)
		}
	}
	store.storeHeader.UserField = append(store.storeHeader.UserField, &gen.DataStoreInfo_UserFieldEntry{
		Key:   &key,
		Value: value,
	})
	return store.writeStoreHeader(store.storeHeader)
}

// ClearHeaderUserField removes a user-defined field from the store header.
// Matches Java's FDBRecordStore.clearHeaderUserField().
func (store *FDBRecordStore) ClearHeaderUserField(key string) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	fields := store.storeHeader.UserField
	for i, entry := range fields {
		if entry.GetKey() == key {
			store.storeHeader.UserField = append(fields[:i], fields[i+1:]...)
			return store.writeStoreHeader(store.storeHeader)
		}
	}
	return nil // not found, no-op
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
	maps.Copy(states, store.indexStates)
	return &RecordStoreState{
		StoreHeader: store.storeHeader,
		IndexStates: states,
	}
}

// SetStoreLockState sets the store lock state in the header and persists it.
// Use FORBID_RECORD_UPDATE to prevent record mutations, or FULL_STORE to
// prevent the store from being opened entirely.
// Matches Java's FDBRecordStore.setStoreLockStateAsync().
func (store *FDBRecordStore) SetStoreLockState(state gen.DataStoreInfo_StoreLockState_State, reason string) error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	ts := time.Now().UnixMilli()
	store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
		LockState: &state,
		Reason:    &reason,
		Timestamp: &ts,
	}
	return store.writeStoreHeader(store.storeHeader)
}

// ClearStoreLockState removes the lock state from the store header.
// Matches Java's FDBRecordStore.clearStoreLockStateAsync().
func (store *FDBRecordStore) ClearStoreLockState() error {
	if store.storeHeader == nil {
		return ErrRecordStoreStateNotLoaded
	}
	store.storeHeader.StoreLockState = nil
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

// loadStoreState loads store state via the cache or directly from FDB.
// When bypassFullStoreLockReason is set, the cache is bypassed entirely
// (matching Java's checkVersion which skips cache on lock bypass).
func (store *FDBRecordStore) loadStoreState(existenceCheck StoreExistenceCheck, bypassReason string) error {
	if bypassReason != "" {
		// Bypass cache when using lock bypass — need fresh state to validate lock.
		state, err := loadRecordStoreState(store, existenceCheck)
		if err != nil {
			return err
		}
		store.storeHeader = state.StoreHeader
		store.indexStates = state.IndexStates
		return nil
	}

	entry, err := store.storeStateCache.Get(store, existenceCheck)
	if err != nil {
		return err
	}

	store.storeHeader = entry.GetRecordStoreState().StoreHeader
	store.indexStates = entry.GetRecordStoreState().IndexStates
	return nil
}

// SetStateCacheability sets whether this store's state can be cached across
// transactions. When enabled, the metadata version stamp is initialized so
// cache invalidation can work. Requires FormatVersion >= CACHEABLE_STATE (7).
// Returns true if the cacheability was changed, false if already at the desired state.
// Matches Java's FDBRecordStore.setStateCacheabilityAsync().
func (store *FDBRecordStore) SetStateCacheability(cacheable bool) (bool, error) {
	if store.storeHeader == nil {
		return false, ErrRecordStoreStateNotLoaded
	}
	if store.storeHeader.GetFormatVersion() < FormatVersionCacheableState {
		return false, fmt.Errorf("store format version %d does not support cacheability (requires >= %d)",
			store.storeHeader.GetFormatVersion(), FormatVersionCacheableState)
	}
	if store.storeHeader.GetCacheable() == cacheable {
		return false, nil
	}
	store.storeHeader.Cacheable = &cacheable
	if err := store.writeStoreHeader(store.storeHeader); err != nil {
		return false, err
	}
	return true, nil
}

// IsStateCacheable returns whether this store's state is currently cacheable.
func (store *FDBRecordStore) IsStateCacheable() bool {
	return store.storeHeader != nil && store.storeHeader.GetCacheable()
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
