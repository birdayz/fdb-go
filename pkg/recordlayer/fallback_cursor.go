package recordlayer

import (
	"context"
	"fmt"
)

// FallbackCursor wraps an inner cursor with automatic failover to a fallback.
// If the inner cursor returns an error, it closes the inner and switches to a
// fallback cursor. The fallback factory receives the last successful result
// (nil if none) so it can resume from that point.
// Matches Java's com.apple.foundationdb.record.cursors.FallbackCursor.
type fallbackCursor[T any] struct {
	inner               RecordCursor[T]
	fallbackFactory     func(lastResult *RecordCursorResult[T]) RecordCursor[T]
	lastSuccessfulValue *RecordCursorResult[T]
	alreadyFailed       bool
	closed              bool
}

// Fallback creates a cursor that falls back to an alternative on error.
// fallbackFactory receives the last successful result (nil if the inner cursor
// failed on its first call) and should return a cursor that resumes from there.
// Matches Java's FallbackCursor — one-shot fallback (fails permanently if
// the fallback cursor also errors).
func Fallback[T any](
	inner RecordCursor[T],
	fallbackFactory func(lastResult *RecordCursorResult[T]) RecordCursor[T],
) RecordCursor[T] {
	return &fallbackCursor[T]{
		inner:           inner,
		fallbackFactory: fallbackFactory,
	}
}

func (c *fallbackCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	result, err := c.inner.OnNext(ctx)
	if err != nil {
		if c.alreadyFailed {
			return result, fmt.Errorf("fallback cursor also failed: %w", err)
		}

		// Switch to fallback
		_ = c.inner.Close()
		c.alreadyFailed = true
		c.inner = c.fallbackFactory(c.lastSuccessfulValue)
		return c.inner.OnNext(ctx)
	}

	if result.HasNext() {
		c.lastSuccessfulValue = &result
	}
	return result, nil
}

func (c *fallbackCursor[T]) Close() error {
	c.closed = true
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}

func (c *fallbackCursor[T]) IsClosed() bool { return c.closed }
