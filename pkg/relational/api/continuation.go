package api

// Continuation is a serialized pointer into a result set that a client
// can use to resume iteration in a later transaction.
//
// Mirrors Java's com.apple.foundationdb.relational.api.Continuation.
// Implementations live closer to the cursor layer.
type Continuation interface {
	// Serialize returns a wire-compatible representation that encodes
	// the continuation's full state. Must be round-trippable by
	// implementations.
	Serialize() []byte

	// ExecutionState returns the underlying cursor state bytes. A nil
	// result means "at the beginning"; an empty (length-0) result
	// means "at the end".
	ExecutionState() []byte

	// Reason explains why the continuation was generated.
	Reason() ContinuationReason
}

// ContinuationReason tags the cause of a continuation. Matches Java's
// Continuation.Reason enum 1:1.
type ContinuationReason int

const (
	// ContinuationUserRequested means getContinuation was called
	// before the ResultSet was exhausted.
	ContinuationUserRequested ContinuationReason = iota
	// ContinuationTransactionLimitReached fires when the byte / row /
	// time scan budget inside a transaction was hit.
	ContinuationTransactionLimitReached
	// ContinuationQueryExecutionLimitReached fires when a per-query
	// row cap was hit.
	ContinuationQueryExecutionLimitReached
	// ContinuationCursorAfterLast indicates the result set was fully
	// exhausted (no more rows).
	ContinuationCursorAfterLast
)

// String returns the Java enum name (for logging / debug output).
func (r ContinuationReason) String() string {
	switch r {
	case ContinuationUserRequested:
		return "USER_REQUESTED_CONTINUATION"
	case ContinuationTransactionLimitReached:
		return "TRANSACTION_LIMIT_REACHED"
	case ContinuationQueryExecutionLimitReached:
		return "QUERY_EXECUTION_LIMIT_REACHED"
	case ContinuationCursorAfterLast:
		return "CURSOR_AFTER_LAST"
	default:
		return "?"
	}
}

// AtBeginning reports whether the continuation is at the beginning
// of the cursor (nil execution state). Default behavior matches Java's
// Continuation.atBeginning.
func AtBeginning(c Continuation) bool {
	return c.ExecutionState() == nil
}

// AtEnd reports whether the continuation is past the last row (empty
// but non-nil execution state). Matches Java's Continuation.atEnd.
func AtEnd(c Continuation) bool {
	es := c.ExecutionState()
	return es != nil && len(es) == 0
}
