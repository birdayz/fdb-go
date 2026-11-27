// Portions derived from FoundationDB Record Layer (RecordCoreException.java,
// RecordCoreStorageException.java, RecordCoreArgumentException.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"fdb.dev/internal/recordlayer/protoname"
	"fdb.dev/internal/recordlayer/textcmp"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RecordCoreException is implemented by every error type porting a Java
// RecordCoreException subclass. The relational layer gives one that none of
// its specific arms claims ErrorCode.UNKNOWN, as ExceptionUtil does.
type RecordCoreException interface {
	error
	JavaRecordCoreException()
}

// boundaryErrorWrapper marks an internal wrapper that adapts an error from a
// lower-level package (such as javanum or protoname) into a recordlayer error
// type while preserving the outer message and original cause chain.
// OutermostJavaError skips it so classification sees the recordlayer error.
type boundaryErrorWrapper interface {
	boundaryWrapper()
}

type boundaryWrappedError struct {
	outer error
	inner error
}

func (e *boundaryWrappedError) Error() string { return e.outer.Error() }

func (e *boundaryWrappedError) Unwrap() []error { return []error{e.inner, e.outer} }

func (*boundaryWrappedError) boundaryWrapper() {}

// OutermostJavaError is the first error of this package (or of protoname for
// leaf callers) on err's chain of wrappers, following the first of
// several; nil when there is none.
func OutermostJavaError(err error) error {
	for e := err; e != nil; {
		if _, skip := e.(boundaryErrorWrapper); !skip {
			if t := reflect.TypeOf(e); t.Kind() == reflect.Pointer && javaErrorPkgPaths[t.Elem().PkgPath()] {
				return e
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			e = u.Unwrap()
		case interface{ Unwrap() []error }:
			if errs := u.Unwrap(); len(errs) > 0 {
				e = errs[0]
			} else {
				e = nil
			}
		default:
			e = nil
		}
	}
	return nil
}

// javaErrorPkgPaths are the packages whose error types are Java exception
// classes: this one (including IllegalArgumentError and IllegalStateError now
// owned by recordlayer, matching Java exception hierarchy), and protoname for
// leaf callers.
var javaErrorPkgPaths = map[string]bool{
	reflect.TypeOf(MetaDataError{}).PkgPath():              true,
	reflect.TypeOf(protoname.InvalidNameError{}).PkgPath(): true,
	reflect.TypeOf(textcmp.MetaDataError{}).PkgPath():      true,
}

// RecordCoreMessages is the message of every RecordCoreError in err's tree, in
// errors.As order (depth first, each error before what it wraps), each once:
// the Java RecordCoreExceptions an error is, and wraps, as their messages. A
// test asserts with it which exceptions a refusal carries, where errors.As
// finds only the first.
func RecordCoreMessages(err error) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if core, ok := e.(*RecordCoreError); ok && !seen[core.Message] {
			seen[core.Message] = true
			out = append(out, core.Message)
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		}
	}
	walk(err)
	return out
}

// RecordCoreError carries Java's base RecordCoreException diagnostics and cause.
type RecordCoreError struct {
	IndexOption string
	Message     string
	IndexName   string
	Cause       error
}

func (e *RecordCoreError) Error() string { return e.Message }
func (e *RecordCoreError) Unwrap() error { return e.Cause }

func (*RecordCoreError) JavaRecordCoreException() {}

// RecordCoreInternalError reports an internal invariant violation, matching
// Java's RecordCoreInternalException.
type RecordCoreInternalError struct {
	Message string
}

func (e *RecordCoreInternalError) Error() string { return e.Message }

func (*RecordCoreInternalError) JavaRecordCoreException() {}

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
	Message       string      // Java's RecordCoreStorageException message
	IndexName     string      // LogMessageKeys.INDEX_NAME
	PrimaryKey    tuple.Tuple // LogMessageKeys.PRIMARY_KEY (nil if the entry yielded no PK)
	IndexKey      tuple.Tuple // LogMessageKeys.INDEX_KEY
	KeyTuple      tuple.Tuple // LogMessageKeys.KEY_TUPLE
	Version       *int32      // LogMessageKeys.VERSION (reader version)
	StoredVersion *int32      // LogMessageKeys.STORED_VERSION
	ExpectedType  string      // Bound protobuf full name, the language-independent message identity
	ActualType    string      // LogMessageKeys.ACTUAL_TYPE (stored Any type URL)
	Cause         error       // the exception's cause, e.g. the store header's parse error
}

func (e *RecordCoreStorageError) Error() string {
	if e.Cause != nil && e.KeyTuple == nil && e.IndexName == "" {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	if e.KeyTuple != nil {
		message := fmt.Sprintf("%s (key_tuple=%v", e.Message, e.KeyTuple)
		if e.Version != nil && e.StoredVersion != nil {
			message += fmt.Sprintf(", version=%d, stored_version=%d", *e.Version, *e.StoredVersion)
		}
		if e.ExpectedType != "" {
			message += fmt.Sprintf(", expected_type=%s, actual_type=%s", e.ExpectedType, e.ActualType)
		}
		return message + ")"
	}
	return fmt.Sprintf("%s (index_name=%s, primary_key=%v, index_key=%v)",
		e.Message, e.IndexName, e.PrimaryKey, e.IndexKey)
}

func (*RecordCoreStorageError) JavaRecordCoreException() {}

// Unwrap returns the cause.
func (e *RecordCoreStorageError) Unwrap() error { return e.Cause }

// RecordContextNotActiveError is Java's RecordContextNotActiveException, a
// RecordCoreStorageException: the context's transaction is no longer active.
// A context is deactivated by its commit whatever the outcome, and a context a
// transaction route (FDBDatabase.Run and its variants, the runner) hands its
// body is the route's to commit, so a commit there is refused before anything
// commits. errors.As finds it as itself and as a *RecordCoreStorageError.
type RecordContextNotActiveError struct{ Message string }

func (e *RecordContextNotActiveError) Error() string { return e.Message }

func (*RecordContextNotActiveError) JavaRecordCoreException() {}

// Unwrap exposes the RecordCoreStorageException superclass.
func (e *RecordContextNotActiveError) Unwrap() error {
	return &RecordCoreStorageError{Message: e.Message}
}

// errTransactionNotActive is FDBRecordContext.ensureActive's refusal.
func errTransactionNotActive() error {
	return &RecordContextNotActiveError{Message: "Transaction is no longer active."}
}

// RecordCoreArgumentError reports an invalid argument with its context.
// Matches Java's RecordCoreArgumentException; the fields are the log keys the
// raising sites attach (SCAN_TYPE, INDEX_NAME, SUBSPACE_KEY).
type RecordCoreArgumentError struct {
	Message   string
	ScanType  IndexScanType
	IndexName string
	// SubspaceKey is the offending subspace key of a subspace-key refusal
	// (Index.normalizeSubspaceKey, FormerIndex's constructor;
	// LogMessageKeys.SUBSPACE_KEY), and HasSubspaceKey says the site attached
	// one: the offending key is often nil, so nil cannot mean "not attached".
	SubspaceKey    any
	HasSubspaceKey bool
	// Key and Subspace are the raw bytes attached by SplitHelper.unpackKey.
	Key      []byte
	Subspace []byte
	// Plan is the refused plan's class name (LogMessageKeys.PLAN).
	Plan string
	// ClientKnobName, ClientKnobValue and Expected are a refused client knob's
	// log keys (CLIENT_KNOB_NAME, CLIENT_KNOB_VALUE, EXPECTED).
	ClientKnobName  string
	ClientKnobValue string
	Expected        string
	// Cause is the exception Java's constructor chains (the key store's
	// IOException / GeneralSecurityException in KeyStoreSerializationKeyManager).
	Cause error
}

// Unwrap exposes the chained cause to errors.Is / errors.As.
func (e *RecordCoreArgumentError) Unwrap() error { return e.Cause }

// Error renders the message and, in parentheses, only the fields its site set.
func (e *RecordCoreArgumentError) Error() string {
	var fields []string
	if e.ScanType != "" {
		fields = append(fields, "scanType="+string(e.ScanType))
	}
	if e.IndexName != "" {
		fields = append(fields, fmt.Sprintf("index=%q", e.IndexName))
	}
	if e.HasSubspaceKey {
		fields = append(fields, fmt.Sprintf("subspace_key=%v", e.SubspaceKey))
	}
	if e.Key != nil {
		fields = append(fields, fmt.Sprintf("key=%x", e.Key))
	}
	if e.Subspace != nil {
		fields = append(fields, fmt.Sprintf("subspace=%x", e.Subspace))
	}
	if e.Plan != "" {
		fields = append(fields, "plan="+e.Plan)
	}
	if e.ClientKnobName != "" {
		fields = append(fields, fmt.Sprintf("client_knob_name=%q", e.ClientKnobName))
	}
	if e.Expected != "" {
		fields = append(fields, fmt.Sprintf("client_knob_value=%q", e.ClientKnobValue), "expected="+e.Expected)
	}
	msg := e.Message
	if len(fields) > 0 {
		msg += " (" + strings.Join(fields, ", ") + ")"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (*RecordCoreArgumentError) JavaRecordCoreException() {}

// RecordCoreRetriableTransactionError is Java's
// RecordCoreRetriableTransactionException: a failure the transaction runner
// retries like a retryable FDB error.
type RecordCoreRetriableTransactionError struct {
	Message string
	Cause   error
}

func (e *RecordCoreRetriableTransactionError) Error() string { return e.Message }

func (e *RecordCoreRetriableTransactionError) Unwrap() error { return e.Cause }

func (*RecordCoreRetriableTransactionError) JavaRecordCoreException() {}

// UnsupportedOperationError corresponds to Java's UnsupportedOperationException.
type UnsupportedOperationError struct{ Message string }

func (e *UnsupportedOperationError) Error() string { return e.Message }

// ClassCastError corresponds to Java's ClassCastException, e.g. a vector
// index reading a value column that is not bytes (Tuple.getBytes).
type ClassCastError struct{ Message string }

func (e *ClassCastError) Error() string { return e.Message }

// IndexOutOfBoundsError corresponds to Java's IndexOutOfBoundsException, e.g.
// a vector index entry without a value column or with an unknown vector type.
type IndexOutOfBoundsError struct{ Message string }

func (e *IndexOutOfBoundsError) Error() string { return e.Message }

// NoSuchElementError is Java's NoSuchElementException.
type NoSuchElementError struct{ Message string }

func (e *NoSuchElementError) Error() string { return e.Message }

// NumberFormatError is Java's NumberFormatException, raised by
// Integer.parseInt on an index option that is not a decimal int. Java's class
// extends IllegalArgumentException, so errors.As matches it as an
// *IllegalArgumentError too.
type NumberFormatError struct {
	// Input is the value Integer.parseInt or Double.parseDouble refused
	// (Double.parseDouble's trimmed).
	Input string
	// Text, when set, is the exception's own message instead of
	// forInputString's: Double.parseDouble's "empty String" for a value that
	// trims to nothing and "multiple points" for a second decimal point in the
	// significand (FloatingDecimal.readJavaFormatString).
	Text string
}

// Error is NumberFormatException.forInputString's text for radix 10, or Text.
func (e *NumberFormatError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return `For input string: "` + e.Input + `"`
}

// As lets errors.As match a NumberFormatError as the IllegalArgumentError
// its Java class extends.
func (e *NumberFormatError) As(target any) bool {
	if t, ok := target.(**IllegalArgumentError); ok {
		*t = &IllegalArgumentError{Message: e.Error()}
		return true
	}
	return false
}

// TagValidationError reports a tag set rejected by the record layer's limits.
// Java throws IllegalArgumentException with these exact messages.
type TagValidationError struct {
	Message string
	// Tag is the offending tag for a length violation, empty for a count violation.
	Tag string
}

func (e *TagValidationError) Error() string { return e.Message }

// RunnerClosedError corresponds to FDBDatabaseRunner.RunnerClosed.
type RunnerClosedError struct{}

func (*RunnerClosedError) Error() string { return "runner is closed" }

func (*RunnerClosedError) JavaRecordCoreException() {}

// BackendCapabilityError is returned when an operation is not supported on the
// configured fdb backend. The libfdb_c escape hatch (RFC-109) drives the
// Run / RunRead gold path, standalone transactions (CreateWritableTransaction —
// used by the FDBDatabaseRunner and SQL BeginTx), and LocalityGetBoundaryKeys
// (online MUTUAL indexing) all through backend interfaces, so those work on
// libfdb_c. Only the pure-Go-typed CreateTransaction (which returns the concrete
// fdb.Transaction) is pure-Go-only in v1; it — and any operation a custom
// transactor genuinely can't provide — fail fast with this error.
type BackendCapabilityError struct {
	Op string // the unavailable operation, e.g. "CreateTransaction"
}

func (e *BackendCapabilityError) Error() string {
	return fmt.Sprintf("recordlayer: %s is not supported on this fdb backend "+
		"(pure-Go-only; the libfdb_c escape hatch covers the Run/RunRead path)", e.Op)
}

// TransactionSizeExceededError is returned when the approximate transaction
// size exceeds the configured error threshold. Callers should commit the
// current transaction and start a new one.
type TransactionSizeExceededError struct {
	CurrentBytes int64
	LimitBytes   int64
}

func (e *TransactionSizeExceededError) Error() string {
	return fmt.Sprintf("transaction size %d bytes exceeds limit %d bytes", e.CurrentBytes, e.LimitBytes)
}

// MemoryLimitExceededError is returned when a statement's accounted in-memory
// buffering exceeds the statement-wide memory byte budget (RFC-130). It is a
// per-statement resource-limit error in the same family as the scan/byte/time
// limits — the relational layer maps it to SQLSTATE 54F01
// (ErrCodeExecutionLimitReached), matching Java's reuse of that SQLSTATE for
// resource limits (there is no memory-specific SQLSTATE in Java either).
type MemoryLimitExceededError struct {
	// Used is the running memory total (in bytes) at the point the budget
	// was breached — i.e. the would-be total INCLUDING the charge that
	// tripped it.
	Used int64
	// Limit is the configured statement-wide memory byte budget.
	Limit int64
}

func (e *MemoryLimitExceededError) Error() string {
	return fmt.Sprintf("statement memory budget exceeded: %d bytes buffered exceeds limit %d bytes", e.Used, e.Limit)
}

// TimeLimitExceededError is returned when the OnlineIndexer's overall time limit is exceeded.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexingBase.TimeLimitException.
type TimeLimitExceededError struct {
	TimeLimit time.Duration
	Elapsed   time.Duration
}

func (e *TimeLimitExceededError) Error() string {
	return fmt.Sprintf("online indexer time limit exceeded: limit=%v, elapsed=%v", e.TimeLimit, e.Elapsed)
}

func (*TimeLimitExceededError) JavaRecordCoreException() {}

// ScanLimitReachedError is returned by a leaf cursor when it hits a
// scanned-records or scanned-bytes limit AND ExecuteProperties.
// FailOnScanLimitReached is set. Mirrors Java's ScanLimitReachedException
// (RecordCoreException, SQLSTATE 54F01): with setFailOnScanLimitReached(true)
// the scan errors instead of paginating. Default (false) is unchanged —
// the cursor returns a ScanLimitReached/ByteLimitReached NoNextReason and
// the caller paginates via the continuation.
type ScanLimitReachedError struct {
	// Reason is the out-of-band stop reason that triggered the failure
	// (ScanLimitReached or ByteLimitReached).
	Reason NoNextReason
}

func (e *ScanLimitReachedError) Error() string {
	switch e.Reason {
	case ByteLimitReached:
		return "scan limit reached: scanned-bytes limit exceeded"
	case TimeLimitReached:
		return "scan limit reached: time limit exceeded"
	default:
		return "scan limit reached: scanned-records limit exceeded"
	}
}

func (*ScanLimitReachedError) JavaRecordCoreException() {}
