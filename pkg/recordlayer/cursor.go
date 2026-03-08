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
	// ToBytes serializes this continuation to a byte array
	// Returns nil if this is an end continuation
	ToBytes() []byte
	
	// IsEnd returns true if this represents the end of iteration
	IsEnd() bool
}

// BytesContinuation is a simple continuation that wraps a byte array
type BytesContinuation struct {
	bytes []byte
}

// ToBytes returns the continuation bytes
func (c *BytesContinuation) ToBytes() []byte {
	return c.bytes
}

// IsEnd returns true if this is an end continuation
func (c *BytesContinuation) IsEnd() bool {
	return c.bytes == nil
}

// EndContinuation represents the end of a cursor's iteration
type EndContinuation struct{}

// ToBytes always returns nil for end continuations
func (c *EndContinuation) ToBytes() []byte {
	return nil
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

// GetValue returns the value if HasNext is true, otherwise returns zero value
func (r RecordCursorResult[T]) GetValue() T {
	if r.value != nil {
		return *r.value
	}
	var zero T
	return zero
}

// GetContinuation returns the continuation for resuming the cursor
func (r RecordCursorResult[T]) GetContinuation() RecordCursorContinuation {
	return r.continuation
}

// GetNoNextReason returns the reason why there's no next record (valid when HasNext is false)
func (r RecordCursorResult[T]) GetNoNextReason() NoNextReason {
	return r.noNextReason
}

// RecordCursor is a generic async iterator over records
type RecordCursor[T any] interface {
	// OnNext asynchronously returns the next result from this cursor
	OnNext(ctx context.Context) (RecordCursorResult[T], error)
	
	// Close releases any resources held by this cursor
	Close() error
	
	// Seq returns an iterator sequence over values only
	Seq(ctx context.Context) iter.Seq[T]
	
	// Seq2 returns an iterator sequence over (value, error) pairs
	Seq2(ctx context.Context) iter.Seq2[T, error]
	
	// SeqWithContinuation returns an iterator sequence over (value, continuation) pairs
	SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation]
}

// ForEach applies a function to each record in the cursor
func ForEach[T any](ctx context.Context, cursor RecordCursor[T], fn func(T) error) error {
	defer func() { _ = cursor.Close() }()
	
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return err
		}
		
		if !result.HasNext() {
			return nil
		}
		
		if err := fn(result.GetValue()); err != nil {
			return err
		}
	}
}

// AsList collects all records from the cursor into a slice
func AsList[T any](ctx context.Context, cursor RecordCursor[T]) ([]T, error) {
	var results []T
	err := ForEach(ctx, cursor, func(record T) error {
		results = append(results, record)
		return nil
	})
	return results, err
}

// Sequence transformation functions that work with iter.Seq/Seq2
// These are more idiomatic Go than cursor-specific transformations

// Filter returns a filtered sequence
func Filter[T any](seq iter.Seq[T], predicate func(T) bool) iter.Seq[T] {
	return func(yield func(T) bool) {
		for value := range seq {
			if predicate(value) {
				if !yield(value) {
					return
				}
			}
		}
	}
}

// Map transforms values in a sequence
func Map[T, R any](seq iter.Seq[T], fn func(T) R) iter.Seq[R] {
	return func(yield func(R) bool) {
		for value := range seq {
			if !yield(fn(value)) {
				return
			}
		}
	}
}

// MapErr transforms values in a sequence with error handling
func MapErr[T, R any](seq iter.Seq2[T, error], fn func(T) (R, error)) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		for value, err := range seq {
			if err != nil {
				if !yield(*new(R), err) {
					return
				}
				continue
			}
			
			mapped, mappingErr := fn(value)
			if !yield(mapped, mappingErr) {
				return
			}
		}
	}
}

// Filter2 filters a Seq2 sequence
func Filter2[T any](seq iter.Seq2[T, error], predicate func(T) bool) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for value, err := range seq {
			if err != nil {
				if !yield(*new(T), err) {
					return
				}
				continue
			}
			
			if predicate(value) {
				if !yield(value, nil) {
					return
				}
			}
		}
	}
}

// Limit returns at most n values from a sequence
func Limit[T any](seq iter.Seq[T], n int) iter.Seq[T] {
	return func(yield func(T) bool) {
		count := 0
		for value := range seq {
			if count >= n {
				return
			}
			if !yield(value) {
				return
			}
			count++
		}
	}
}

// filterCursor wraps another cursor and filters records by a predicate.
// Filtered records are skipped silently; continuations are forwarded from the inner cursor.
type filterCursor[T any] struct {
	inner     RecordCursor[T]
	predicate func(T) bool
}

func (c *filterCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	for {
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			return result, nil
		}
		if c.predicate(result.GetValue()) {
			return result, nil
		}
		// Skip this record, keep iterating
	}
}

func (c *filterCursor[T]) Close() error {
	return c.inner.Close()
}

func (c *filterCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *filterCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
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

func (c *filterCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
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