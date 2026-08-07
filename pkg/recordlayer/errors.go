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

// UnknownIndexTypeError is raised when no index maintainer implements an index's
// type. It is the port of Java's registry miss:
// IndexMaintainerFactoryRegistryImpl.getIndexMaintainerFactory looks the type up
// and throws MetaDataException("Unknown index type for " + index) when the lookup
// returns null (IndexMaintainerFactoryRegistryImpl.java:78-82).
//
// It FAILS CLOSED, and the reason is a wire-format one rather than a tidiness
// one. Go used to answer an unrecognised type with the STANDARD maintainer — the
// VALUE-index one. An index whose type this build does not implement was
// therefore not rejected: it was silently maintained as a value index, writing
// value-index key bytes into that index's own subspace. Nothing surfaces at write
// time, because a value index is perfectly happy to serve those writes. The
// damage appears when a build that DOES implement the type reads that subspace
// and finds another format's entries, or when Java reads it and finds entries its
// maintainer never wrote. An index this process cannot maintain must stop the
// write, not receive a guess.
//
// Unwraps to *MetaDataError so `errors.As` for the general metadata failure
// matches — the Go equivalent of Java's `catch (MetaDataException)`, which is
// what callers of the registry actually catch — while a caller that wants the
// index identity can match this type directly.
type UnknownIndexTypeError struct {
	IndexName string
	IndexType string
}

func (e *UnknownIndexTypeError) Error() string {
	return fmt.Sprintf("Unknown index type %q for index %q", e.IndexType, e.IndexName)
}

// Unwrap reports this as a metadata failure. Java's exception IS a
// MetaDataException; in Go the identity fields need their own struct, so the
// relationship Java gets from inheritance is spelled out here.
func (e *UnknownIndexTypeError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
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

// FoundSplitOutOfOrderError is raised when a split record's segments are present
// but not in sequence — segment N+1 was expected and something else was found.
//
// Ports Java's SplitHelper.FoundSplitOutOfOrderException
// (SplitHelper.java:1225-1231), which extends RecordCoreStorageException and
// carries LogMessageKeys.SPLIT_EXPECTED / SPLIT_FOUND. Java additionally attaches
// KEY_TUPLE and SUBSPACE at the throw site (SplitHelper.java:826-828); KeyTuple
// carries the first of those, and the subspace is implicit in it here.
//
// It is a distinct type from FoundSplitWithoutStartError on purpose, because the
// two describe different damage: this one says the record's pieces are all
// there but mis-sequenced, which points at a writer that interleaved or a range
// that spans two records. A caller that wants to tell "corrupt but complete"
// from "truncated" cannot do it from a formatted string.
type FoundSplitOutOfOrderError struct {
	Expected int64       // LogMessageKeys.SPLIT_EXPECTED
	Found    int64       // LogMessageKeys.SPLIT_FOUND
	KeyTuple tuple.Tuple // LogMessageKeys.KEY_TUPLE — the key whose suffix broke the sequence
}

func (e *FoundSplitOutOfOrderError) Error() string {
	return fmt.Sprintf("Split record segments out of order (split_expected=%d, split_found=%d, key_tuple=%v)",
		e.Expected, e.Found, e.KeyTuple)
}

// Unwrap reports this as storage corruption. Java's exception IS a
// RecordCoreStorageException; Go spells that relationship out so a caller
// matching the general storage-corruption type keeps working, the way
// `catch (RecordCoreStorageException)` does.
func (e *FoundSplitOutOfOrderError) Unwrap() error {
	return &RecordCoreStorageError{Message: "Split record segments out of order"}
}

// FoundSplitWithoutStartError is raised when a split record's continuation
// segments are found with no start segment — the record is truncated at the
// front, not merely mis-ordered.
//
// Ports Java's SplitHelper.FoundSplitWithoutStartException
// (SplitHelper.java:1213-1219), carrying LogMessageKeys.SPLIT_NEXT_INDEX and
// SPLIT_REVERSE, plus KEY_TUPLE from the throw sites.
//
// Java chooses between this and FoundSplitOutOfOrderException by whether any
// start-or-later segment has already been seen — `lastIndex >= START_SPLIT_RECORD`
// picks out-of-order, otherwise without-start (SplitHelper.java:824-835). Go
// raised one untyped error for both, so a truncated record and a scrambled one
// were indistinguishable to every caller and to every log.
//
// Reverse matters for reading the report: under a reverse scan the segments
// arrive highest-first, so "no start yet" is the expected intermediate state
// rather than evidence of damage, and Java records which direction produced the
// judgement.
type FoundSplitWithoutStartError struct {
	NextIndex int64       // LogMessageKeys.SPLIT_NEXT_INDEX — the segment found instead of the start
	Reverse   bool        // LogMessageKeys.SPLIT_REVERSE — direction of the scan that found it
	KeyTuple  tuple.Tuple // LogMessageKeys.KEY_TUPLE
}

func (e *FoundSplitWithoutStartError) Error() string {
	return fmt.Sprintf("Found split record without start (next_index=%d, reverse=%t, key_tuple=%v)",
		e.NextIndex, e.Reverse, e.KeyTuple)
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
