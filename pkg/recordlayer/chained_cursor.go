package recordlayer

import (
	"context"
)

// ChainedCursor iterates over values generated dynamically one at a time.
// A generator function takes the previous value (nil for start) and returns the
// next value. Iteration stops when the generator returns nil, false.
// Matches Java's com.apple.foundationdb.record.cursors.ChainedCursor.
//
// Continuations use raw encoded bytes (no proto wrapping) from caller-supplied
// encode/decode functions, matching Java's custom Continuation class.
type chainedCursor[T any] struct {
	generator func(prev *T) (*T, error) // nil result means exhausted
	encode    func(T) []byte
	decode    func([]byte) (T, bool)
	lastValue *T
	closed    bool
}

// Chained creates a cursor that produces values from a generator function.
// generator receives the previous value (nil for the first call) and returns
// the next value or nil to signal exhaustion.
// encode/decode serialize/deserialize values for continuations.
// Matches Java's ChainedCursor.
func Chained[T any](
	generator func(prev *T) (*T, error),
	encode func(T) []byte,
	decode func([]byte) (T, bool),
	continuation []byte,
) RecordCursor[T] {
	c := &chainedCursor[T]{
		generator: generator,
		encode:    encode,
		decode:    decode,
	}

	if len(continuation) > 0 && decode != nil {
		if val, ok := decode(continuation); ok {
			c.lastValue = &val
		}
	}

	return c
}

func (c *chainedCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	next, err := c.generator(c.lastValue)
	if err != nil {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
	}

	if next == nil {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	c.lastValue = next
	cont := c.makeContinuation(*next)
	return NewResultWithValue(*next, cont), nil
}

func (c *chainedCursor[T]) makeContinuation(val T) RecordCursorContinuation {
	if c.encode == nil {
		return &StartContinuation{}
	}
	return &BytesContinuation{bytes: c.encode(val)}
}

func (c *chainedCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *chainedCursor[T]) IsClosed() bool { return c.closed }
