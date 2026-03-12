package recordlayer

import "fmt"

// Phase 1: Store existence errors (replace sentinels from store.go)

// RecordStoreAlreadyExistsError is returned when attempting to create a store that already exists.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordStoreAlreadyExistsException.
type RecordStoreAlreadyExistsError struct{}

func (e *RecordStoreAlreadyExistsError) Error() string {
	return "record store already exists"
}

// RecordStoreDoesNotExistError is returned when attempting to open a store that does not exist.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordStoreDoesNotExistException.
type RecordStoreDoesNotExistError struct{}

func (e *RecordStoreDoesNotExistError) Error() string {
	return "record store does not exist"
}

// RecordStoreNoInfoButNotEmptyError is returned when a store subspace has data
// but no valid store header (StoreInfoKey).
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordStoreNoInfoAndNotEmptyException.
type RecordStoreNoInfoButNotEmptyError struct {
	FirstKey []byte // First key found in the subspace (Java's LogMessageKeys.KEY)
}

func (e *RecordStoreNoInfoButNotEmptyError) Error() string {
	return "record store has no info but is not empty"
}

// RecordStoreStateNotLoadedError is returned when store operations are called
// before the store state has been loaded via Create/Open/CreateOrOpen.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.UninitializedRecordStoreException.
type RecordStoreStateNotLoadedError struct{}

func (e *RecordStoreStateNotLoadedError) Error() string {
	return "record store state not loaded"
}

// Phase 1: Index errors (replace sentinels from index_state.go)

// IndexNotReadableError is returned when trying to scan an index that is not in a readable state.
// Matches Java's com.apple.foundationdb.record.ScanNonReadableIndexException.
type IndexNotReadableError struct {
	IndexName    string
	CurrentState IndexState
}

func (e *IndexNotReadableError) Error() string {
	return fmt.Sprintf("index is not readable: %s is %s", e.IndexName, e.CurrentState)
}

// IndexNotFoundError is returned when an index name is not found in the metadata.
// Matches Java's MetaDataException for missing indexes.
type IndexNotFoundError struct {
	IndexName string
}

func (e *IndexNotFoundError) Error() string {
	return fmt.Sprintf("index not found in metadata: %s", e.IndexName)
}

// IndexNotBuiltError is returned when trying to mark an index as readable but it has
// unbuilt ranges remaining in its range set.
type IndexNotBuiltError struct {
	IndexName string
}

func (e *IndexNotBuiltError) Error() string {
	return fmt.Sprintf("index is not built: %q has unbuilt ranges", e.IndexName)
}

// Phase 2: Missing error types for implemented features

// MetaDataError is returned for metadata validation failures.
// Matches Java's com.apple.foundationdb.record.metadata.MetaDataException.
type MetaDataError struct {
	Message string
}

func (e *MetaDataError) Error() string {
	return e.Message
}

// UnsupportedFormatVersionError is returned when a store header contains a format
// version higher than the maximum version this code supports.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.UnsupportedFormatVersionException.
type UnsupportedFormatVersionError struct {
	Version    int32
	MaxVersion int32
}

func (e *UnsupportedFormatVersionError) Error() string {
	return fmt.Sprintf("unsupported format version %d (max supported: %d)", e.Version, e.MaxVersion)
}

// RecordSerializationError is returned when a record fails to serialize (marshal) to protobuf.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordSerializationException.
type RecordSerializationError struct {
	Cause error
}

func (e *RecordSerializationError) Error() string {
	return fmt.Sprintf("failed to serialize record: %v", e.Cause)
}

func (e *RecordSerializationError) Unwrap() error {
	return e.Cause
}

// RecordDeserializationError is returned when a record fails to deserialize (unmarshal) from protobuf.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordDeserializationException.
type RecordDeserializationError struct {
	Cause error
}

func (e *RecordDeserializationError) Error() string {
	return fmt.Sprintf("failed to deserialize record: %v", e.Cause)
}

func (e *RecordDeserializationError) Unwrap() error {
	return e.Cause
}
