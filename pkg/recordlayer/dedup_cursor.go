package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen"
)

// DedupCursor removes adjacent duplicate elements from an inner cursor.
// Assumes the inner cursor is sorted so that duplicates are grouped consecutively.
// Matches Java's com.apple.foundationdb.record.cursors.DedupCursor.
//
// Type parameters:
//   - T: the element type
//
// Functions:
//   - equal: returns true if two elements are considered duplicates
//   - pack: serializes an element to bytes for the continuation (may be nil to skip)
//   - unpack: deserializes an element from continuation bytes
type dedupCursor[T any] struct {
	innerFactory CursorFactory[T]
	inner        RecordCursor[T]
	equal        func(a, b T) bool
	pack         func(T) []byte
	unpack       func([]byte) (T, bool)
	lastValue    T
	hasLast      bool
	closed       bool
}

// Dedup creates a cursor that removes adjacent duplicate elements.
// equal compares two elements; pack/unpack serialize the last value for continuations.
// If pack is nil, continuation will not include lastValue.
// Matches Java's DedupCursor.
func Dedup[T any](
	innerFactory CursorFactory[T],
	equal func(a, b T) bool,
	pack func(T) []byte,
	unpack func([]byte) (T, bool),
	continuation []byte,
) RecordCursor[T] {
	c := &dedupCursor[T]{
		innerFactory: innerFactory,
		equal:        equal,
		pack:         pack,
		unpack:       unpack,
	}

	if len(continuation) > 0 {
		var cont gen.DedupContinuation
		if err := cont.UnmarshalVT(continuation); err != nil {
			// Java: throw new RecordCoreException("Error parsing continuation", ex)
			//           .addLogInfo("raw_bytes", ...)  (DedupCursor's constructor).
			// A corrupt continuation must fail, not silently restart from scratch:
			// restarting re-emits rows the caller already consumed.
			return &errorCursor[T]{err: &ContinuationParseError{
				Message:  "Error parsing continuation",
				RawBytes: continuation,
				Cause:    err,
			}}
		}
		// LastValue presence check mirrors Java's dedupContinuation.hasLastValue().
		if lv := cont.LastValue; lv != nil {
			// Java's constructor calls unpackValue.apply(lastValue) inside the same
			// try block; a failure there propagates out of the constructor rather
			// than silently dropping the dedup state (which would re-emit the last
			// value as a duplicate on resume).
			if unpack == nil {
				return &errorCursor[T]{err: fmt.Errorf("dedup continuation carries lastValue but no unpack function was provided")}
			}
			val, ok := unpack(lv)
			if !ok {
				return &errorCursor[T]{err: fmt.Errorf("dedup continuation: unpack lastValue failed (raw_bytes=%x)", lv)}
			}
			c.lastValue = val
			c.hasLast = true
		}
		c.inner = innerFactory(cont.GetInnerContinuation())
	} else {
		c.inner = innerFactory(nil)
	}

	return c
}

func (c *dedupCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}

		if !result.HasNext() {
			// Inner exhausted or stopped — pass through with wrapped continuation
			innerCont := result.GetContinuation()
			wrapped, wrapErr := c.wrapContinuation(innerCont)
			if wrapErr != nil {
				return RecordCursorResult[T]{}, wrapErr
			}
			return NewResultNoNext[T](result.GetNoNextReason(), wrapped), nil
		}

		val := result.GetValue()

		// Skip if equal to last value
		if c.hasLast && c.equal(val, c.lastValue) {
			continue
		}

		// New unique value
		c.lastValue = val
		c.hasLast = true

		innerCont := result.GetContinuation()
		wrapped, wrapErr := c.wrapContinuation(innerCont)
		if wrapErr != nil {
			return RecordCursorResult[T]{}, wrapErr
		}
		return NewResultWithValue(val, wrapped), nil
	}
}

func (c *dedupCursor[T]) wrapContinuation(inner RecordCursorContinuation) (RecordCursorContinuation, error) {
	if inner == nil || inner.IsEnd() {
		return &EndContinuation{}, nil
	}
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("dedup continuation: %w", err)
	}
	return &dedupContinuationWrapper[T]{
		inner:    innerBytes,
		lastPack: c.packLast(),
	}, nil
}

func (c *dedupCursor[T]) packLast() []byte {
	if !c.hasLast || c.pack == nil {
		return nil
	}
	return c.pack(c.lastValue)
}

type dedupContinuationWrapper[T any] struct {
	inner    []byte
	lastPack []byte
}

func (d *dedupContinuationWrapper[T]) ToBytes() ([]byte, error) {
	cont := &gen.DedupContinuation{
		InnerContinuation: d.inner,
		LastValue:         d.lastPack,
	}
	return cont.MarshalVT()
}

func (d *dedupContinuationWrapper[T]) IsEnd() bool {
	return false
}

func (c *dedupCursor[T]) Close() error {
	c.closed = true
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}

func (c *dedupCursor[T]) IsClosed() bool { return c.closed }
