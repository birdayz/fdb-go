// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// SplitHelper.java, IndexingBase.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"github.com/google/uuid"
)

// Phase 1: Store existence errors (replace sentinels from store.go)

// RecordStoreAlreadyExistsError is returned when attempting to create a store that already exists.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordStoreAlreadyExistsException.
type RecordStoreAlreadyExistsError struct{}

func (e *RecordStoreAlreadyExistsError) Error() string {
	return "record store already exists"
}

func (*RecordStoreAlreadyExistsError) JavaRecordCoreException() {}

// RecordStoreDoesNotExistError is returned when attempting to open a store that does not exist.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordStoreDoesNotExistException.
type RecordStoreDoesNotExistError struct{}

func (e *RecordStoreDoesNotExistError) Error() string {
	return "record store does not exist"
}

func (*RecordStoreDoesNotExistError) JavaRecordCoreException() {}

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

func (*RecordStoreNoInfoButNotEmptyError) JavaRecordCoreException() {}

// RecordStoreStateNotLoadedError is returned when store operations are called
// before the store state has been loaded via Create/Open/CreateOrOpen.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.UninitializedRecordStoreException.
type RecordStoreStateNotLoadedError struct{}

func (e *RecordStoreStateNotLoadedError) Error() string {
	return "record store state not loaded"
}

func (*RecordStoreStateNotLoadedError) JavaRecordCoreException() {}

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

func (*StoreIsLockedForRecordUpdatesError) JavaRecordCoreException() {}

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

func (*StoreIsFullyLockedError) JavaRecordCoreException() {}

// UnknownStoreLockStateError is returned when a store has an unrecognized lock state
// at FormatVersion >= FULL_STORE_LOCK (14). This prevents opening stores with
// lock states added by newer versions that we don't understand.
// Matches Java's com.apple.foundationdb.record.UnknownStoreLockStateException.
type UnknownStoreLockStateError struct {
	// LockStateValue is the state as read: UNSPECIFIED (0) for a stored
	// number the enum does not declare, which Java's parse leaves in the
	// unknown fields and reads as the default.
	LockStateValue int32
	// UnknownFields is the lock-state message's unknown fields, Java's
	// LogMessageKeys.VALUE (UnknownStoreLockStateException.java:49): an
	// undeclared state number is there.
	UnknownFields []byte
}

func (e *UnknownStoreLockStateError) Error() string {
	return fmt.Sprintf("Store has unknown lock state: %d", e.LockStateValue)
}

func (*UnknownStoreLockStateError) JavaRecordCoreException() {}

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

func (*StaleMetaDataVersionError) JavaRecordCoreException() {}

// LegacyKeySpacePrefixError refuses a silent switch from an earlier Go path.
type LegacyKeySpacePrefixError struct {
	LegacyPrefix []byte
	FlatPrefix   []byte
}

func (e *LegacyKeySpacePrefixError) Error() string {
	return fmt.Sprintf("legacy nested KeySpace prefix %x still contains data; explicit legacy access or cutover to %x required", e.LegacyPrefix, e.FlatPrefix)
}

// RecordAlreadyExistsError is returned when attempting to insert a record that already exists.
// Includes structured context matching Java's RecordAlreadyExistsException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordAlreadyExistsException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordAlreadyExistsException.java
// Java includes: LogMessageKeys.PRIMARY_KEY
type RecordAlreadyExistsError struct {
	Message    string
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
}

func (e *RecordAlreadyExistsError) Error() string {
	return e.Message
}

func (*RecordAlreadyExistsError) JavaRecordCoreException() {}

// RecordDoesNotExistError is returned when attempting to update a record that does not exist.
// Includes structured context matching Java's RecordDoesNotExistException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordDoesNotExistException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordDoesNotExistException.java
// Java includes: LogMessageKeys.PRIMARY_KEY
type RecordDoesNotExistError struct {
	Message    string
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
}

func (e *RecordDoesNotExistError) Error() string {
	return e.Message
}

func (*RecordDoesNotExistError) JavaRecordCoreException() {}

// RecordTypeChangedError is returned when attempting to update a record but its type has changed.
// Includes structured context matching Java's RecordTypeChangedException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordTypeChangedException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordTypeChangedException.java
// Java includes: LogMessageKeys.PRIMARY_KEY, LogMessageKeys.ACTUAL_TYPE, LogMessageKeys.EXPECTED_TYPE
type RecordTypeChangedError struct {
	Message      string
	PrimaryKey   any // tuple.Tuple, but using any to avoid import cycle
	ActualType   string
	ExpectedType string
}

func (e *RecordTypeChangedError) Error() string {
	return e.Message
}

func (*RecordTypeChangedError) JavaRecordCoreException() {}

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

func (*FoundSplitOutOfOrderError) JavaRecordCoreException() {}

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

func (*FoundSplitWithoutStartError) JavaRecordCoreException() {}

// PartlyBuiltError is returned when an OnlineIndexer encounters an index that was
// partly built by another method or is blocked from continuing.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexingBase.PartlyBuiltException.
type PartlyBuiltError struct {
	IndexName     string
	SavedStamp    string // string representation of the saved stamp
	ExpectedStamp string // string representation of the expected stamp
	Message       string
	// Saved and Expected are the stamps themselves (Java getSavedStamp and
	// getExpectedStamp). The build catcher resumes the saved stamp's method.
	Saved    *metadatapb.IndexBuildIndexingStamp
	Expected *metadatapb.IndexBuildIndexingStamp
	// IndexerID is the refusing indexer's identity, Java's INDEXER_ID log key
	// (IndexingBase.java:604, :1011-1012).
	IndexerID uuid.UUID
	// IndexVersion is the index's last-modified version, Java's INDEX_VERSION
	// log key (IndexingBase.java:1291).
	IndexVersion int
}

func (e *PartlyBuiltError) Error() string {
	return fmt.Sprintf("index %q: %s (saved=%s, expected=%s)",
		e.IndexName, e.Message, e.SavedStamp, e.ExpectedStamp)
}

func (*PartlyBuiltError) JavaRecordCoreException() {}

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
// Reachable from a VERSION index whose version lands in the VALUE portion of a
// KeyWithValue split (IndexTest.createVersionIndexWithVersionInValue), or from
// ordering functions over versions in legal VALUE indexes and primary keys.
// A VERSION index rooted at an ordering function is instead refused at metadata
// validation because functions declare zero version columns.
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

// RecordCountKeySizeMismatchError is Java's
// recordCoreException("key and value are not the same size") thrown by
// getSnapshotRecordCount (FDBRecordStore.java:2295-2297) when the evaluated value
// has a different number of columns than the key expression it is supposed to be a
// value of.
//
// It is a typed error rather than a plain message because the two sizes are the
// whole diagnosis: the caller either passed a value for a different count key or
// truncated one, and the alternative to failing is answering from a slot nothing
// ever writes — a confident, wrong 0.
type RecordCountKeySizeMismatchError struct {
	KeyColumnSize int
	ValueSize     int
}

func (e *RecordCountKeySizeMismatchError) Error() string {
	return fmt.Sprintf("key and value are not the same size: keyColumnSize=%d, valueSize=%d",
		e.KeyColumnSize, e.ValueSize)
}

// LocatableResolverLockedError is Java's LocatableResolverLockedException.
type LocatableResolverLockedError struct{ Message string }

func (e *LocatableResolverLockedError) Error() string { return e.Message }

// UnknownOrElseCursorStateError is returned when an OrElse continuation carries a
// state value outside the known OrElseContinuation_State range. Matches Java's
// OrElseCursor.UnknownOrElseCursorStateException.
type UnknownOrElseCursorStateError struct{}

func (e *UnknownOrElseCursorStateError) Error() string {
	return "unknown state for OrElseCursor"
}

func (*UnknownOrElseCursorStateError) JavaRecordCoreException() {}
