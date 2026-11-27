// Portions derived from FoundationDB Record Layer (IndexMaintainer.java,
// IndexingBase.java, MetaDataValidator.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
)

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

func (*IndexNotReadableError) JavaRecordCoreException() {}

// IndexNotFoundError is returned when an index name is not found in the metadata.
// Matches Java's MetaDataException for missing indexes.
type IndexNotFoundError struct {
	IndexName string
}

func (e *IndexNotFoundError) Error() string {
	return fmt.Sprintf("index not found in metadata: %s", e.IndexName)
}

func (*IndexNotFoundError) JavaRecordCoreException() {}

// IndexNotBuiltError is returned when trying to mark an index as readable but it has
// unbuilt ranges or pending index writes remain.
type IndexNotBuiltError struct {
	IndexName     string
	PendingWrites bool
}

func (e *IndexNotBuiltError) Error() string {
	if e.PendingWrites {
		return fmt.Sprintf("index is not built: %q has pending writes", e.IndexName)
	}
	return fmt.Sprintf("index is not built: %q has unbuilt ranges", e.IndexName)
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

func (*UnknownIndexTypeError) JavaRecordCoreException() {}

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
	// Java's text (MetaDataValidator.java:124-133): the index name unquoted.
	return fmt.Sprintf("Index %s has %s version %d which is greater than the meta-data version %d",
		e.IndexName, e.Kind, offending, e.MetaDataVersion)
}

func (*IndexVersionTooNewError) JavaRecordCoreException() {}

// Unwrap reports this as a metadata validation failure so that callers matching
// on *MetaDataError keep working, mirroring Java where this is a MetaDataException.
func (e *IndexVersionTooNewError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
}

// DuplicateIndexOptionError is returned when stored index metadata lists one
// option key twice. The Java counterpart is the IllegalArgumentException Guava's
// ImmutableMap.Builder.build throws from Index.buildOptions (Index.java:253-266);
// Error reproduces its message, which names the later entry first.
type DuplicateIndexOptionError struct {
	Key string
	// First and Second are the values in stored order.
	First, Second string
}

func (e *DuplicateIndexOptionError) Error() string {
	return fmt.Sprintf("Multiple entries with same key: %s=%s and %s=%s", e.Key, e.Second, e.Key, e.First)
}

// IndexKeySizeError indicates an index entry key exceeds the FDB key size limit.
// Matches Java's FDBExceptions.FDBStoreKeySizeException.
type IndexKeySizeError struct {
	IndexName  string
	PrimaryKey tuple.Tuple
	KeySize    int
	Limit      int
}

func (e *IndexKeySizeError) Error() string {
	return fmt.Sprintf("index entry key too large for index %q (pk=%v): %d bytes exceeds limit %d",
		e.IndexName, e.PrimaryKey, e.KeySize, e.Limit)
}

func (*IndexKeySizeError) JavaRecordCoreException() {}

// IndexValueSizeError indicates an index entry value exceeds the FDB value size limit.
// Matches Java's FDBExceptions.FDBStoreValueSizeException.
type IndexValueSizeError struct {
	IndexName  string
	PrimaryKey tuple.Tuple
	ValueSize  int
	Limit      int
}

func (e *IndexValueSizeError) Error() string {
	return fmt.Sprintf("index entry value too large for index %q (pk=%v): %d bytes exceeds limit %d",
		e.IndexName, e.PrimaryKey, e.ValueSize, e.Limit)
}

func (*IndexValueSizeError) JavaRecordCoreException() {}

// RecordIndexUniquenessViolationError indicates a unique index constraint violation.
// Matches Java's RecordIndexUniquenessViolation.
type RecordIndexUniquenessViolationError struct {
	IndexName   string
	IndexKey    tuple.Tuple
	PrimaryKey  tuple.Tuple
	ExistingKey tuple.Tuple
}

func (e *RecordIndexUniquenessViolationError) Error() string {
	return fmt.Sprintf("uniqueness violation for index %q: value %v already exists for record %v (new record: %v)",
		e.IndexName, e.IndexKey, e.ExistingKey, e.PrimaryKey)
}

func (*RecordIndexUniquenessViolationError) JavaRecordCoreException() {}

// IndexMaintainerNotLinkedError is raised when an index's type is implemented
// by a package this binary does not link (see linkedIndexTypePackages). Like
// UnknownIndexTypeError it unwraps to *MetaDataError, Java's registry miss.
type IndexMaintainerNotLinkedError struct {
	IndexName string
	IndexType string
	Package   string
}

func (e *IndexMaintainerNotLinkedError) Error() string {
	return fmt.Sprintf("index %q has type %q, implemented by package %s, which this binary does not link: add import _ %q",
		e.IndexName, e.IndexType, e.Package, e.Package)
}

func (*IndexMaintainerNotLinkedError) JavaRecordCoreException() {}

// Unwrap reports this as a metadata failure.
func (e *IndexMaintainerNotLinkedError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
}

// UnsupportedRemoteFetchIndexError is Java's UnsupportedRemoteFetchIndexException:
// the index maintainer, or the transaction's client, cannot fetch remotely.
type UnsupportedRemoteFetchIndexError struct {
	Message   string
	IndexName string
}

func (e *UnsupportedRemoteFetchIndexError) Error() string { return e.Message }

func (*UnsupportedRemoteFetchIndexError) JavaRecordCoreException() {}

// UnsupportedMethodError is Java's UnsupportedMethodException.
type UnsupportedMethodError struct{ Message string }

func (e *UnsupportedMethodError) Error() string { return e.Message }

func (*UnsupportedMethodError) JavaRecordCoreException() {}

// AggregateFunctionNotSupportedError is Java's AggregateFunctionNotSupportedException
// (thrown by FDBRecordStore.evaluateAggregateFunction, FDBRecordStore.java:2409-2412)
// — no readable, queryable index over the requested record types can evaluate the
// function.
//
// It is a distinct type because callers branch on it: Java's
// IndexBuildState.loadIndexBuildStateAsync catches exactly this exception to report
// "total unknown" and lets every other failure propagate
// (IndexBuildState.java:76-82).
type AggregateFunctionNotSupportedError struct {
	Function string
	Operand  string
}

func (e *AggregateFunctionNotSupportedError) Error() string {
	return fmt.Sprintf("Aggregate function requires appropriate index: function=%s, operand=%s", e.Function, e.Operand)
}

func (*AggregateFunctionNotSupportedError) JavaRecordCoreException() {}

// TextQueryPlanError reports that an index cannot satisfy a text query,
// where Java's planText returns no plan for the index.
type TextQueryPlanError struct {
	IndexName string
	Reason    string
}

func (e *TextQueryPlanError) Error() string {
	return fmt.Sprintf("text index %q cannot satisfy the query: %s", e.IndexName, e.Reason)
}

// SynchronizedSessionLockedError is returned when a non-mutual indexer detects
// an active heartbeat from another indexer process.
// Matches Java's SynchronizedSessionLockedException, with its log keys
// (indexing/IndexingHeartbeat.java:111-115): INDEXER_ID (IndexerID, the refused
// indexer's own identity), EXISTING_INDEXER_ID, AGE_MILLISECONDS and
// TIME_LIMIT_MILLIS. ExistingInfo, the holder's heartbeat info, is a Go
// addition: Java's exception carries no info key.
type SynchronizedSessionLockedError struct {
	IndexerID         uuid.UUID
	ExistingIndexerID string
	ExistingInfo      string
	HeartbeatAgeMs    int64
	LeaseLengthMs     int64
}

func (e *SynchronizedSessionLockedError) Error() string {
	return fmt.Sprintf(
		"index build session locked by another indexer (id=%s, info=%s, age=%dms, lease=%dms)",
		e.ExistingIndexerID, e.ExistingInfo, e.HeartbeatAgeMs, e.LeaseLengthMs,
	)
}

// IndexingHeartbeatKeyError rejects legacy or malformed heartbeat keys before
// admitting UUID-only builders. Old Go builders must be stopped and fenced before
// administrative cleanup; automatically expiring these keys cannot establish that.
type IndexingHeartbeatKeyError struct {
	IndexName string
	Key       []byte
}

func (e *IndexingHeartbeatKeyError) Error() string {
	return fmt.Sprintf("incompatible indexing heartbeat key %x for index %q: fence legacy builders and clean heartbeat keys before starting UUID-only builders", e.Key, e.IndexName)
}

// IndexingValidationError matches Java's IndexingBase.ValidationException: a
// session precondition failed. It carries the log keys Java attaches at each
// throw site: the primary index (and, for "Index state is not as expected", its
// last-modified version), a disagreeing follower, and a BY_INDEX build's
// source index. The build catcher falls back from a BY_INDEX build on it.
type IndexingValidationError struct {
	Message          string
	IndexName        string
	IndexVersion     int
	IndexState       IndexState
	TargetIndexName  string
	TargetIndexState IndexState
	SourceIndexName  string
	// IndexerID is Java's INDEXER_ID log key, which the source-index validations
	// carry (validateOrThrowEx, IndexingBase.java:1171-1174); the state checks do
	// not (:203-207, :237-241), and leave it nil.
	IndexerID uuid.UUID
}

func (e *IndexingValidationError) Error() string { return e.Message }

func (*IndexingValidationError) JavaRecordCoreException() {}

// UnexpectedReadableError matches Java's IndexingBase.UnexpectedReadableException:
// a build transaction found some (AllReadable false) or all of its targets
// already published, typically by a mutual peer. Under a mutual policy the
// build catcher treats the second as success and falls back to a records scan
// on the first; any other build returns it.
type UnexpectedReadableError struct {
	AllReadable bool
	Message     string
	IndexNames  []string
	IndexStates []IndexState
}

func (e *UnexpectedReadableError) Error() string { return e.Message }

func (*UnexpectedReadableError) JavaRecordCoreException() {}

// IndexRangeClaimLostError prevents committing staged index writes when a
// target's range was already claimed. It is a failed batch, not successful
// progress or a reason to commit and jump to another fragment.
type IndexRangeClaimLostError struct {
	IndexName  string
	Begin, End []byte
}

func (e *IndexRangeClaimLostError) Error() string {
	return fmt.Sprintf("index %q build range [%x,%x) was already claimed", e.IndexName, e.Begin, e.End)
}

// SlidingWindowCorruptionError reports sliding-window bookkeeping that cannot
// be true of any consistent window — a missing boundary pointer where the count
// says there must be one, or a boundary entry key shorter than the window key.
//
// Matches Java's RecordCoreException with INDEX_NAME log info; it is a distinct
// Go type so callers can tell corrupt bookkeeping apart from an ordinary
// read failure.
type SlidingWindowCorruptionError struct {
	IndexName string
	Message   string
}

func (e *SlidingWindowCorruptionError) Error() string {
	return fmt.Sprintf("sliding window index %q: %s", e.IndexName, e.Message)
}

func (*SlidingWindowCorruptionError) JavaRecordCoreException() {}

// SlidingWindowDeleteWhereError reports a deleteRecordsWhere the sliding window
// cannot serve. Matches Java's Query.InvalidExpressionException
// ("deleteRecordsWhere not supported by index X") raised when canDeleteWhere
// returns false.
type SlidingWindowDeleteWhereError struct {
	IndexName string
	Message   string
}

func (e *SlidingWindowDeleteWhereError) Error() string {
	return fmt.Sprintf("deleteRecordsWhere not supported by index %q: %s", e.IndexName, e.Message)
}

// SPFreshSplitWindowError is the retryable conflict an SPFresh foreground
// write raises when every posting its routing reaches is SEALED by a split in
// progress. It wraps not_committed (1020), so every owner and the SQL mapping
// read it as a retryable conflict, but the attempt loop retries it without
// counting an attempt: the write re-runs until the split publishes (RFC-094
// §6), bounded by the caller's context.
type SPFreshSplitWindowError struct {
	Sealed []SPFreshSealedPosting
}

func (e *SPFreshSplitWindowError) Error() string {
	return fmt.Sprintf("SPFresh write met only sealed postings %v: %v", e.Sealed, e.Unwrap())
}

func (e *SPFreshSplitWindowError) Unwrap() error { return fdb.Error{Code: 1020} }

// SPFreshStalledSealError fails a call whose SPFresh write met the same sealed
// postings on spfreshStalledSealBound consecutive retries under a simulated
// environment, where no second actor can take the stalled lease over. It is
// not retriable, so a simulation that stalls a seal fails instead of hanging.
type SPFreshStalledSealError struct {
	Sealed  []SPFreshSealedPosting
	Retries int
}

func (e *SPFreshStalledSealError) Error() string {
	return fmt.Sprintf("SPFresh write stalled: postings %v stayed sealed across %d retries", e.Sealed, e.Retries)
}
