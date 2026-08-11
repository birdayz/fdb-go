package api

import (
	"bytes"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

const (
	continuationWireVersion = int32(1)
	maxContinuationBytes    = 16 << 20

	continuationAvailableMessage = "query page has a continuation"
	invalidContinuationMessage   = "invalid continuation"
)

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

// immutableContinuation is a parsed, defensive continuation snapshot. Its
// byte-returning methods always copy, so neither the producer passed to
// NewContinuationAvailableError nor a consumer can mutate the token carried by
// a page-boundary error.
type immutableContinuation struct {
	serialized     []byte
	executionState []byte
	reason         ContinuationReason
}

func (c *immutableContinuation) Serialize() []byte {
	if c == nil {
		return nil
	}
	return cloneContinuationBytes(c.serialized)
}

func (c *immutableContinuation) ExecutionState() []byte {
	if c == nil {
		return nil
	}
	return cloneContinuationBytes(c.executionState)
}

func (c *immutableContinuation) Reason() ContinuationReason {
	if c == nil {
		return ContinuationUserRequested
	}
	return c.reason
}

func (c *immutableContinuation) clone() *immutableContinuation {
	if c == nil {
		return nil
	}
	// The backing slices are already privately owned and are never mutated.
	// A new unexported view is sufficient here because the only byte accessors
	// above return copies; duplicating both the envelope and its nested state on
	// every Continuation() call would turn a 16 MiB token into another 32 MiB of
	// live allocations without adding isolation.
	return &immutableContinuation{
		serialized:     c.serialized,
		executionState: c.executionState,
		reason:         c.reason,
	}
}

// cloneContinuationBytes preserves the distinction between nil (cursor at the
// beginning / absent execution_state) and a present, zero-length slice (cursor
// after the last row). append to a nil slice would collapse those two states.
func cloneContinuationBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func parseContinuationSnapshot(serialized []byte) (*immutableContinuation, error) {
	if len(serialized) == 0 || len(serialized) > maxContinuationBytes {
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}

	// This carrier needs only the three top-level control fields. Decode those
	// with a bounded wire scanner and leave compiled_statement / copyPlan
	// opaque. A normal protobuf unmarshal would recursively allocate every plan
	// node before the GO_V1 bounded decoder exists; millions of zero-length
	// repeated messages can amplify a sub-16 MiB token far beyond its byte cap.
	// The closed top-level shape has seven singular fields, so duplicates,
	// unknown fields, and wrong wire types all fail closed here.
	var (
		seen                [8]bool
		version             uint64
		reasonValue         uint64
		executionStateStart = -1
		executionStateEnd   = -1
		remaining           = serialized
		offset              int
	)
	for len(remaining) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(remaining)
		if tagLen < 0 || number < 1 || number > 7 || seen[number] {
			return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
		}
		seen[number] = true
		remaining = remaining[tagLen:]
		offset += tagLen

		switch number {
		case 1, 3, 4, 6:
			if wireType != protowire.VarintType {
				return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
			}
			value, valueLen := protowire.ConsumeVarint(remaining)
			if valueLen < 0 {
				return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
			}
			if number == 1 {
				version = value
			} else if number == 6 {
				reasonValue = value
			}
			remaining = remaining[valueLen:]
			offset += valueLen
		case 2, 5, 7:
			if wireType != protowire.BytesType {
				return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
			}
			value, valueLen := protowire.ConsumeBytes(remaining)
			if valueLen < 0 {
				return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
			}
			if number == 2 {
				executionStateStart = offset + valueLen - len(value)
				executionStateEnd = executionStateStart + len(value)
			}
			remaining = remaining[valueLen:]
			offset += valueLen
		}
	}

	if !seen[1] || !seen[2] || !seen[6] || version != uint64(continuationWireVersion) {
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}
	reason, ok := continuationReasonFromWire(reasonValue)
	if !ok {
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}

	// One defensive copy owns both the serialized envelope and the execution
	// state view. Byte-returning methods copy on access, so producer mutation
	// after this point cannot change the carrier and the state is not copied a
	// second time merely because it is nested inside the envelope.
	snapshotBytes := cloneContinuationBytes(serialized)
	return &immutableContinuation{
		serialized:     snapshotBytes,
		executionState: snapshotBytes[executionStateStart:executionStateEnd],
		reason:         reason,
	}, nil
}

func continuationReasonFromWire(reason uint64) (ContinuationReason, bool) {
	switch reason {
	case uint64(gen.ContinuationProto_USER_REQUESTED_CONTINUATION):
		return ContinuationUserRequested, true
	case uint64(gen.ContinuationProto_TRANSACTION_LIMIT_REACHED):
		return ContinuationTransactionLimitReached, true
	case uint64(gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED):
		return ContinuationQueryExecutionLimitReached, true
	case uint64(gen.ContinuationProto_CURSOR_AFTER_LAST):
		return ContinuationCursorAfterLast, true
	default:
		return 0, false
	}
}

var endContinuationSnapshot = mustBuildEndContinuation()

func mustBuildEndContinuation() *immutableContinuation {
	wire := &gen.ContinuationProto{
		Version:        proto.Int32(continuationWireVersion),
		ExecutionState: make([]byte, 0),
		Reason:         gen.ContinuationProto_CURSOR_AFTER_LAST.Enum(),
	}
	serialized, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if err != nil {
		panic("api: marshal END continuation: " + err.Error())
	}
	return &immutableContinuation{
		serialized:     serialized,
		executionState: make([]byte, 0),
		reason:         ContinuationCursorAfterLast,
	}
}

// EndContinuation returns the standard immutable version-1 continuation for a
// result set whose source is exhausted. Its execution state is present but
// empty, its reason is CURSOR_AFTER_LAST, and it has no compiled statement.
func EndContinuation() Continuation {
	return endContinuationSnapshot.clone()
}

// ContinuationAvailableError is the typed driver.Rows boundary signal for an
// incomplete SQL result page. Ordinary database/sql callers observe it from
// Rows.Err; sqldriver.PageRows consumes it and exposes its result-scoped
// continuation. It is deliberately not a sentinel and does not unwrap another
// error (in particular, it can never wrap driver.ErrBadConn).
type ContinuationAvailableError struct {
	continuation *immutableContinuation
}

// NewContinuationAvailableError validates and snapshots a non-terminal SQL
// continuation. The serialized envelope is authoritative: its parsed execution
// state and reason must agree with the Continuation methods supplied by the
// producer. Only query- and transaction-limit boundaries are valid here.
func NewContinuationAvailableError(c Continuation) (*ContinuationAvailableError, error) {
	if c == nil {
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}

	// parseContinuationSnapshot applies the size limit before taking its own
	// defensive copy. Passing the producer's view directly avoids copying an
	// oversized token merely to reject it, and avoids a redundant second copy
	// for valid tokens.
	snapshot, err := parseContinuationSnapshot(c.Serialize())
	if err != nil {
		return nil, err
	}
	producerState := c.ExecutionState()
	if (producerState == nil) != (snapshot.executionState == nil) ||
		!bytes.Equal(producerState, snapshot.executionState) ||
		c.Reason() != snapshot.reason || len(snapshot.executionState) == 0 {
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}
	switch snapshot.reason {
	case ContinuationQueryExecutionLimitReached, ContinuationTransactionLimitReached:
		// Valid non-terminal page boundaries.
	default:
		return nil, NewError(ErrCodeInvalidContinuation, invalidContinuationMessage)
	}

	return &ContinuationAvailableError{continuation: snapshot}, nil
}

// Error returns a constant, redacted boundary description. It never includes
// token bytes, plan text, literals, or keys.
func (*ContinuationAvailableError) Error() string {
	return continuationAvailableMessage
}

// Continuation returns a defensive immutable copy of the continuation carried
// by this boundary, or nil for an invalid zero-value error.
func (e *ContinuationAvailableError) Continuation() Continuation {
	if e == nil || e.continuation == nil {
		return nil
	}
	return e.continuation.clone()
}

// Reason returns the reason from the same immutable parsed snapshot exposed by
// Continuation. The zero value is USER_REQUESTED_CONTINUATION only when e is an
// invalid zero-value error; constructors never produce that state.
func (e *ContinuationAvailableError) Reason() ContinuationReason {
	if e == nil || e.continuation == nil {
		return ContinuationUserRequested
	}
	return e.continuation.reason
}

var _ error = (*ContinuationAvailableError)(nil)
