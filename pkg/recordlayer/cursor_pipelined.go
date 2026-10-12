// Portions derived from FoundationDB Record Layer (MapPipelinedCursor.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import "context"

// PendingRead is an issued read. Get waits for and returns its result, once;
// IsReady reports that Get would not wait.
type PendingRead[V any] interface {
	Get() (V, error)
	IsReady() bool
}

// NewPendingRead wraps get; a nil ready means the result never waits.
func NewPendingRead[V any](get func() (V, error), ready func() bool) PendingRead[V] {
	return &pendingFunc[V]{get: get, ready: ready}
}

type pendingFunc[V any] struct {
	get   func() (V, error)
	ready func() bool
	done  bool
	value V
	err   error
}

func (p *pendingFunc[V]) Get() (V, error) {
	if !p.done {
		p.value, p.err = p.get()
		p.done, p.get, p.ready = true, nil, nil
	}
	return p.value, p.err
}

func (p *pendingFunc[V]) IsReady() bool { return p.done || p.ready == nil || p.ready() }

// MapPendingRead applies f to p's result when it is read.
func MapPendingRead[T, V any](p PendingRead[T], f func(T, error) (V, error)) PendingRead[V] {
	return NewPendingRead(func() (V, error) { return f(p.Get()) }, p.IsReady)
}

// MapPipelined keeps up to pipelineSize mappings issued, counting the one being
// returned, and yields them in source order with their source continuations.
// It runs on the caller's goroutine; the overlap comes from reads in flight.
// Matches Java's MapPipelinedCursor.
func MapPipelined[T, V any](inner RecordCursor[T], issue func(T) PendingRead[V], pipelineSize int) RecordCursor[V] {
	if pipelineSize <= 0 {
		_ = inner.Close()
		return &errorCursor[V]{err: &RecordCoreArgumentError{Message: "pipeline size must be positive"}}
	}
	return &mapPipelinedCursor[T, V]{inner: inner, issue: issue, size: pipelineSize}
}

type mapPipelineEntry[V any] struct {
	pending      PendingRead[V]
	continuation RecordCursorContinuation
}

type mapPipelinedCursor[T, V any] struct {
	inner    RecordCursor[T]
	issue    func(T) PendingRead[V]
	size     int
	pipeline []mapPipelineEntry[V]
	head     int
	count    int
	stop     *RecordCursorResult[V]
	last     RecordCursorResult[V]
	hasLast  bool
	err      error
	closed   bool
}

// Offsets are relative to the head; subtract before adding to avoid overflow.
func (c *mapPipelinedCursor[T, V]) pipelineIndex(offset int) int {
	if offset >= len(c.pipeline)-c.head {
		return offset - (len(c.pipeline) - c.head)
	}
	return c.head + offset
}

func (c *mapPipelinedCursor[T, V]) push(entry mapPipelineEntry[V]) {
	if c.count == len(c.pipeline) {
		capacity := 1
		if c.count > 0 {
			capacity = c.size
			if c.count <= c.size/2 {
				capacity = 2 * c.count
			}
		}
		pipeline := make([]mapPipelineEntry[V], capacity)
		n := copy(pipeline, c.pipeline[c.head:])
		copy(pipeline[n:], c.pipeline[:c.head])
		c.pipeline, c.head = pipeline, 0
	}
	c.pipeline[c.pipelineIndex(c.count)] = entry
	c.count++
}

func (c *mapPipelinedCursor[T, V]) pop() mapPipelineEntry[V] {
	entry := c.pipeline[c.head]
	c.pipeline[c.head] = mapPipelineEntry[V]{}
	c.head++
	if c.head == len(c.pipeline) {
		c.head = 0
	}
	c.count--
	return entry
}

func (c *mapPipelinedCursor[T, V]) fail(err error) (RecordCursorResult[V], error) {
	c.err = err
	clear(c.pipeline)
	c.pipeline, c.head, c.count = nil, 0, 0
	return RecordCursorResult[V]{}, err
}

func (c *mapPipelinedCursor[T, V]) OnNext(ctx context.Context) (RecordCursorResult[V], error) {
	if c.hasLast && !c.last.HasNext() {
		return c.last, nil
	}
	if c.closed {
		return RecordCursorResult[V]{}, context.Canceled
	}
	if c.err != nil {
		return RecordCursorResult[V]{}, c.err
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[V]{}, err
	}
	for c.stop == nil && c.count < c.size {
		source, err := c.inner.OnNext(ctx)
		if err != nil {
			return c.fail(err)
		}
		if !source.HasNext() {
			stop := NewResultNoNext[V](source.GetNoNextReason(), source.GetContinuation())
			c.stop = &stop
			if source.GetNoNextReason() == TimeLimitReached && c.hasLast {
				if err := c.keepCompletedPrefix(); err != nil {
					return c.fail(err)
				}
			}
			break
		}
		c.push(mapPipelineEntry[V]{
			pending: c.issue(source.GetValue()), continuation: source.GetContinuation(),
		})
	}
	if c.count == 0 {
		c.last, c.hasLast = *c.stop, true
		return c.last, nil
	}
	entry := c.pop()
	value, err := entry.pending.Get()
	if err != nil {
		return c.fail(err)
	}
	result := NewResultWithValue(value, entry.continuation)
	c.last, c.hasLast = result, true
	return result, nil
}

// Under time pressure Java returns only the completed prefix and resumes after
// it, failing at once if a completed load failed (cancelPendingFutures).
func (c *mapPipelinedCursor[T, V]) keepCompletedPrefix() error {
	continuation := c.last.GetContinuation()
	keep := 0
	for ; keep < c.count; keep++ {
		entry := c.pipeline[c.pipelineIndex(keep)]
		if !entry.pending.IsReady() {
			break
		}
		if _, err := entry.pending.Get(); err != nil {
			return err
		}
		continuation = entry.continuation
	}
	if keep == c.count {
		return nil
	}
	for i := keep; i < c.count; i++ {
		c.pipeline[c.pipelineIndex(i)] = mapPipelineEntry[V]{}
	}
	c.count = keep
	stop := NewResultNoNext[V](TimeLimitReached, continuation)
	c.stop = &stop
	return nil
}

func (c *mapPipelinedCursor[T, V]) Close() error {
	c.closed = true
	clear(c.pipeline)
	c.pipeline, c.head, c.count = nil, 0, 0
	return c.inner.Close()
}

func (c *mapPipelinedCursor[T, V]) IsClosed() bool { return c.closed }
