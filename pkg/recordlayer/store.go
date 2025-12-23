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
	context  *FDBRecordContext
	metaData *RecordMetaData
	subspace subspace.Subspace
}

// LoadRecord loads a record by its primary key
func (store *FDBRecordStore) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[proto.Message], error) {
	recordsSubspace := store.subspace.Sub(RecordKey)
	
	// Try each record type - like Java, we always append record type index
	for _, recordType := range store.metaData.RecordTypes() {
		recordTypeIndex := recordType.GetRecordTypeIndex()
		keyTuple := append(primaryKey, recordTypeIndex)
		recordKey := recordsSubspace.Pack(keyTuple)
		
		// Get the value from FDB
		value := store.context.Transaction().Get(recordKey).MustGet()
		if value != nil {
			// Found the record! Now deserialize it
			protoMessage, err := store.deserializeRecord(value, recordType)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize record: %w", err)
			}
			
			return &FDBStoredRecord[proto.Message]{
				PrimaryKey:   primaryKey,
				RecordType:   recordType,
				Record:       protoMessage,
				ValueSize:    len(value),
				KeySize:      len(recordKey),
				Split:        false,
			}, nil
		}
	}
	
	return nil, nil // Record not found with any record type
}

// SaveRecord saves a protobuf record to the store
func (store *FDBRecordStore) SaveRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	// Extract the primary key from the record
	recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, fmt.Errorf("unknown record type: %s", recordTypeName)
	}

	if recordType.PrimaryKey == nil {
		return nil, fmt.Errorf("no primary key defined for record type: %s", recordTypeName)
	}

	// Extract primary key values using the key expression
	keyValues, err := recordType.PrimaryKey.Evaluate(record)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}

	// Create primary key tuple  
	primaryKey := make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		primaryKey[i] = v
	}

	// Like Java Record Layer, ALWAYS include record type index in the key
	// This ensures no collisions between different record types
	recordTypeIndex := recordType.GetRecordTypeIndex()
	keyTuple := append(primaryKey, recordTypeIndex)

	// Wrap the record in a UnionDescriptor like Java Record Layer does
	// This ensures compatibility with Java's serialization format
	unionRecord, err := store.wrapInUnion(record, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap record in union: %w", err)
	}

	// Serialize the union message
	data, err := proto.Marshal(unionRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal union record: %w", err)
	}

	// Construct the key for the record using the RECORD_KEY subspace
	// Key structure depends on whether record type is part of primary key
	recordsSubspace := store.subspace.Sub(RecordKey)
	recordKey := recordsSubspace.Pack(keyTuple)

	// Store the record in FDB
	store.context.Transaction().Set(recordKey, data)

	// Return the stored record
	// Note: PrimaryKey is always the user-visible key (without record type prefix)
	return &FDBStoredRecord[proto.Message]{
		PrimaryKey:   primaryKey,
		RecordType:   recordType,
		Record:       record,
		ValueSize:    len(data),
		KeySize:      len(recordKey),
		Split:        false,
	}, nil
}

// DeleteRecord deletes a record by its primary key, following Java's deleteRecordAsync semantics
// Returns true if a record was deleted, false if no record existed with that key
// Matches Java's FDBRecordStore.deleteRecordAsync(Tuple primaryKey)
func (store *FDBRecordStore) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	// First, load the existing record to see if it exists and get its type
	// This matches Java's behavior of loading the record first
	existingRecord, err := store.LoadRecord(primaryKey)
	if err != nil {
		return false, fmt.Errorf("failed to load record for deletion: %w", err)
	}
	
	// If no record exists, return false (not an error)
	if existingRecord == nil {
		return false, nil
	}
	
	// For now, we don't implement store state validation or index updates
	// TODO: Add store state validation (validateRecordUpdateAllowed)
	// TODO: Add secondary index updates (updateSecondaryIndexes)
	// TODO: Add record counting (addRecordCount with -1)
	// TODO: Add version handling for versioned records
	
	// Delete the record from all possible locations (try each record type)
	recordsSubspace := store.subspace.Sub(RecordKey)
	
	// Like Java, we need to try each record type since we don't know which one it is
	for _, recordType := range store.metaData.RecordTypes() {
		recordTypeIndex := recordType.GetRecordTypeIndex()
		keyTuple := append(primaryKey, recordTypeIndex)
		recordKey := recordsSubspace.Pack(keyTuple)
		
		// Check if this key exists before deleting
		value := store.context.Transaction().Get(recordKey).MustGet()
		if value != nil {
			// Found the record! Delete it and return true
			store.context.Transaction().Clear(recordKey)
			
			// TODO: Clear version key if versioning is enabled
			// TODO: Update secondary indexes (call updateSecondaryIndexes(oldRecord, null))
			// TODO: Decrement record count
			
			return true, nil
		}
	}
	
	// This should not happen since LoadRecord found it, but handle gracefully
	return false, nil
}

// RecordExists checks if a record exists with the given primary key.
// This is more efficient than LoadRecord when you only need to check existence.
//
// The isolationLevel parameter controls whether to use snapshot or serializable isolation:
// - IsolationLevelSnapshot: Uses snapshot reads (non-conflicting, sees consistent view at transaction start)
// - IsolationLevelSerializable: Uses serializable reads (participates in conflict detection)
//
// Java equivalent: FDBRecordStore.recordExistsAsync(Tuple primaryKey, IsolationLevel isolationLevel)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStore.java:1209
//
// Returns:
//   - bool: true if a record exists with the given primary key
//   - error: any error that occurred during the check
func (store *FDBRecordStore) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	recordsSubspace := store.subspace.Sub(RecordKey)

	// Try each record type - like LoadRecord, we check all possible record type indices
	for _, recordType := range store.metaData.RecordTypes() {
		recordTypeIndex := recordType.GetRecordTypeIndex()
		keyTuple := append(primaryKey, recordTypeIndex)
		recordKey := recordsSubspace.Pack(keyTuple)

		// Check if the key exists using appropriate isolation level
		// Java: ReadTransaction tr = isolationLevel.isSnapshot() ? ensureContextActive().snapshot() : ensureContextActive()
		var value []byte
		if isolationLevel.IsSnapshot() {
			// Use snapshot read (non-conflicting)
			value = store.context.Transaction().Snapshot().Get(recordKey).MustGet()
		} else {
			// Use normal read (participates in conflict detection)
			value = store.context.Transaction().Get(recordKey).MustGet()
		}

		// TODO: Support split records (large records >100KB) like Java's SplitHelper.keyExists()
		if value != nil {
			return true, nil
		}
	}

	return false, nil
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

	// Extract primary key values using the key expression
	keyValues, err := recordType.PrimaryKey.Evaluate(record)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}

	// Create primary key tuple
	primaryKey := make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		primaryKey[i] = v
	}

	// Perform existence checks
	if existenceCheck != RecordExistenceCheckNone {
		existingRecord, err := store.LoadRecord(primaryKey)
		if err != nil {
			return nil, fmt.Errorf("failed to check record existence: %w", err)
		}

		// ERROR_IF_EXISTS: Fail if record already exists
		if existenceCheck.ErrorIfExists() && existingRecord != nil {
			return nil, &RecordAlreadyExistsError{
				Message:    "record already exists",
				PrimaryKey: primaryKey,
			}
		}

		// ERROR_IF_NOT_EXISTS: Fail if record doesn't exist
		if existenceCheck.ErrorIfNotExists() && existingRecord == nil {
			return nil, &RecordDoesNotExistError{
				Message:    "record does not exist",
				PrimaryKey: primaryKey,
			}
		}

		// ERROR_IF_RECORD_TYPE_CHANGED: Fail if existing record has different type
		if existenceCheck.ErrorIfTypeChanged() && existingRecord != nil {
			existingTypeName := string(existingRecord.Record.ProtoReflect().Descriptor().Name())
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

	// Like Java Record Layer, ALWAYS include record type index in the key
	// This ensures no collisions between different record types
	recordTypeIndex := recordType.GetRecordTypeIndex()
	keyTuple := append(primaryKey, recordTypeIndex)

	// Wrap the record in a UnionDescriptor like Java Record Layer does
	// This ensures compatibility with Java's serialization format
	unionRecord, err := store.wrapInUnion(record, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap record in union: %w", err)
	}

	// Serialize the union message
	data, err := proto.Marshal(unionRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal union record: %w", err)
	}

	// Construct the key for the record using the RECORD_KEY subspace
	recordsSubspace := store.subspace.Sub(RecordKey)
	recordKey := recordsSubspace.Pack(keyTuple)

	// Store the record in FDB
	store.context.Transaction().Set(recordKey, data)

	// TODO: Update secondary indexes (Phase 5)
	// TODO: Update record counts (Phase 3)
	// TODO: Store version if versionstamp support is enabled (Phase 2)

	// Return the stored record
	return &FDBStoredRecord[proto.Message]{
		PrimaryKey:   primaryKey,
		RecordType:   recordType,
		Record:       record,
		ValueSize:    len(data),
		KeySize:      len(recordKey),
		Split:        false,
	}, nil
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
// The range covers all keys that start with the primary key tuple (e.g., {orderID, recordTypeIndex, UNSPLIT_RECORD}).
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

// ScanRecords scans all records in the store
func (store *FDBRecordStore) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	lowEndpoint := EndpointTypeTreeStart
	if continuation != nil {
		lowEndpoint = EndpointTypeContinuation
	}
	return store.ScanRecordsInRange(nil, nil, lowEndpoint, EndpointTypeTreeEnd, continuation, scanProperties)
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
	if b.subspace.Bytes() == nil {
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
	exists, _, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrRecordStoreDoesNotExist
	}

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
	exists, _, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	
	if !exists {
		// Create store header if it doesn't exist
		storeHeader := createStoreHeader(int32(b.metaData.Version()))
		if err := store.writeStoreHeader(storeHeader); err != nil {
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

// Users of the library should create their own typed stores for their types
// Example usage in user code:
//
// orderStore := NewTypedRecordStore[*myapp.Order](
//     baseStore,
//     recordType,
//     myUnwrapFunc,
//     myWrapFunc,
// )