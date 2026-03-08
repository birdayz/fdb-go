package recordlayer

import (
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
	context     *FDBRecordContext
	metaData    *RecordMetaData
	subspace    subspace.Subspace
	storeHeader *gen.DataStoreInfo // Cached store header, loaded on Open/Create
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
		store.saveRecordVersion(primaryKey, version)
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

// DeleteAllRecords deletes all records from the store.
// This clears the records subspace and the record count subspace.
// Matches Java's FDBRecordStore.deleteAllRecords().
func (store *FDBRecordStore) DeleteAllRecords() error {
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return err
	}

	recordsSubspace := store.subspace.Sub(RecordKey)
	store.context.Transaction().ClearRange(recordsSubspace)

	// Clear record counts. ClearRange alone doesn't override pending atomic
	// Add mutations within the same transaction, so we also explicitly Set
	// the count key to 0 to ensure reads in the same tx see the reset.
	countSubspace := store.subspace.Sub(RecordCountKey)
	store.context.Transaction().ClearRange(countSubspace)

	countKey := store.metaData.GetRecordCountKey()
	if countKey != nil {
		fdbKey := countSubspace.Pack(tuple.Tuple{})
		store.context.Transaction().Set(fdbKey, encodeRecordCount(0))
	}

	// Clear version keys
	versionSubspace := store.subspace.Sub(RecordVersionKey)
	store.context.Transaction().ClearRange(versionSubspace)

	// Clear all index data
	indexSubspace := store.subspace.Sub(IndexKey)
	store.context.Transaction().ClearRange(indexSubspace)

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
		if err := store.getIndexMaintainer(index).Update(oldRecord, newRecord); err != nil {
			return err
		}
	}

	// Universal indexes (apply to all record types)
	for _, index := range store.metaData.GetUniversalIndexes() {
		if err := store.getIndexMaintainer(index).Update(oldRecord, newRecord); err != nil {
			return err
		}
	}

	return nil
}

// indexSubspace returns the FDB subspace for a specific index.
// Layout: [storeSubspace][IndexKey=2][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSubspace(Index).
func (store *FDBRecordStore) indexSubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
}

// getIndexMaintainer returns the appropriate IndexMaintainer for the given index.
// Currently only StandardIndexMaintainer (VALUE indexes) is supported.
func (store *FDBRecordStore) getIndexMaintainer(index *Index) IndexMaintainer {
	return newStandardIndexMaintainer(index, store.indexSubspace(index), store.context.Transaction())
}

// saveRecordVersion stores the version for a record using the new inline format.
// Version is stored adjacent to the record at recordsSubspace.pack(primaryKey, -1),
// matching Java's SplitHelper.RECORD_VERSION for format version >= 6.
// For incomplete versions, queues a SET_VERSIONSTAMPED_VALUE mutation.
func (store *FDBRecordStore) saveRecordVersion(primaryKey tuple.Tuple, version *FDBRecordVersion) {
	versionKey := store.versionKey(primaryKey)

	if version.IsComplete() {
		// Direct set for complete versions (rare — only when explicitly provided)
		store.context.Transaction().Set(versionKey, version.ToBytes())
	} else {
		// Queue SET_VERSIONSTAMPED_VALUE for incomplete versions
		store.context.AddToLocalVersionCache(versionKey, version.GetLocalVersion())
		store.context.AddVersionMutation(versionKey, buildVersionstampedValue(version))
	}
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

	return CompleteVersionFromBytes(value)
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
	}
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

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java exactly.
type StoreBuilder struct {
	context  *FDBRecordContext
	metaData *RecordMetaData
	subspace subspace.Subspace
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

	store := &FDBRecordStore{
		context:  b.context,
		metaData: b.metaData,
		subspace: b.subspace,
	}

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

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := &FDBRecordStore{
		context:  b.context,
		metaData: b.metaData,
		subspace: b.subspace,
	}

	// Verify store exists and has proper header
	exists, storeHeader, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrRecordStoreDoesNotExist
	}
	store.storeHeader = storeHeader

	return store, nil
}

// CreateOrOpen creates store if it doesn't exist, opens if it does (like Java)
func (b *StoreBuilder) CreateOrOpen() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := &FDBRecordStore{
		context:  b.context,
		metaData: b.metaData,
		subspace: b.subspace,
	}

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
	}
	store.storeHeader = storeHeader

	return store, nil
}

// Build returns a store without checking database state (advanced use case)
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	return &FDBRecordStore{
		context:  b.context,
		metaData: b.metaData,
		subspace: b.subspace,
	}, nil
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