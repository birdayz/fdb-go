package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

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
	if e.FirstKey != nil {
		return fmt.Sprintf("record store has no info but is not empty (first key: %x)", e.FirstKey)
	}
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

// IndexVersionKind names which of an index's two version stamps a
// IndexVersionTooNewError refers to.
type IndexVersionKind string

const (
	// IndexVersionAdded is the version at which the index was introduced.
	IndexVersionAdded IndexVersionKind = "added"
	// IndexVersionLastModified is the version at which the index definition last changed.
	IndexVersionLastModified IndexVersionKind = "last modified"
)

// IndexVersionTooNewError is returned when an index declares a version that is
// newer than the metadata version carrying it.
//
// Such metadata is not merely untidy: an index whose lastModifiedVersion exceeds
// the metadata version is permanently "new since" every store header version, so
// every subsequent version bump re-runs the rebuild decision for it and can clear
// an index a background build already populated.
//
// Matches Java's MetaDataValidator.validateIndex(), which throws MetaDataException
// for both the added-version and last-modified-version cases
// (MetaDataValidator.java:124-133).
type IndexVersionTooNewError struct {
	IndexName           string
	Kind                IndexVersionKind
	AddedVersion        int
	LastModifiedVersion int
	MetaDataVersion     int
}

func (e *IndexVersionTooNewError) Error() string {
	offending := e.AddedVersion
	if e.Kind == IndexVersionLastModified {
		offending = e.LastModifiedVersion
	}
	return fmt.Sprintf("index %q has %s version %d which is greater than the meta-data version %d",
		e.IndexName, e.Kind, offending, e.MetaDataVersion)
}

// Unwrap reports this as a metadata validation failure so that callers matching
// on *MetaDataError keep working, mirroring Java where this is a MetaDataException.
func (e *IndexVersionTooNewError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
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

// UnsupportedFeatureForFormatVersionError is returned when a version-gated store
// feature is used on a store whose header format version predates it.
//
// Java raises a RecordCoreException with LogMessageKeys.FORMAT_VERSION at each
// such site — e.g. "Store does not support setting a store lock state"
// (FDBRecordStore.java:3478/:3494), "Store does not support updating record count
// state" (:3443), "Store does not support incarnation" (:3503/:3517), "cannot
// access header user fields at current format version" (:3222). The guard exists
// because these features WRITE new store-header fields: an instance still pinned
// at the older format can open the store and will simply not understand them, so
// a lock it cannot see is a lock it will not honour.
type UnsupportedFeatureForFormatVersionError struct {
	Feature         string
	Version         int32
	RequiredVersion int32
}

func (e *UnsupportedFeatureForFormatVersionError) Error() string {
	return fmt.Sprintf("store does not support %s at format version %d (requires >= %d)",
		e.Feature, e.Version, e.RequiredVersion)
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
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
	Cause      error
}

func (e *RecordDeserializationError) Error() string {
	if e.PrimaryKey != nil {
		return fmt.Sprintf("failed to deserialize record %v: %v", e.PrimaryKey, e.Cause)
	}
	return fmt.Sprintf("failed to deserialize record: %v", e.Cause)
}

func (e *RecordDeserializationError) Unwrap() error {
	return e.Cause
}

// ContinuationParseError is returned when continuation bytes fail to parse as
// their wrapper proto. Matches Java's RecordCoreException("error parsing continuation")
// with the "raw_bytes" log info key (e.g. OrElseCursor's constructor,
// RecordCursor.fromList): a corrupt continuation is a caller error and must
// surface, never be silently treated as a fresh start.
//
// Message overrides the default "error parsing continuation" text for the Java
// classes whose RecordCoreException wording differs (ConcatCursor uses
// "Error parsing ConcatCursor continuation", DedupCursor uses
// "Error parsing continuation"). Leave empty for the common wording.
type ContinuationParseError struct {
	Message  string
	RawBytes []byte
	Cause    error
}

func (e *ContinuationParseError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "error parsing continuation"
	}
	return fmt.Sprintf("%s (raw_bytes=%x): %v", msg, e.RawBytes, e.Cause)
}

func (e *ContinuationParseError) Unwrap() error {
	return e.Cause
}

// KeyExpressionError is returned when a key expression evaluation fails.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyExpression.InvalidExpressionException.
type KeyExpressionError struct {
	Message string
}

func (e *KeyExpressionError) Error() string {
	return e.Message
}

// RecordCoreStorageError signals storage-level corruption detected while
// resolving an index entry to its base record. Matches Java's
// com.apple.foundationdb.record.RecordCoreStorageException.
//
// When a record is fetched from an index entry with IndexOrphanBehavior.ERROR
// (FDBRecordStoreBase.loadIndexEntryRecord) and the referenced record is
// missing, Java throws this exception rather than dropping the row, attaching
// LogMessageKeys.INDEX_NAME / PRIMARY_KEY / INDEX_KEY. An index entry with no
// base record means the index and the records disagree — index corruption, an
// out-of-band delete, or a maintainer bug — and both query execution
// (RecordQueryIndexPlan → FetchIndexRecords.PRIMARY_KEY) and the index-from-index
// rebuild (scanIndexRecords defaults to ERROR) use this loud path. Silently
// skipping would convert detectable corruption into quietly-fewer rows.
type RecordCoreStorageError struct {
	Message    string      // Java's RecordCoreStorageException message
	IndexName  string      // LogMessageKeys.INDEX_NAME
	PrimaryKey tuple.Tuple // LogMessageKeys.PRIMARY_KEY (nil if the entry yielded no PK)
	IndexKey   tuple.Tuple // LogMessageKeys.INDEX_KEY
}

func (e *RecordCoreStorageError) Error() string {
	return fmt.Sprintf("%s (index_name=%s, primary_key=%v, index_key=%v)",
		e.Message, e.IndexName, e.PrimaryKey, e.IndexKey)
}

// PartlyBuiltError is returned when an OnlineIndexer encounters an index that was
// partly built by another method or is blocked from continuing.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexingBase.PartlyBuiltException.
type PartlyBuiltError struct {
	IndexName     string
	SavedStamp    string // string representation of the saved stamp
	ExpectedStamp string // string representation of the expected stamp
	Message       string
}

func (e *PartlyBuiltError) Error() string {
	return fmt.Sprintf("index %q: %s (saved=%s, expected=%s)",
		e.IndexName, e.Message, e.SavedStamp, e.ExpectedStamp)
}

// IncompleteVersionstampError is returned when a tuple carrying an INCOMPLETE
// versionstamp reaches an encoder that can only write a complete one.
//
// The Java counterpart is the IllegalArgumentException thrown by
// TupleOrdering.packNullsLast (TupleOrdering.java:178) and by Tuple.pack, whose
// message this reproduces verbatim. Java THROWS; the Go port used to reach the
// same condition through tuple.Pack, which PANICS — a library-boundary crash
// where Java hands the caller a recoverable failure. Every site that can put a
// versionstamp in front of a vanilla pack returns this instead.
//
// Reachable from ordinary DDL: an index whose version column lands in the VALUE
// portion of a KeyWithValue split, or one that wraps the version in an ordering
// function. Both metadata shapes are what Java's own index generator emits (see
// IndexTest.java createVersionIndexWithVersionInValue), so the shapes cannot be
// rejected at DDL without diverging on index metadata — the failure belongs at
// the write, exactly where Java puts it.
type IncompleteVersionstampError struct {
	// Context names the encoder that refused, e.g. the index whose entry was
	// being written.
	Context string
}

func (e *IncompleteVersionstampError) Error() string {
	const msg = "Incomplete Versionstamp included in vanilla tuple pack"
	if e.Context == "" {
		return msg
	}
	return e.Context + ": " + msg
}
