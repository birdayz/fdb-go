package recordlayer

import (
	"context"
	"fmt"
)

// ChainedCursor iterates over values generated dynamically one at a time.
// A generator function takes the previous value (nil for start) and returns the
// next value. Iteration stops when the generator returns nil, nil.
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
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java ChainedCursor's cached no-next result) — never re-invokes the
	// generator, which may not be idempotent past exhaustion.
	lastNoNext *RecordCursorResult[T]
	// Java advances lastValue before rejecting an end token, so retrying after
	// an encoding failure can skip the un-emitted value. Go deliberately latches
	// that error rather than invoking a stateful generator again. See
	// DIVERGENCES.md, "ChainedCursor encoding failures".
	continuationErr error
}

// Chained creates a cursor that produces values from a generator function.
// generator receives the previous value (nil for the first call) and returns
// the next value or nil to signal exhaustion.
// encode must produce non-nil bytes for every generated value; a missing encoder
// or nil encoding causes OnNext to fail rather than emit an unusable position.
// encode/decode serialize/deserialize values for continuations. Only a nil
// continuation starts a fresh cursor; a non-nil continuation requires a decoder
// that returns true. Decoding failures are returned by OnNext without calling
// the generator.
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

	// Java tests continuation != null and lets decoder failures propagate.
	// Empty bytes can encode a value, and ignoring a failed decode replays rows.
	if continuation != nil {
		if decode == nil {
			return &errorCursor[T]{err: &ContinuationParseError{
				RawBytes: continuation,
				Cause:    fmt.Errorf("chained continuation requires a decoder"),
			}}
		}
		val, ok := decode(continuation)
		if !ok {
			return &errorCursor[T]{err: &ContinuationParseError{
				RawBytes: continuation,
				Cause:    fmt.Errorf("chained continuation decoder rejected bytes"),
			}}
		}
		c.lastValue = &val
	}

	return c
}

func (c *chainedCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	if c.continuationErr != nil {
		return RecordCursorResult[T]{}, c.continuationErr
	}

	next, err := c.generator(c.lastValue)
	if err != nil {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
	}

	if next == nil {
		res := NewResultNoNext[T](SourceExhausted, &EndContinuation{})
		c.lastNoNext = &res
		return res, nil
	}

	c.lastValue = next
	cont, err := c.makeContinuation(*next)
	if err != nil {
		c.continuationErr = err
		return RecordCursorResult[T]{}, err
	}
	return NewResultWithValue(*next, cont), nil
}

func (c *chainedCursor[T]) makeContinuation(val T) (RecordCursorContinuation, error) {
	if c.encode == nil {
		return nil, &ContinuationEncodeError{Message: "chained continuation requires an encoder"}
	}
	raw := c.encode(val)
	if raw == nil {
		// Java's RecordCursorResult.withNextValue rejects an end continuation.
		// Surface the same invariant as an error before the result constructor,
		// rather than panicking or manufacturing a start token that replays rows.
		return nil, &ContinuationEncodeError{Message: "cannot return end continuation with next value"}
	}
	return &BytesContinuation{bytes: raw}, nil
}

func (c *chainedCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *chainedCursor[T]) IsClosed() bool { return c.closed }
