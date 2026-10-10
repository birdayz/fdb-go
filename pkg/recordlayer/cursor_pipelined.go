// Portions derived from FoundationDB Record Layer (MapPipelinedCursor.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import "context"

// MapPipelined keeps up to pipelineSize mappings issued ahead of the one being
// returned and yields them in source order, each with its source continuation.
// issue starts a mapping (sends its reads) and returns the call that waits for
// it. Everything runs on the caller's goroutine; the overlap comes from
// futures already in flight. Matches Java's MapPipelinedCursor.
func MapPipelined[T, V any](inner RecordCursor[T], issue func(T) func() (V, error), pipelineSize int) RecordCursor[V] {
	if pipelineSize <= 0 {
		_ = inner.Close()
		return &errorCursor[V]{err: &RecordCoreArgumentError{Message: "pipeline size must be positive"}}
	}
	return &mapPipelinedCursor[T, V]{inner: inner, issue: issue, size: pipelineSize}
}

type mapPipelineEntry[V any] struct {
	resolve      func() (V, error)
	continuation RecordCursorContinuation
}

type mapPipelinedCursor[T, V any] struct {
	inner    RecordCursor[T]
	issue    func(T) func() (V, error)
	size     int
	pipeline []mapPipelineEntry[V]
	stop     *RecordCursorResult[V]
	last     *RecordCursorResult[V]
	err      error
}

func (c *mapPipelinedCursor[T, V]) fail(err error) (RecordCursorResult[V], error) {
	c.err = err
	clear(c.pipeline)
	c.pipeline = nil
	return RecordCursorResult[V]{}, err
}

func (c *mapPipelinedCursor[T, V]) OnNext(ctx context.Context) (RecordCursorResult[V], error) {
	if c.last != nil && !c.last.HasNext() {
		return *c.last, nil
	}
	if c.err != nil {
		return RecordCursorResult[V]{}, c.err
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[V]{}, err
	}
	for c.stop == nil && len(c.pipeline) < c.size {
		source, err := c.inner.OnNext(ctx)
		if err != nil {
			return c.fail(err)
		}
		if !source.HasNext() {
			stop := NewResultNoNext[V](source.GetNoNextReason(), source.GetContinuation())
			// Under time pressure Java stops waiting for unfinished entries and
			// resumes after the last returned row; an issued read is never known
			// finished here, so every queued entry is dropped. Not before the
			// first row: there is no earlier continuation to resume from.
			if source.GetNoNextReason() == TimeLimitReached && c.last != nil && len(c.pipeline) > 0 {
				clear(c.pipeline)
				c.pipeline = nil
				stop = NewResultNoNext[V](TimeLimitReached, c.last.GetContinuation())
			}
			c.stop = &stop
			break
		}
		c.pipeline = append(c.pipeline, mapPipelineEntry[V]{
			resolve: c.issue(source.GetValue()), continuation: source.GetContinuation(),
		})
	}
	if len(c.pipeline) == 0 {
		c.last = c.stop
		return *c.last, nil
	}
	entry := c.pipeline[0]
	c.pipeline[0] = mapPipelineEntry[V]{}
	c.pipeline = c.pipeline[1:]
	value, err := entry.resolve()
	if err != nil {
		return c.fail(err)
	}
	result := NewResultWithValue(value, entry.continuation)
	c.last = &result
	return result, nil
}

func (c *mapPipelinedCursor[T, V]) Close() error {
	clear(c.pipeline)
	c.pipeline = nil
	return c.inner.Close()
}

func (c *mapPipelinedCursor[T, V]) IsClosed() bool { return c.inner.IsClosed() }
