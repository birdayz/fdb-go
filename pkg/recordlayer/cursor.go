package recordlayer

import (
	"context"
	"fmt"
	"iter"
	"math"
)

// NoNextReason indicates why a cursor stopped producing records
type NoNextReason int

const (
	// SourceExhausted means the cursor reached the end of its data
	SourceExhausted NoNextReason = iota
	// ReturnLimitReached means the cursor hit its row limit
	ReturnLimitReached
	// ByteLimitReached means the cursor hit its byte scan limit
	ByteLimitReached
	// TimeLimitReached means the cursor hit its time limit
	TimeLimitReached
	// ScanLimitReached means the cursor hit its key-value scan limit
	ScanLimitReached
)

// IsOutOfBand returns true if this reason represents an out-of-band completion
// (not solely dependent on the records returned)
func (r NoNextReason) IsOutOfBand() bool {
	return r != SourceExhausted && r != ReturnLimitReached
}

// IsSourceExhausted returns true if there is no more data available
func (r NoNextReason) IsSourceExhausted() bool {
	return r == SourceExhausted
}

// IsLimitReached returns true if the cursor stopped due to any kind of limit
func (r NoNextReason) IsLimitReached() bool {
	return r != SourceExhausted
}

// RecordCursorContinuation represents the position of a cursor for resumption
type RecordCursorContinuation interface {
	// ToBytes serializes this continuation to a byte array.
	// Returns (nil, nil) if this is an end continuation.
	ToBytes() ([]byte, error)

	// IsEnd returns true if this represents the end of iteration
	IsEnd() bool
}

// BytesContinuation is a simple continuation that wraps a byte array
type BytesContinuation struct {
	bytes []byte
}

// NewBytesContinuation creates a BytesContinuation with the given bytes.
// A nil bytes value means end-of-cursor (IsEnd returns true).
func NewBytesContinuation(b []byte) *BytesContinuation {
	return &BytesContinuation{bytes: b}
}

// ToBytes returns the continuation bytes
func (c *BytesContinuation) ToBytes() ([]byte, error) {
	return c.bytes, nil
}

// IsEnd returns true if this is an end continuation
func (c *BytesContinuation) IsEnd() bool {
	return c.bytes == nil
}

// EndContinuation represents the end of a cursor's iteration.
// INVARIANT: Only valid when NoNextReason is SourceExhausted.
// Matches Java's RecordCursorEndContinuation.
type EndContinuation struct{}

// ToBytes always returns nil for end continuations
func (c *EndContinuation) ToBytes() ([]byte, error) {
	return nil, nil
}

// IsEnd always returns true for end continuations
func (c *EndContinuation) IsEnd() bool {
	return true
}

// StartContinuation represents the start of a cursor's iteration, or a state
// where no continuation is available for the current result. Unlike EndContinuation,
// this is NOT terminal — the cursor may have more data.
// Matches Java's RecordCursorStartContinuation.
type StartContinuation struct{}

// ToBytes returns nil (no position information available).
func (c *StartContinuation) ToBytes() ([]byte, error) {
	return nil, nil
}

// IsEnd returns false — StartContinuation is never an end state.
func (c *StartContinuation) IsEnd() bool {
	return false
}

// RecordCursorResult represents the result of a cursor's OnNext() call.
//
// Invariants (matching Java's RecordCursorResult):
//   - A result WITH a value (HasNext=true) must NOT have an EndContinuation
//   - A result with SourceExhausted MUST have an EndContinuation
//   - A result with any other NoNextReason must NOT have an EndContinuation
type RecordCursorResult[T any] struct {
	value        *T
	continuation RecordCursorContinuation
	noNextReason NoNextReason
	hasNext      bool
}

// NewResultWithValue creates a result with a value.
// Panics if continuation is an EndContinuation — a value result must always
// have a resumable continuation. Matches Java's RecordCursorResult.withNextValue().
func NewResultWithValue[T any](value T, continuation RecordCursorContinuation) RecordCursorResult[T] {
	if continuation != nil && continuation.IsEnd() {
		panic("cannot return end continuation with next value")
	}
	return RecordCursorResult[T]{
		value:        &value,
		continuation: continuation,
		hasNext:      true,
	}
}

// NewResultNoNext creates a result indicating no more records.
// Enforces invariants matching Java's RecordCursorResult.withoutNextValue():
//   - EndContinuation is only valid with SourceExhausted
//   - SourceExhausted requires an EndContinuation
func NewResultNoNext[T any](reason NoNextReason, continuation RecordCursorContinuation) RecordCursorResult[T] {
	isEnd := continuation != nil && continuation.IsEnd()
	if isEnd && !reason.IsSourceExhausted() {
		panic(fmt.Sprintf("cannot return end continuation with NoNextReason %d (only valid for SourceExhausted)", reason))
	}
	if reason.IsSourceExhausted() && !isEnd {
		panic("SourceExhausted requires an end continuation")
	}
	return RecordCursorResult[T]{
		continuation: continuation,
		noNextReason: reason,
		hasNext:      false,
	}
}

// HasNext returns true if this result contains a value
func (r RecordCursorResult[T]) HasNext() bool {
	return r.hasNext
}

// GetValue returns the value. Panics if HasNext() is false — callers must check HasNext() first.
// This matches Java's behavior of throwing IllegalResultValueAccessException.
func (r RecordCursorResult[T]) GetValue() T {
	if !r.hasNext {
		panic("GetValue called on RecordCursorResult with no value (check HasNext() first)")
	}
	return *r.value
}

// GetContinuation returns the continuation for resuming the cursor
func (r RecordCursorResult[T]) GetContinuation() RecordCursorContinuation {
	return r.continuation
}

// GetNoNextReason returns the reason why there's no next record (valid when HasNext is false)
func (r RecordCursorResult[T]) GetNoNextReason() NoNextReason {
	return r.noNextReason
}

// HasStoppedBeforeEnd returns true if the cursor stopped before exhausting all records.
// This means the cursor can be resumed with the continuation to get more results.
// Matches Java's RecordCursorResult.hasStoppedBeforeEnd() which checks the continuation,
// not the reason — with the EndContinuation↔SourceExhausted invariant, these are equivalent.
func (r RecordCursorResult[T]) HasStoppedBeforeEnd() bool {
	return !r.hasNext && r.continuation != nil && !r.continuation.IsEnd()
}

// WithContinuation returns a copy of this result with a different continuation.
// Matches Java's RecordCursorResult.withContinuation().
func (r RecordCursorResult[T]) WithContinuation(continuation RecordCursorContinuation) RecordCursorResult[T] {
	r.continuation = continuation
	return r
}

// MapResult transforms a result's value using the given function.
// If the result has no value (HasNext() is false), returns a no-next result
// with the same reason and continuation. Go generics require this as a
// standalone function rather than a method.
// Matches Java's RecordCursorResult.map().
func MapResult[T, R any](result RecordCursorResult[T], fn func(T) R) RecordCursorResult[R] {
	if !result.hasNext {
		return NewResultNoNext[R](result.noNextReason, result.continuation)
	}
	mapped := fn(*result.value)
	return NewResultWithValue(mapped, result.continuation)
}

// RecordCursor is a generic async iterator over records
type RecordCursor[T any] interface {
	// OnNext asynchronously returns the next result from this cursor
	OnNext(ctx context.Context) (RecordCursorResult[T], error)

	// Close releases any resources held by this cursor
	Close() error

	// IsClosed returns true if the cursor has been closed
	IsClosed() bool
}

// CursorFactory creates a cursor from an optional continuation.
// nil continuation means start from the beginning.
type CursorFactory[T any] func(continuation []byte) RecordCursor[T]

// Seq returns an iterator sequence over values only.
// Errors are silently dropped; use Seq2 if you need error handling.
func Seq[T any](cursor RecordCursor[T], ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = cursor.Close() }()
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

// Seq2 returns an iterator sequence over (value, error) pairs.
func Seq2[T any](cursor RecordCursor[T], ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = cursor.Close() }()
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

// SeqWithContinuation returns an iterator sequence over (value, continuation) pairs.
func SeqWithContinuation[T any](cursor RecordCursor[T], ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = cursor.Close() }()
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// emptyCursor is a cursor that immediately returns no results.
// Matches Java's RecordCursor.empty().
type emptyCursor[T any] struct{}

// Empty returns a cursor that produces no results (source exhausted immediately).
func Empty[T any]() RecordCursor[T] {
	return &emptyCursor[T]{}
}

func (c *emptyCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
}

func (c *emptyCursor[T]) Close() error { return nil }

func (c *emptyCursor[T]) IsClosed() bool { return false }

// errorCursor is a cursor that immediately returns an error on every OnNext call.
// Used when a cursor cannot be created (e.g., scanning a non-readable index).
type errorCursor[T any] struct {
	err error
}

func (c *errorCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	return RecordCursorResult[T]{}, c.err
}

func (c *errorCursor[T]) Close() error { return nil }

func (c *errorCursor[T]) IsClosed() bool { return false }

// listCursor wraps a slice as a cursor. Matches Java's RecordCursor.fromList().
// Supports continuation via single-byte position encoding (up to 255 elements).
type listCursor[T any] struct {
	items  []T
	pos    int
	closed bool
}

// FromList creates a cursor from a slice. Matches Java's RecordCursor.fromList().
func FromList[T any](items []T) RecordCursor[T] {
	return &listCursor[T]{items: items}
}

// FromListWithContinuation creates a cursor from a slice, starting from a continuation.
// Matches Java's RecordCursor.fromList(list, continuation).
// Continuation format: 4-byte big-endian position (nil/empty = start from beginning).
// Java uses ByteBuffer.wrap(continuation).getInt() which reads first 4 bytes and
// throws BufferUnderflowException for continuations shorter than 4 bytes. Go returns
// an error from OnNext for invalid continuation lengths to match Java's fail-fast behavior.
func FromListWithContinuation[T any](items []T, continuation []byte) RecordCursor[T] {
	if len(continuation) == 0 {
		return &listCursor[T]{items: items, pos: 0}
	}
	if len(continuation) < 4 {
		// Java throws BufferUnderflowException. Return an error cursor.
		return &errorCursor[T]{err: fmt.Errorf("invalid list continuation: expected at least 4 bytes, got %d", len(continuation))}
	}
	// Read first 4 bytes as big-endian int (matches Java's ByteBuffer.getInt())
	start := int(continuation[0])<<24 | int(continuation[1])<<16 | int(continuation[2])<<8 | int(continuation[3])
	if start > len(items) {
		start = len(items)
	}
	return &listCursor[T]{items: items, pos: start}
}

func listCursorContinuation(pos int) []byte {
	return []byte{byte(pos >> 24), byte(pos >> 16), byte(pos >> 8), byte(pos)}
}

func (c *listCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.closed || c.pos >= len(c.items) {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}
	value := c.items[c.pos]
	c.pos++
	return NewResultWithValue(value, &BytesContinuation{bytes: listCursorContinuation(c.pos)}), nil
}

func (c *listCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *listCursor[T]) IsClosed() bool { return c.closed }

// saturatingAdd returns a + b, clamped to math.MaxInt on overflow.
// Both a and b must be non-negative.
func saturatingAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// Note: Most sequence utilities are available in Go 1.23+ standard library:
// - slices.Collect() for collecting sequences
// - Use range loops directly for counting, filtering, etc.
// - Many iterator utilities in the "iter" package
//
// We only provide Record Layer specific cursor transformations here.
