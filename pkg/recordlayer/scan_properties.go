package recordlayer

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

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
}

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