package recordlayer

import (
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

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

// noNextOrFail returns a ScanLimitReachedError when FailOnScanLimitReached
// is set on the execute properties, otherwise it returns the out-of-band
// no-next result (paginate). Reason must be ScanLimitReached or
// ByteLimitReached; the continuation is the resume point for the
// paginating case. Centralizes the fail-vs-paginate decision so every
// leaf cursor's scan/byte-limit return site behaves identically.
func noNextOrFail[T any](
	props ExecuteProperties,
	reason NoNextReason,
	continuation RecordCursorContinuation,
) (RecordCursorResult[T], error) {
	if props.FailOnScanLimitReached {
		return RecordCursorResult[T]{}, &ScanLimitReachedError{Reason: reason}
	}
	return NewResultNoNext[T](reason, continuation), nil
}

// IsolationLevel represents the transaction isolation level.
// Matches Java's IsolationLevel enum.
//
// Java Reference: com.apple.foundationdb.record.IsolationLevel
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/IsolationLevel.java
type IsolationLevel int

const (
	// IsolationLevelSnapshot uses snapshot reads, which see a consistent view of the database
	// at the time the transaction started. Snapshot reads do not conflict with writes.
	//
	// Java equivalent: SNAPSHOT
	IsolationLevelSnapshot IsolationLevel = iota

	// IsolationLevelSerializable uses serializable reads, which participate in conflict detection.
	// Serializable reads will cause conflicts if another transaction writes to the same keys.
	//
	// Java equivalent: SERIALIZABLE
	IsolationLevelSerializable

	// Legacy aliases for backwards compatibility
	SnapshotIsolation     = IsolationLevelSnapshot
	SerializableIsolation = IsolationLevelSerializable
)

// IsSnapshot returns true if this isolation level uses snapshot reads.
//
// Java equivalent: IsolationLevel.isSnapshot()
func (level IsolationLevel) IsSnapshot() bool {
	return level == IsolationLevelSnapshot
}

// String returns a human-readable representation of the isolation level.
func (level IsolationLevel) String() string {
	switch level {
	case IsolationLevelSnapshot:
		return "Snapshot"
	case IsolationLevelSerializable:
		return "Serializable"
	default:
		return "Unknown"
	}
}

// CursorStreamingMode controls how FDB fetches data
type CursorStreamingMode int

const (
	// StreamingModeSmall fetches small amounts at a time
	StreamingModeSmall CursorStreamingMode = iota
	// StreamingModeMedium fetches medium amounts at a time
	StreamingModeMedium
	// StreamingModeLarge fetches large amounts at a time
	StreamingModeLarge
	// StreamingModeSerial fetches one at a time
	StreamingModeSerial
	// StreamingModeWantAll fetches as much as possible
	StreamingModeWantAll
	// StreamingModeIterator is the default iterator mode
	StreamingModeIterator
)

// ToFDB converts to FDB's StreamingMode
func (m CursorStreamingMode) ToFDB() fdb.StreamingMode {
	switch m {
	case StreamingModeSmall:
		return fdb.StreamingModeSmall
	case StreamingModeMedium:
		return fdb.StreamingModeMedium
	case StreamingModeLarge:
		return fdb.StreamingModeLarge
	case StreamingModeSerial:
		return fdb.StreamingModeSerial
	case StreamingModeWantAll:
		return fdb.StreamingModeWantAll
	default:
		return fdb.StreamingModeIterator
	}
}

// ExecuteProperties holds properties that pertain to an entire execution
type ExecuteProperties struct {
	// IsolationLevel for the execution
	IsolationLevel IsolationLevel

	// ReturnedRowLimit is the maximum number of records to return
	// 0 or negative means no limit
	ReturnedRowLimit int

	// ScannedRecordsLimit is the maximum number of records to scan
	// 0 or negative means no limit
	ScannedRecordsLimit int

	// ScannedBytesLimit is the maximum number of bytes to scan
	// 0 or negative means no limit
	ScannedBytesLimit int64

	// TimeLimit is the maximum time for the operation
	// Zero duration means no limit
	TimeLimit time.Duration

	// DefaultCursorStreamingMode is the default streaming mode for cursors
	DefaultCursorStreamingMode CursorStreamingMode

	// FailOnScanLimitReached determines if hitting scan limits should fail the operation
	FailOnScanLimitReached bool

	// Skip is the number of records to skip before returning results.
	// Matches Java's ExecuteProperties.getSkip().
	Skip int

	// MaterializationLimit caps the number of rows that may be buffered
	// in memory by operators that materialize an entire relation (NLJ
	// inner, UNION buffered, recursive CTE levels). Zero means use
	// DefaultMaterializationLimit.
	MaterializationLimit int

	// State is the statement-scoped mutable counter object (RFC-130),
	// mirroring Java's ExecuteState held by reference inside the otherwise
	// value-copied ExecuteProperties. It is a POINTER, so every value-copy of
	// ExecuteProperties (the WithX helpers, ClearSkipAndLimit, per-operator
	// innerProps) shares ONE counter and none of them reset it — the
	// statement-wide memory byte budget survives all inner-plan resets
	// structurally, which is the whole point. It is always minted once per
	// statement (never nil) where the statement's ExecuteProperties is first
	// built; the "no budget" case is memLimit<=0, NOT a nil State, so a missed
	// accumulation site charges an unlimited counter rather than silently
	// no-oping. None of the WithX helpers below clear it: they copy the value
	// struct, which copies the pointer.
	State *ExecuteState
}

const DefaultMaterializationLimit = 100_000

// DefaultExecuteProperties returns properties with sensible defaults
func DefaultExecuteProperties() ExecuteProperties {
	return ExecuteProperties{
		IsolationLevel:             SerializableIsolation,
		ReturnedRowLimit:           0, // No limit
		ScannedRecordsLimit:        0, // No limit
		ScannedBytesLimit:          0, // No limit
		TimeLimit:                  0, // No limit
		DefaultCursorStreamingMode: StreamingModeIterator,
		FailOnScanLimitReached:     false,
	}
}

// WithReturnedRowLimit returns a copy with the specified row limit
func (e ExecuteProperties) WithReturnedRowLimit(limit int) ExecuteProperties {
	e.ReturnedRowLimit = limit
	return e
}

// WithTimeLimit returns a copy with the specified time limit
func (e ExecuteProperties) WithTimeLimit(limit time.Duration) ExecuteProperties {
	e.TimeLimit = limit
	return e
}

// WithIsolationLevel returns a copy with the specified isolation level
func (e ExecuteProperties) WithIsolationLevel(level IsolationLevel) ExecuteProperties {
	e.IsolationLevel = level
	return e
}

// WithScannedRecordsLimit returns a copy with the specified scanned records limit.
func (e ExecuteProperties) WithScannedRecordsLimit(limit int) ExecuteProperties {
	e.ScannedRecordsLimit = limit
	return e
}

// WithScannedBytesLimit returns a copy with the specified scanned bytes limit.
func (e ExecuteProperties) WithScannedBytesLimit(limit int64) ExecuteProperties {
	e.ScannedBytesLimit = limit
	return e
}

// WithSkip returns a copy with the specified skip count.
func (e ExecuteProperties) WithSkip(skip int) ExecuteProperties {
	e.Skip = skip
	return e
}

// ClearRowAndTimeLimits returns a copy with row limit and time limit cleared.
// Matches Java's ExecuteProperties.clearRowAndTimeLimits().
func (e ExecuteProperties) ClearRowAndTimeLimits() ExecuteProperties {
	e.ReturnedRowLimit = 0
	e.TimeLimit = 0
	return e
}

// WithMaterializationLimit returns a copy with the specified materialization limit.
func (e ExecuteProperties) WithMaterializationLimit(limit int) ExecuteProperties {
	e.MaterializationLimit = limit
	return e
}

// GetMaterializationLimit returns the effective materialization limit,
// falling back to DefaultMaterializationLimit when zero.
func (e ExecuteProperties) GetMaterializationLimit() int {
	if e.MaterializationLimit > 0 {
		return e.MaterializationLimit
	}
	return DefaultMaterializationLimit
}

// ClearSkipAndLimit returns a copy with skip and row limit cleared.
// Matches Java's ExecuteProperties.clearSkipAndLimit().
func (e ExecuteProperties) ClearSkipAndLimit() ExecuteProperties {
	e.Skip = 0
	e.ReturnedRowLimit = 0
	return e
}

// ScanProperties groups properties that pertain to a single scan
type ScanProperties struct {
	// ExecuteProperties holds the execution-level properties
	ExecuteProperties ExecuteProperties

	// Reverse indicates if the scan should be in reverse order
	Reverse bool

	// CursorStreamingMode overrides the default streaming mode
	CursorStreamingMode CursorStreamingMode
}

// ForwardScan returns properties for a forward scan with default execution properties.
// Returns a fresh copy each time to prevent mutation of shared state.
func ForwardScan() ScanProperties {
	return ScanProperties{
		ExecuteProperties:   DefaultExecuteProperties(), // No row limit (0 = unlimited)
		Reverse:             false,
		CursorStreamingMode: StreamingModeIterator,
	}
}

// ReverseScan returns properties for a reverse scan with default execution properties.
// Returns a fresh copy each time to prevent mutation of shared state.
func ReverseScan() ScanProperties {
	return ScanProperties{
		ExecuteProperties:   DefaultExecuteProperties(), // No row limit (0 = unlimited)
		Reverse:             true,
		CursorStreamingMode: StreamingModeIterator,
	}
}

// NewScanProperties creates scan properties with the given execution properties
func NewScanProperties(executeProps ExecuteProperties) ScanProperties {
	return ScanProperties{
		ExecuteProperties:   executeProps,
		Reverse:             false,
		CursorStreamingMode: executeProps.DefaultCursorStreamingMode,
	}
}

// WithReverse returns a copy with the reverse flag set
func (s ScanProperties) WithReverse(reverse bool) ScanProperties {
	s.Reverse = reverse
	return s
}

// WithStreamingMode returns a copy with the specified streaming mode
func (s ScanProperties) WithStreamingMode(mode CursorStreamingMode) ScanProperties {
	s.CursorStreamingMode = mode
	return s
}

// WithExecuteProperties returns a copy with the specified execute properties
func (s ScanProperties) WithExecuteProperties(props ExecuteProperties) ScanProperties {
	s.ExecuteProperties = props
	return s
}

// IsReverse returns true if this is a reverse scan
func (s ScanProperties) IsReverse() bool {
	return s.Reverse
}

// GetExecuteProperties returns the execute properties
func (s ScanProperties) GetExecuteProperties() ExecuteProperties {
	return s.ExecuteProperties
}
