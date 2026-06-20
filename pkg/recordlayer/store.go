package recordlayer

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"
	"unsafe"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Record Layer format versions - should match Java FormatVersion enum.
// See FormatVersion.java for the full list.
const (
	formatVersionInfoAdded = 1 // Minimum version (INFO_ADDED in Java)
	// formatVersionSaveUnsplitWithSuffix (5, SAVE_UNSPLIT_WITH_SUFFIX) is the version at
	// which every record key carries a split suffix even when split_long_records is false —
	// unless DataStoreInfo.omit_unsplit_record_suffix is true (set when a store created at an
	// earlier format version is upgraded). Below this version, unsplit records are stored at
	// the bare key recordsSubspace.pack(primaryKey) with no suffix.
	formatVersionSaveUnsplitWithSuffix = 5
	// formatVersionSaveVersionWithRecord (6, SAVE_VERSION_WITH_RECORD) is the version at which
	// record versions are stored inline adjacent to the record (recordsSubspace.pack(pk, -1))
	// rather than in the separate RecordVersionKey(8) subspace. See useOldVersionFormat().
	formatVersionSaveVersionWithRecord = 6
	// (7 = CACHEABLE_STATE; declared separately as formatVersionCacheableState below.)
	formatVersionHeaderUserFields      = 8  // User-defined key→bytes map in store header
	formatVersionReadableUniquePending = 9  // READABLE_UNIQUE_PENDING index state
	formatVersionCheckIndexBuildType   = 10 // Non-idempotent index build-from-source validation
	formatVersionRecordCountState      = 11 // RecordCountState enum (READABLE/WRITE_ONLY/DISABLED)
	formatVersionStoreLockState        = 12 // StoreLockState with FORBID_RECORD_UPDATE + FULL_STORE
	formatVersionIncarnation           = 13 // Incarnation counter for cross-cluster migration
	formatVersionFullStoreLock         = 14 // Unknown lock states prevent store opening
	formatVersionCurrent               = formatVersionFullStoreLock
	formatVersionMinimum               = formatVersionInfoAdded // Matches Java's FormatVersion.getMinimumVersion()
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

// formatVersionCacheableState is the minimum format version required for
// store state cacheability. Matches Java's FormatVersion.CACHEABLE_STATE.
const formatVersionCacheableState = 7

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
	recordsSubspace    subspace.Subspace        // Cached subspace.Sub(RecordKey) — avoids alloc per method call
	storeHeader        *gen.DataStoreInfo       // Cached store header, loaded on Open/Create or lazily
	indexStates        map[string]IndexState    // Cached index states, loaded on Open/Create or lazily
	indexRebuildPolicy IndexRebuildPolicy       // Policy for rebuilding indexes on metadata version change
	storeStateCache    FDBRecordStoreStateCache // Cache for store state across transactions
	stateMu            sync.RWMutex             // protects storeHeader + indexStates
	stateLoadOnce      sync.Once                // ensures lazy store state load happens exactly once (Build() path)
	stateLoadErr       error                    // error from lazy load (nil if loaded successfully or not yet attempted)
	versionChanged     bool                     // true if checkPossiblyRebuild detected a version change
	maintainerCache    sync.Map                 // string → IndexMaintainer, cached per-transaction
}

// ensureStoreStateLoaded lazily loads store state (header + index states) from
// FDB on first call. Subsequent calls are no-ops via sync.Once.
//
// Matches Java's build() + lazy preloadRecordStoreStateAsync() pattern:
// Java's Builder.build() returns immediately (zero reads), then the first
// operation that needs index state calls preloadRecordStoreStateAsync().
//
// MUST be called before acquiring stateMu to avoid deadlock.
//
// For Open/CreateOrOpen paths, storeHeader and indexStates are already populated
// during the open call — stateLoadOnce.Do returns immediately.
func (store *FDBRecordStore) ensureStoreStateLoaded() {
	store.stateLoadOnce.Do(func() {
		if store.indexStates != nil {
			return // already loaded (Open/CreateOrOpen path)
		}
		state, err := loadRecordStoreState(store, ExistenceCheckNone)
		if err != nil {
			// Capture the error for callers that can propagate it.
			// Default to all readable — safe when CreateOrOpen ran at
			// startup and all indexes are known to be built.
			store.stateLoadErr = err
			store.indexStates = make(map[string]IndexState)
			return
		}
		store.storeHeader = state.StoreHeader
		store.indexStates = state.IndexStates
	})
}

// ensureStoreStateLoadedErr calls ensureStoreStateLoaded and returns any
// error that occurred during the lazy load. For callers that can propagate
// errors (batch operations, validate, updateSecondaryIndexes).
func (store *FDBRecordStore) ensureStoreStateLoadedErr() error {
	store.ensureStoreStateLoaded()
	return store.stateLoadErr
}

// IsVersionChanged returns true if the metadata version changed during
// the most recent Open/CreateOrOpen (i.e., checkPossiblyRebuild detected
// that the stored version < current metadata version).
// Matches Java's FDBRecordStore.isVersionChanged().
func (store *FDBRecordStore) IsVersionChanged() bool {
	return store.versionChanged
}

// AsBuilder creates a new StoreBuilder pre-configured with this store's
// subspace, metadata, and index rebuild policy. Uses the same context.
// Matches Java's FDBRecordStore.asBuilder().
func (store *FDBRecordStore) AsBuilder() *StoreBuilder {
	return &StoreBuilder{
		context:            store.context,
		metaData:           store.metaData,
		subspace:           store.subspace,
		indexRebuildPolicy: store.indexRebuildPolicy,
		storeStateCache:    store.storeStateCache,
	}
}

// CopyBuilder creates a new StoreBuilder with this store's configuration
// but for a different context (transaction). Used to open the same store
// in a new transaction.
// Matches Java's FDBRecordStore.copyBuilder().
func (store *FDBRecordStore) CopyBuilder(newContext *FDBRecordContext) *StoreBuilder {
	return &StoreBuilder{
		context:            newContext,
		metaData:           store.metaData,
		subspace:           store.subspace,
		indexRebuildPolicy: store.indexRebuildPolicy,
		storeStateCache:    store.storeStateCache,
	}
}

// validateRecordUpdateAllowed checks if the store allows record mutations.
// Returns StoreIsLockedForRecordUpdatesError if the store header has
// StoreLockState set to FORBID_RECORD_UPDATE.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.validateRecordUpdateAllowed().
func (store *FDBRecordStore) validateRecordUpdateAllowed() error {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return fmt.Errorf("load store state: %w", err)
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.validateRecordUpdateAllowedLocked()
}

// validateRecordUpdateAllowedLocked is the same check but caller must hold stateMu.
func (store *FDBRecordStore) validateRecordUpdateAllowedLocked() error {
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
	if storeHeader.GetFormatVersion() >= formatVersionFullStoreLock {
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
	startTime := time.Now()
	recordsSubspace := store.recordsSubspace

	var sizeInfo sizeInfo
	value, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		store.metaData.IsSplitLongRecords(),
		store.omitUnsplitRecordSuffix(),
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
		return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
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

	store.context.Timer().RecordSince(EventLoadRecord, startTime)

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
	startTime := time.Now()
	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Load existing record to get size info and record data (for counting)
	var oldsizeInfo sizeInfo
	value, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		store.omitUnsplitRecordSuffix(),
		&oldsizeInfo,
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
			return false, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
		}
	}

	// Mark the inline version for cleanup by deleteSplit — but only in the modern
	// layout. In the legacy layout the version lives in the separate RecordVersionKey(8)
	// subspace, not inline, so deleteSplit must not try to clear pk+-1 (it's cleared
	// explicitly below).
	if store.metaData.IsStoreRecordVersions() {
		oldsizeInfo.VersionedInline = !store.useOldVersionFormat()
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
	deleteSplit(store.context.Transaction(), recordsSubspace, primaryKey, splitEnabled, store.omitUnsplitRecordSuffix(), &oldsizeInfo)

	// Clean up the record version. Matches Java's deleteTypedRecord.
	if store.metaData.IsStoreRecordVersions() {
		versionKey := store.versionKey(primaryKey)
		if store.useOldVersionFormat() {
			// Legacy layout: the version lives in the separate subspace and was NOT
			// touched by deleteSplit. Clear the committed value AND drop any pending
			// same-transaction mutation / local cache entry. We must do both: if the
			// record was updated earlier in THIS transaction its committed (pre-tx)
			// version is still present in FDB while a pending incomplete SET sits in the
			// mutation queue — clearing only one of them would orphan the other. (This
			// is slightly more defensive than Java's deleteTypedRecord, whose
			// incomplete-version branch drops the mutation but leaves the prior committed
			// value behind; clearing is harmless for a pure insert and avoids a stale
			// version surviving for a deleted record.)
			store.context.Transaction().Clear(versionKey)
			store.context.RemoveVersionMutation(versionKey)
			store.context.RemoveLocalVersion(versionKey)
		} else {
			// Modern layout: deleteSplit cleared the inline FDB key; just dequeue any
			// pending incomplete version mutation + local version cache entry.
			store.context.RemoveLocalVersion(versionKey)
			store.context.RemoveVersionMutation(versionKey)
		}
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

	// Record instrumentation
	timer := store.context.Timer()
	timer.RecordSince(EventDeleteRecord, startTime)
	timer.Increment(CountDeleteRecordKey)
	timer.IncrementBy(CountDeleteRecordKeyBytes, int64(oldsizeInfo.KeySize))

	if err := store.context.CheckTransactionSize(); err != nil {
		return true, err
	}

	return true, nil
}

// RecordExists checks if a record exists with the given primary key.
// Handles both split and unsplit records via SplitHelper.
//
// Java equivalent: FDBRecordStore.recordExistsAsync(Tuple primaryKey, IsolationLevel isolationLevel)
func (store *FDBRecordStore) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	recordsSubspace := store.recordsSubspace

	var tx fdb.ReadTransaction
	if isolationLevel.IsSnapshot() {
		tx = store.context.Transaction().Snapshot()
	} else {
		tx = store.context.Transaction()
	}

	return recordExistsWithSplit(tx, recordsSubspace, primaryKey, store.metaData.IsSplitLongRecords(), store.omitUnsplitRecordSuffix())
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
//   - error: RecordAlreadyExistsError, RecordDoesNotExistError, or RecordTypeChangedError based on existenceCheck
//
// Note: Version and versionstamp support will be added in Phase 2
func (store *FDBRecordStore) SaveRecordWithOptions(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	return store.saveRecordInternal(record, existenceCheck, false)
}

// saveRecordInternal implements the save logic with an explicit overrideLock parameter.
// When overrideLock is true, the FORBID_RECORD_UPDATE lock check is skipped.
// This eliminates the goroutine-unsafe overrideLock field pattern.
func (store *FDBRecordStore) saveRecordInternal(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
	overrideLock bool,
) (*FDBStoredRecord[proto.Message], error) {
	if record == nil {
		return nil, fmt.Errorf("cannot save nil record")
	}
	startTime := time.Now()
	// Extract the primary key from the record
	recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type: %s", recordTypeName)}
	}

	if recordType.PrimaryKey == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("no primary key defined for record type: %s", recordTypeName)}
	}

	// Extract primary key values using the flat evaluator (avoids [][]any alloc).
	keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, nil, record)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}
	// Reinterpret []any as tuple.Tuple ([]TupleElement where TupleElement=any).
	// Identical memory layout — no copy needed.
	primaryKey := *(*tuple.Tuple)(unsafe.Pointer(&keyValues))

	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Always load the existing record (matching Java's saveRecordAsync behavior).
	// This is needed for: existence checks, record counting, and future
	// index updates / version management.
	var oldsizeInfo sizeInfo
	oldValue, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		store.omitUnsplitRecordSuffix(),
		&oldsizeInfo,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load existing record: %w", err)
	}
	oldRecordExists := oldValue != nil

	// Cache deserialization result from type check so index update can reuse it
	// (avoids deserializing the same old record twice).
	var cachedOldRT *RecordType
	var cachedOldMsg proto.Message

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
			oldRT, oldMsg, deserErr := store.deserializeAndDiscover(oldValue)
			if deserErr != nil {
				// Propagate deserialization error. Java's loadExistingRecord()
				// deserializes before the type check — if deser fails, error propagates.
				return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
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
			// Cache for index update reuse.
			cachedOldRT = oldRT
			cachedOldMsg = oldMsg
		}
	}

	// Check lock state AFTER load and existence checks but BEFORE write,
	// matching Java's saveRecordAsync() error precedence: existence/type
	// errors take priority over lock errors.
	if !overrideLock {
		if err := store.validateRecordUpdateAllowed(); err != nil {
			return nil, err
		}
	}

	// Serialize directly into union wire format (no UnionDescriptor allocation)
	data, err := serializeUnion(record, recordType)
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
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

	// If versioning is enabled, mark old record as having an inline version so the
	// split helper clears it — but only in the modern layout. In the legacy layout
	// the version lives in the separate subspace and is overwritten in place by
	// saveRecordVersion below, so it must not be treated as inline.
	if store.metaData.IsStoreRecordVersions() && oldRecordExists {
		oldsizeInfo.VersionedInline = !store.useOldVersionFormat()
	}

	// Save using split helper (handles both split and unsplit data)
	var oldsizeInfoPtr *sizeInfo
	if oldRecordExists {
		oldsizeInfoPtr = &oldsizeInfo
	}

	var newsizeInfo sizeInfo
	if err := saveWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		data,
		splitEnabled,
		store.omitUnsplitRecordSuffix(),
		oldsizeInfoPtr,
		&newsizeInfo,
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
		if err := store.saveRecordVersion(primaryKey, version, &newsizeInfo); err != nil {
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
		KeyCount:   newsizeInfo.KeyCount,
		ValueSize:  newsizeInfo.ValueSize,
		KeySize:    newsizeInfo.KeySize,
		Split:      newsizeInfo.IsSplit,
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() {
		var oldStoredRecord *FDBStoredRecord[proto.Message]
		if oldRecordExists {
			oldRT, oldMsg := cachedOldRT, cachedOldMsg
			if oldRT == nil {
				// Not cached (type check didn't run) — deserialize now.
				var deserErr error
				oldRT, oldMsg, deserErr = store.deserializeAndDiscover(oldValue)
				if deserErr != nil {
					return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
				}
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

	// Record instrumentation
	timer := store.context.Timer()
	timer.RecordSince(EventSaveRecord, startTime)
	timer.Increment(CountSaveRecordKey)
	timer.IncrementBy(CountSaveRecordKeyBytes, int64(newsizeInfo.KeySize))
	timer.IncrementBy(CountSaveRecordValueBytes, int64(newsizeInfo.ValueSize))

	if err := store.context.CheckTransactionSize(); err != nil {
		return newStoredRecord, err
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
//   - error: RecordAlreadyExistsError if a record with the same primary key already exists
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
//   - error: RecordDoesNotExistError if no record exists with this primary key
//   - error: RecordTypeChangedError if an existing record has a different type
func (store *FDBRecordStore) UpdateRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
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
func (store *FDBRecordStore) GetIndexMaintainer(index *Index) (IndexMaintainer, error) {
	if index == nil {
		return nil, nil
	}
	return store.getIndexMaintainer(index)
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
	indexSub := store.indexSubspace(index)
	prefixKey := indexSub.Pack(prefix)
	r, err := fdb.PrefixRange(prefixKey)
	if err != nil {
		return fmt.Errorf("DeleteIndexEntriesInRange: PrefixRange(%x): %w", prefixKey, err)
	}
	store.context.Transaction().ClearRange(r)
	return nil
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
	} {
		sub := store.subspace.Sub(key)
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
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return fmt.Errorf("load store state: %w", err)
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.updateSecondaryIndexesLocked(oldRecord, newRecord)
}

// updateSecondaryIndexesLocked is the lock-free variant for use when the caller
// already holds stateMu.RLock() (e.g. SaveRecordBatch which takes it once).
func (store *FDBRecordStore) updateSecondaryIndexesLocked(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
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
// updateOneIndex updates a single index. Caller must hold stateMu (read or write).
func (store *FDBRecordStore) updateOneIndex(index *Index, oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return err
	}
	if store.getIndexStateLocked(index.Name) == IndexStateWriteOnly {
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
// Maintainers are cached for the lifetime of the store (= one transaction),
// avoiding repeated allocation of maintainer + mutation objects.
// Matches Java's FDBRecordStore.getIndexMaintainer() dispatch.
func (store *FDBRecordStore) getIndexMaintainer(index *Index) (IndexMaintainer, error) {
	if cached, ok := store.maintainerCache.Load(index.Name); ok {
		return cached.(IndexMaintainer), nil
	}

	m, err := store.createIndexMaintainer(index)
	if err != nil {
		return nil, err
	}

	store.maintainerCache.Store(index.Name, m)
	return m, nil
}

func (store *FDBRecordStore) createIndexMaintainer(index *Index) (IndexMaintainer, error) {
	idxSubspace := store.indexSubspace(index)
	tx := store.context.Transaction()
	switch index.Type {
	case IndexTypeCount:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countMutation{index: index}), nil
	case IndexTypeCountNotNull:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countNotNullMutation{index: index}), nil
	case IndexTypeCountUpdates:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countUpdatesMutation{index: index}), nil
	case IndexTypeSum:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &sumMutation{index: index}), nil
	case IndexTypeMaxEverLong:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverLongMutation{index: index, isMax: true}), nil
	case IndexTypeMinEverLong:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverLongMutation{index: index, isMax: false}), nil
	case IndexTypeMaxEverTuple:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverTupleMutation{index: index, isMax: true}), nil
	case IndexTypeMinEverTuple:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverTupleMutation{index: index, isMax: false}), nil
	case IndexTypeRank:
		secSubspace := store.indexSecondarySubspace(index)
		return newRankIndexMaintainer(index, idxSubspace, secSubspace, tx, store), nil
	case IndexTypeVersion:
		return newVersionIndexMaintainer(index, idxSubspace, tx, store.context, store), nil
	case IndexTypeMaxEverVersion:
		return newMaxEverVersionIndexMaintainer(index, idxSubspace, tx, store.context, store), nil
	case IndexTypePermutedMin:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, false), nil
	case IndexTypePermutedMax:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, true), nil
	case IndexTypeBitmapValue:
		return newBitmapValueIndexMaintainer(index, idxSubspace, tx, store), nil
	case IndexTypeText:
		secSubspace := store.indexSecondarySubspace(index)
		return newTextIndexMaintainerWithTimer(index, idxSubspace, secSubspace, tx, store, store.context.Timer())
	case IndexTypeTimeWindowLeaderboard:
		secSubspace := store.indexSecondarySubspace(index)
		return newTimeWindowLeaderboardIndexMaintainer(index, idxSubspace, secSubspace, tx, store), nil
	case IndexTypeMultidimensional:
		numDims := 2 // default; extracted from DimensionsKeyExpression at runtime
		if d := extractDimensionsExpression(index.RootExpression); d != nil {
			numDims = d.DimensionsSize
		}
		return newMultidimensionalIndexMaintainer(index, idxSubspace, tx, store, numDims), nil
	case IndexTypeVector:
		// Java's VectorIndexMaintainer stores HNSW graph data under the primary index subspace
		// (getIndexSubspace()), not the secondary subspace. Match Java's layout.
		return newVectorIndexMaintainer(index, idxSubspace, idxSubspace, tx, store)
	case IndexTypeVectorSPFresh:
		// Go-only FDB-native vector index (RFC-094); all data under the
		// primary index subspace, generation-prefixed.
		return newSPFreshIndexMaintainer(index, idxSubspace, tx, store, store.context, store.context.Timer())
	default:
		return newStandardIndexMaintainer(index, idxSubspace, tx, store), nil
	}
}

// indexStoreContext interface implementation for FDBRecordStore.
// These are called from index maintainers during updateSecondaryIndexes,
// which holds stateMu.RLock() — so they use getIndexStateLocked.
func (store *FDBRecordStore) isIndexWriteOnly(index *Index) bool {
	return store.getIndexStateLocked(index.Name) == IndexStateWriteOnly
}

func (store *FDBRecordStore) isIndexReadableUniquePending(index *Index) bool {
	return store.getIndexStateLocked(index.Name) == IndexStateReadableUniquePending
}

func (store *FDBRecordStore) addUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple, existingKey tuple.Tuple) error {
	return store.AddUniquenessViolationWithExisting(index, indexKey, primaryKey, existingKey)
}

func (store *FDBRecordStore) removeUniquenessViolations(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	return store.ResolveUniquenessViolation(index, indexKey, primaryKey)
}

// lockRegistry delegation — matches Java's LockRegistry on FDBRecordContext.
func (store *FDBRecordStore) AcquireWriteLock(key string) { store.context.locks.WriteLock(key) }
func (store *FDBRecordStore) ReleaseWriteLock(key string) { store.context.locks.WriteUnlock(key) }
func (store *FDBRecordStore) AcquireReadLock(key string)  { store.context.locks.ReadLock(key) }
func (store *FDBRecordStore) ReleaseReadLock(key string)  { store.context.locks.ReadUnlock(key) }

func (store *FDBRecordStore) isKeyInIndexBuildRange(index *Index, primaryKey tuple.Tuple) (bool, error) {
	irs := NewIndexingRangeSet(store.subspace, index)
	return irs.ContainsKey(store.context.Transaction(), primaryKey.Pack())
}

// ScanRecords scans all records in the store.
// For forward scans, continuation sets the low endpoint (start after last returned key).
// For reverse scans, continuation sets the high endpoint (end before last returned key).
// Matches Java's KeyValueCursorBase behavior.
func (store *FDBRecordStore) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	startTime := time.Now()
	lowEndpoint := EndpointTypeTreeStart
	highEndpoint := EndpointTypeTreeEnd
	if continuation != nil {
		if scanProperties.IsReverse() {
			highEndpoint = EndpointTypeContinuation
		} else {
			lowEndpoint = EndpointTypeContinuation
		}
	}
	cursor := store.ScanRecordsInRange(nil, nil, lowEndpoint, highEndpoint, continuation, scanProperties)
	store.context.Timer().RecordSince(EventScanRecords, startTime)
	return cursor
}

// ScanRecordsByType scans records filtered to a specific record type.
// When the primary key uses RecordTypeKey() as its first component (common pattern),
// this does a prefix scan on just that type's range — O(records of this type) instead
// of O(all records). Falls back to full scan + filter if RecordTypeKey is not used.
// Matches Java's RecordQuery with RecordTypeFilter.
func (store *FDBRecordStore) ScanRecordsByType(recordTypeName string, continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType != nil && recordType.PrimaryKey != nil && primaryKeyHasRecordTypePrefix(recordType.PrimaryKey) {
		// Fast path: prefix scan on the record type key range.
		// RecordTypeKey is the first component, so all records of this type
		// have PK starting with (recordTypeKey, ...).
		// Use GetRecordTypeKey() to respect explicit keys from SetRecordTypeKey().
		// Matches Java's RecordType.getRecordTypeKey().
		rtk := recordType.GetRecordTypeKey()
		lowEP := EndpointTypeRangeInclusive
		highEP := EndpointTypeRangeInclusive
		if len(continuation) > 0 {
			// Match ScanRecords: reverse scans narrow the high endpoint,
			// forward scans narrow the low endpoint.
			if scanProperties.IsReverse() {
				highEP = EndpointTypeContinuation
			} else {
				lowEP = EndpointTypeContinuation
			}
		}
		return store.ScanRecordsInRange(
			tuple.Tuple{rtk}, tuple.Tuple{rtk},
			lowEP, highEP,
			continuation, scanProperties,
		)
	}
	// Slow path: full scan + filter (no RecordTypeKey prefix).
	inner := store.ScanRecords(continuation, scanProperties)
	return &filterCursor[*FDBStoredRecord[proto.Message]]{
		inner: inner,
		predicate: func(rec *FDBStoredRecord[proto.Message]) bool {
			return rec.RecordType.Name == recordTypeName
		},
	}
}

// primaryKeyHasRecordTypePrefix checks if the key expression starts with RecordTypeKey().
func primaryKeyHasRecordTypePrefix(expr KeyExpression) bool {
	if _, ok := expr.(*RecordTypeKeyExpression); ok {
		return true
	}
	if concat, ok := expr.(*CompositeKeyExpression); ok && len(concat.expressions) > 0 {
		_, ok := concat.expressions[0].(*RecordTypeKeyExpression)
		return ok
	}
	return false
}

// KeyExpressionHasRecordTypePrefix is the exported form of the per-expression
// prefix check. Used by the SQL layer's covering-index pushdown to decide
// how to slice a primary-key tuple (first element is the record-type key
// when true).
func KeyExpressionHasRecordTypePrefix(expr KeyExpression) bool {
	return primaryKeyHasRecordTypePrefix(expr)
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
	recordsSubspace := store.recordsSubspace
	prefixLength := len(recordsSubspace.FDBKey())

	return &keyValueCursor{
		store:               store,
		low:                 low,
		high:                high,
		lowEndpoint:         lowEndpoint,
		highEndpoint:        highEndpoint,
		continuation:        continuation,
		scanProperties:      scanProperties,
		prefixLength:        prefixLength,
		startTime:           time.Now(),
		recordsSubspace:     recordsSubspace,
		storeRecordVersions: store.metaData.IsStoreRecordVersions(),
		// Bare-key layout applies ONLY when the store does not split long records;
		// a split-capable store always suffixes its keys even at format < 5. Matches
		// Java's scan logic, which gates the bare-key path on !isSplitLongRecords().
		omitUnsplitRecordSuffix: store.omitUnsplitRecordSuffix() && !store.metaData.IsSplitLongRecords(),
		useOldVersionFormat:     store.useOldVersionFormat(),
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
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getFormatVersion().
func (store *FDBRecordStore) GetFormatVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.getFormatVersionLocked()
}

func (store *FDBRecordStore) getFormatVersionLocked() int32 {
	if store.storeHeader != nil && store.storeHeader.FormatVersion != nil {
		return *store.storeHeader.FormatVersion
	}
	return 0
}

// omitUnsplitRecordSuffix reports whether records are stored WITHOUT the unsplit
// record suffix (legacy layout: the value lives at the bare key
// recordsSubspace.pack(primaryKey) with no trailing suffix). This only takes
// effect when split_long_records is false; split stores always suffix their keys.
//
// Matches Java's FDBRecordStore.omitUnsplitRecordSuffix field, as initialized in
// checkVersion(): the persisted DataStoreInfo flag is authoritative once the store
// is at format version >= SAVE_UNSPLIT_WITH_SUFFIX (5); below that the records were
// necessarily saved without a suffix, so the flag is implicitly true.
func (store *FDBRecordStore) omitUnsplitRecordSuffix() bool {
	h := store.storeHeader
	if h == nil {
		// No header yet (new store being created) — current layout.
		return false
	}
	if h.GetFormatVersion() >= formatVersionSaveUnsplitWithSuffix {
		// "Note that this depends on the property that calling get on an unset
		// boolean field results in getting back false." (Java checkVersion.)
		return h.GetOmitUnsplitRecordSuffix()
	}
	return true
}

// useOldVersionFormat reports whether record versions are stored in the legacy
// separate RecordVersionKey(8) subspace (keyed by the bare primary key) rather
// than inline adjacent to the record at recordsSubspace.pack(pk, -1).
//
// Matches Java's FDBRecordStore.useOldVersionFormat():
//
//	formatVersion < SAVE_VERSION_WITH_RECORD (6) || omitUnsplitRecordSuffix
//
// The omit clause is required because a store that omits the unsplit suffix has
// no place to put the inline version key, so versions stay in the old subspace
// even after the format version is bumped past 6.
func (store *FDBRecordStore) useOldVersionFormat() bool {
	h := store.storeHeader
	if h == nil {
		return false
	}
	return h.GetFormatVersion() < formatVersionSaveVersionWithRecord || store.omitUnsplitRecordSuffix()
}

// GetUserVersion returns the user-defined store version.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getUserVersion().
func (store *FDBRecordStore) GetUserVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader != nil && store.storeHeader.UserVersion != nil {
		return *store.storeHeader.UserVersion
	}
	return 0
}

// SetUserVersion updates the user-defined store version and writes it to FDB.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setUserVersion().
func (store *FDBRecordStore) SetUserVersion(version int32) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	store.storeHeader.UserVersion = &version
	lastUpdateTime := uint64(time.Now().UnixMilli())
	store.storeHeader.LastUpdateTime = &lastUpdateTime
	return store.writeStoreHeader(store.storeHeader)
}

// GetMetaDataVersion returns the metadata version stored in the header.
// Goroutine-safe via stateMu (read lock).
func (store *FDBRecordStore) GetMetaDataVersion() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader != nil && store.storeHeader.MetaDataversion != nil {
		return *store.storeHeader.MetaDataversion
	}
	return 0
}

// GetIncarnation returns the incarnation counter from the store header.
// Used for cross-cluster data migration versioning.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getIncarnation().
func (store *FDBRecordStore) GetIncarnation() int32 {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader != nil {
		return store.storeHeader.GetIncarnation()
	}
	return 0
}

// UpdateIncarnation atomically updates the incarnation counter using the provided
// function. The new value must be strictly greater than the current value.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.updateIncarnation().
func (store *FDBRecordStore) UpdateIncarnation(updater func(current int32) int32) error {
	if updater == nil {
		return fmt.Errorf("updater must not be nil")
	}
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
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
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getHeaderUserField().
func (store *FDBRecordStore) GetHeaderUserField(key string) []byte {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
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
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setHeaderUserField().
func (store *FDBRecordStore) SetHeaderUserField(key string, value []byte) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
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
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.clearHeaderUserField().
func (store *FDBRecordStore) ClearHeaderUserField(key string) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
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
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getRecordStoreState().
func (store *FDBRecordStore) GetRecordStoreState() *RecordStoreState {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
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
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setStoreLockStateAsync().
func (store *FDBRecordStore) SetStoreLockState(state gen.DataStoreInfo_StoreLockState_State, reason string) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
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
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.clearStoreLockStateAsync().
func (store *FDBRecordStore) ClearStoreLockState() error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	store.storeHeader.StoreLockState = nil
	return store.writeStoreHeader(store.storeHeader)
}

// ReloadRecordStoreState forces a reload of the store state from FDB.
// Useful when another transaction may have changed the state.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.loadRecordStoreStateAsync() force reload path.
func (store *FDBRecordStore) ReloadRecordStoreState() error {
	exists, header, err := store.checkStoreExists()
	if err != nil {
		return err
	}
	if !exists {
		return &RecordStoreDoesNotExistError{}
	}
	store.stateMu.Lock()
	store.storeHeader = header
	store.stateMu.Unlock()
	return store.loadIndexStates()
}

// loadStoreState loads store state via the cache or directly from FDB.
// When bypassFullStoreLockReason is set, the cache is bypassed entirely
// (matching Java's checkVersion which skips cache on lock bypass).
// Called during store Build() before concurrent access, but uses stateMu
// for consistency.
func (store *FDBRecordStore) loadStoreState(existenceCheck StoreExistenceCheck, bypassReason string) error {
	if bypassReason != "" {
		// Bypass cache when using lock bypass — need fresh state to validate lock.
		state, err := loadRecordStoreState(store, existenceCheck)
		if err != nil {
			return err
		}
		store.stateMu.Lock()
		store.storeHeader = state.StoreHeader
		store.indexStates = state.IndexStates
		store.stateMu.Unlock()
		return nil
	}

	entry, err := store.storeStateCache.Get(store, existenceCheck)
	if err != nil {
		return err
	}

	cachedState := entry.GetRecordStoreState()
	store.stateMu.Lock()
	if entry.shared {
		// Clone cached state so store mutations don't corrupt the shared cache entry.
		// Matches Java's RecordStoreState.toImmutable() which returns an unmodifiable copy.
		store.storeHeader = proto.Clone(cachedState.StoreHeader).(*gen.DataStoreInfo)
		store.indexStates = make(map[string]IndexState, len(cachedState.IndexStates))
		for k, v := range cachedState.IndexStates {
			store.indexStates[k] = v
		}
	} else {
		// Non-shared entry (e.g., PassThrough cache) — safe to use directly.
		store.storeHeader = cachedState.StoreHeader
		store.indexStates = cachedState.IndexStates
	}
	store.stateMu.Unlock()
	return nil
}

// SetStateCacheability sets whether this store's state can be cached across
// transactions. When enabled, the metadata version stamp is initialized so
// cache invalidation can work. Requires FormatVersion >= CACHEABLE_STATE (7).
// Returns true if the cacheability was changed, false if already at the desired state.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.setStateCacheabilityAsync().
func (store *FDBRecordStore) SetStateCacheability(cacheable bool) (bool, error) {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return false, &RecordStoreStateNotLoadedError{}
	}
	if store.storeHeader.GetFormatVersion() < formatVersionCacheableState {
		return false, fmt.Errorf("store format version %d does not support cacheability (requires >= %d)",
			store.storeHeader.GetFormatVersion(), formatVersionCacheableState)
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
// Goroutine-safe via stateMu (read lock).
func (store *FDBRecordStore) IsStateCacheable() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
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
	recordsSub := store.recordsSubspace
	begin, end := recordsSub.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}
	return store.context.Transaction().GetEstimatedRangeSizeBytes(kr).Get()
}

// UniquenessViolation represents a recorded uniqueness violation entry.
// Matches Java's RecordIndexUniquenessViolation.
type UniquenessViolation struct {
	IndexName   string
	IndexKey    tuple.Tuple
	PrimaryKey  tuple.Tuple
	ExistingKey tuple.Tuple // Conflicting PK stored by the other side (may be nil)
}

// ScanUniquenessViolations returns all uniqueness violations recorded for the given index.
// Violations are stored in the IndexUniquenessViolationsKey (7) subspace.
// Matches Java's StandardIndexMaintainer.scanUniquenessViolations().
func (store *FDBRecordStore) ScanUniquenessViolations(index *Index) ([]UniquenessViolation, error) {
	if index == nil {
		return nil, fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	begin, end := violationSubspace.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(kr, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan uniqueness violations for index %q: %w", index.Name, err)
	}

	var violations []UniquenessViolation
	for _, kv := range kvs {
		t, err := fastSubspaceUnpack(kv.Key, len(violationSubspace.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("unpack violation key: %w", err)
		}
		// Key format: [indexKey..., primaryKey...]
		colCount := index.RootExpression.ColumnSize()
		if len(t) > colCount {
			v := UniquenessViolation{
				IndexName:  index.Name,
				IndexKey:   tuple.Tuple(t[:colCount]),
				PrimaryKey: tuple.Tuple(t[colCount:]),
			}
			// Value contains the conflicting PK (matching Java's wire format).
			// Empty value means no cross-reference was stored.
			if len(kv.Value) > 0 {
				existingKey, err := fastUnpack(kv.Value)
				if err == nil {
					v.ExistingKey = existingKey
				}
			}
			violations = append(violations, v)
		}
	}
	return violations, nil
}

// ResolveUniquenessViolation removes a single uniqueness violation entry.
// Call this after manually resolving the conflict (e.g., deleting the duplicate record).
// Matches Java's StandardIndexMaintainer.resolveUniquenessViolation().
func (store *FDBRecordStore) ResolveUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	entryKey, err := indexEntryKey(index, indexKey, primaryKey)
	if err != nil {
		return fmt.Errorf("resolve uniqueness violation for index %q: %w", index.Name, err)
	}
	store.context.Transaction().Clear(fdb.Key(violationSubspace.Pack(entryKey)))
	return nil
}

// AddUniquenessViolation records a uniqueness violation for the given index.
// Used during WRITE_ONLY index builds when a uniqueness conflict is detected.
// Stores empty bytes as the value (no cross-reference to conflicting PK).
// Use AddUniquenessViolationWithExisting to store the conflicting PK in the value.
func (store *FDBRecordStore) AddUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	return store.AddUniquenessViolationWithExisting(index, indexKey, primaryKey, nil)
}

// AddUniquenessViolationWithExisting records a uniqueness violation with the conflicting PK.
// Matches Java's StandardIndexMaintainer.addUniquenessViolation(valueKey, primaryKey, existingKey).
// When existingKey is non-nil, its packed bytes are stored as the FDB value.
// When existingKey is nil, empty bytes are stored.
func (store *FDBRecordStore) AddUniquenessViolationWithExisting(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple, existingKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	entryKey, err := indexEntryKey(index, indexKey, primaryKey)
	if err != nil {
		return fmt.Errorf("add uniqueness violation for index %q: %w", index.Name, err)
	}
	var value []byte
	if existingKey != nil {
		value = existingKey.Pack()
	}
	store.context.Transaction().Set(fdb.Key(violationSubspace.Pack(entryKey)), value)
	return nil
}

// serializeUnion marshals a record into the UnionDescriptor wire format without
// allocating a UnionDescriptor struct. Writes: tag(fieldNum, LEN) + varint(len) + innerBytes.
// Wire-compatible with Java's UnionDescriptor serialization.
func serializeUnion(record proto.Message, recordType *RecordType) ([]byte, error) {
	if recordType.unionFieldNumber == 0 {
		return nil, fmt.Errorf("no union field number for record type: %s", recordType.Name)
	}

	// Fast path: if SizeVT is available, compute size first and allocate once.
	// This avoids the intermediate MarshalVT allocation.
	type sizer interface {
		SizeVT() int
		MarshalToSizedBufferVT([]byte) (int, error)
	}
	if sv, ok := record.(sizer); ok {
		innerSize := sv.SizeVT()
		// Allocate single buffer: tag + length varint + inner bytes
		tagSize := protowire.SizeTag(recordType.unionFieldNumber)
		lenSize := protowire.SizeBytes(innerSize) - innerSize // just the varint length
		totalSize := tagSize + lenSize + innerSize
		out := make([]byte, totalSize)

		// Write tag + length prefix
		header := protowire.AppendTag(out[:0], recordType.unionFieldNumber, protowire.BytesType)
		header = protowire.AppendVarint(header, uint64(innerSize))
		headerLen := len(header)

		// Marshal directly into the remaining buffer
		_, err := sv.MarshalToSizedBufferVT(out[headerLen : headerLen+innerSize])
		if err != nil {
			return nil, err
		}
		return out[:headerLen+innerSize], nil
	}

	// Slow path: marshal first, then wrap
	var innerBytes []byte
	var err error
	if vm, ok := record.(interface{ MarshalVT() ([]byte, error) }); ok {
		innerBytes, err = vm.MarshalVT()
	} else {
		innerBytes, err = proto.Marshal(record)
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 10+len(innerBytes))
	out = protowire.AppendTag(out, recordType.unionFieldNumber, protowire.BytesType)
	out = protowire.AppendBytes(out, innerBytes)
	return out, nil
}

// deserializeAndDiscover reads the union wire format tag to discover the record type,
// then unmarshals the inner bytes directly into the concrete message type.
// Skips allocating/parsing a full UnionDescriptor.
func (store *FDBRecordStore) deserializeAndDiscover(data []byte) (*RecordType, proto.Message, error) {
	// Scan fields to find the one matching a known record type.
	// Skips unknown fields for forward compatibility (e.g. newer proto versions).
	remaining := data
	for len(remaining) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(remaining)
		if n < 0 {
			return nil, nil, fmt.Errorf("failed to read union tag")
		}
		remaining = remaining[n:]
		if wireType != protowire.BytesType {
			// Skip non-length-delimited fields
			skip := protowire.ConsumeFieldValue(fieldNum, wireType, remaining)
			if skip < 0 {
				return nil, nil, fmt.Errorf("failed to skip field %d", fieldNum)
			}
			remaining = remaining[skip:]
			continue
		}
		innerBytes, m := protowire.ConsumeBytes(remaining)
		if m < 0 {
			return nil, nil, fmt.Errorf("failed to read field %d bytes", fieldNum)
		}
		rt := store.metaData.fieldNumberToRecordType[fieldNum]
		if rt == nil {
			// Unknown field — skip and continue scanning
			remaining = remaining[m:]
			continue
		}
		msg := rt.newMessage()
		if vu, ok := msg.(interface{ UnmarshalVT([]byte) error }); ok {
			if err := vu.UnmarshalVT(innerBytes); err != nil {
				return nil, nil, fmt.Errorf("failed to unmarshal %s: %w", rt.Name, err)
			}
		} else if err := proto.Unmarshal(innerBytes, msg); err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal %s: %w", rt.Name, err)
		}
		return rt, msg, nil
	}
	return nil, nil, fmt.Errorf("union descriptor does not contain any known record type")
}

// deserializeRecord unmarshals the inner bytes of a union-wrapped record directly
// into the known record type, skipping UnionDescriptor allocation.
func (store *FDBRecordStore) deserializeRecord(data []byte, recordType *RecordType) (proto.Message, error) {
	if recordType.unionFieldNumber == 0 {
		return nil, fmt.Errorf("no union field number for record type: %s", recordType.Name)
	}
	// Scan fields to find the target record type, skipping unknown fields.
	remaining := data
	for len(remaining) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(remaining)
		if n < 0 {
			return nil, fmt.Errorf("failed to read union tag")
		}
		remaining = remaining[n:]
		if wireType != protowire.BytesType || fieldNum != recordType.unionFieldNumber {
			skip := protowire.ConsumeFieldValue(fieldNum, wireType, remaining)
			if skip < 0 {
				return nil, fmt.Errorf("failed to skip field %d", fieldNum)
			}
			remaining = remaining[skip:]
			continue
		}
		innerBytes, m := protowire.ConsumeBytes(remaining)
		if m < 0 {
			return nil, fmt.Errorf("failed to read field %d bytes", fieldNum)
		}
		_ = remaining[m:] // consume
		msg := recordType.newMessage()
		if vu, ok := msg.(interface{ UnmarshalVT([]byte) error }); ok {
			if err := vu.UnmarshalVT(innerBytes); err != nil {
				return nil, fmt.Errorf("failed to unmarshal %s: %w", recordType.Name, err)
			}
		} else if err := proto.Unmarshal(innerBytes, msg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal %s: %w", recordType.Name, err)
		}
		return msg, nil
	}
	return nil, fmt.Errorf("union descriptor does not contain %s record", recordType.Name)
}
