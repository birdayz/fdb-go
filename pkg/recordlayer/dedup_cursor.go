package recordlayer

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
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
		if err := proto.Unmarshal(continuation, &cont); err == nil {
			c.inner = innerFactory(cont.GetInnerContinuation())
			if lv := cont.GetLastValue(); len(lv) > 0 && unpack != nil {
				if val, ok := unpack(lv); ok {
					c.lastValue = val
					c.hasLast = true
				}
			}
		} else {
			c.inner = innerFactory(nil)
		}
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
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}

		if !result.HasNext() {
			// Inner exhausted or stopped — pass through with wrapped continuation
			innerCont := result.GetContinuation()
			wrapped := c.wrapContinuation(innerCont)
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
		wrapped := c.wrapContinuation(innerCont)
		return NewResultWithValue(val, wrapped), nil
	}
}

func (c *dedupCursor[T]) wrapContinuation(inner RecordCursorContinuation) RecordCursorContinuation {
	if inner == nil || inner.IsEnd() {
		return &EndContinuation{}
	}
	return &dedupContinuationWrapper[T]{
		inner:    inner.ToBytes(),
		lastPack: c.packLast(),
	}
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

func (d *dedupContinuationWrapper[T]) ToBytes() []byte {
	cont := &gen.DedupContinuation{
		InnerContinuation: d.inner,
		LastValue:         d.lastPack,
	}
	data, _ := proto.Marshal(cont)
	return data
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
