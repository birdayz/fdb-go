package recordlayer

import (
	"context"
	"iter"

	"google.golang.org/protobuf/proto"
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

// ToBytes returns the continuation bytes
func (c *BytesContinuation) ToBytes() ([]byte, error) {
	return c.bytes, nil
}

// IsEnd returns true if this is an end continuation
func (c *BytesContinuation) IsEnd() bool {
	return c.bytes == nil
}

// EndContinuation represents the end of a cursor's iteration
type EndContinuation struct{}

// ToBytes always returns nil for end continuations
func (c *EndContinuation) ToBytes() ([]byte, error) {
	return nil, nil
}

// IsEnd always returns true for end continuations
func (c *EndContinuation) IsEnd() bool {
	return true
}

// RecordCursorResult represents the result of a cursor's OnNext() call
type RecordCursorResult[T any] struct {
	value        *T
	continuation RecordCursorContinuation
	noNextReason NoNextReason
	hasNext      bool
}

// NewResultWithValue creates a result with a value
func NewResultWithValue[T any](value T, continuation RecordCursorContinuation) RecordCursorResult[T] {
	return RecordCursorResult[T]{
		value:        &value,
		continuation: continuation,
		hasNext:      true,
	}
}

// NewResultNoNext creates a result indicating no more records
func NewResultNoNext[T any](reason NoNextReason, continuation RecordCursorContinuation) RecordCursorResult[T] {
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
// Matches Java's RecordCursorResult.hasStoppedBeforeEnd().
func (r RecordCursorResult[T]) HasStoppedBeforeEnd() bool {
	return !r.hasNext && !r.noNextReason.IsSourceExhausted()
}

// RecordCursor is a generic async iterator over records
type RecordCursor[T any] interface {
	// OnNext asynchronously returns the next result from this cursor
	OnNext(ctx context.Context) (RecordCursorResult[T], error)

	// Close releases any resources held by this cursor
	Close() error
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

// errorCursor is a cursor that immediately returns an error on every OnNext call.
// Used when a cursor cannot be created (e.g., scanning a non-readable index).
type errorCursor[T any] struct {
	err error
}

func (c *errorCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	return RecordCursorResult[T]{}, c.err
}

func (c *errorCursor[T]) Close() error { return nil }

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
// Continuation format: 4-byte big-endian position (nil = start from beginning).
func FromListWithContinuation[T any](items []T, continuation []byte) RecordCursor[T] {
	start := 0
	if len(continuation) == 4 {
		start = int(continuation[0])<<24 | int(continuation[1])<<16 | int(continuation[2])<<8 | int(continuation[3])
	}
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

// RecordCursorProto is a convenience type for cursors over protobuf messages
type RecordCursorProto = RecordCursor[*FDBStoredRecord[proto.Message]]

// TypedRecordCursor is a convenience type for typed record cursors
type TypedRecordCursor[T proto.Message] RecordCursor[*FDBStoredRecord[T]]

// Note: Most sequence utilities are available in Go 1.23+ standard library:
// - slices.Collect() for collecting sequences
// - Use range loops directly for counting, filtering, etc.
// - Many iterator utilities in the "iter" package
//
// We only provide Record Layer specific cursor transformations here.
