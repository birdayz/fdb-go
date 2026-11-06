// Portions derived from FoundationDB Record Layer (UnionCursor.java,
// UnionCursorBase.java, UnorderedUnionCursor.java, UnionCursorContinuation.java,
// MergeCursor.java, MergeCursorState.java, KeyedMergeCursorState.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// CursorFunction builds a child cursor from its continuation (nil = start),
// Java's Function<byte[], RecordCursor<T>> handed to the merge cursors.
type CursorFunction[T any] func(continuation []byte) RecordCursor[T]

// mergeStateCursor preserves END with an empty child; other children resume from their bytes
// (Java MergeCursorState.from / KeyedMergeCursorState.from).
func mergeStateCursor[T any](fn CursorFunction[T], cont RecordCursorContinuation) (RecordCursor[T], RecordCursorContinuation, error) {
	if cont.IsEnd() {
		return Empty[T](), &EndContinuation{}, nil
	}
	b, err := cont.ToBytes()
	if err != nil {
		return nil, nil, err
	}
	return fn(b), cont, nil
}

// strongestNoNextReason prefers out-of-band stops; union exhausts only when all stopped children do
// (MergeCursor.java:174-194).
func strongestNoNextReason[T any](children []*mergeChildState[T]) NoNextReason {
	found := false
	reason := SourceExhausted
	for _, child := range children {
		if child.needsAdvance || child.hasResult {
			continue // never pulled, or still holds a value
		}
		childReason := child.result.GetNoNextReason()
		if !found || childReason.IsOutOfBand() || reason.IsSourceExhausted() {
			reason = childReason
			found = true
		}
	}
	return reason
}

// buildUnionContinuation snapshots the children's consumed positions.
func buildUnionContinuation[T any](children []*mergeChildState[T]) (RecordCursorContinuation, error) {
	states := make([]RecordCursorContinuation, len(children))
	for i, child := range children {
		states[i] = child.continuation
	}
	data, err := EncodeUnionContinuation(states)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &EndContinuation{}, nil
	}
	return NewBytesContinuation(data), nil
}

// EncodeUnionContinuation shares Java's START/MID/END framing across union cursors.
// All children at END encode as nil (UnionCursorContinuation.toBytes).
func EncodeUnionContinuation(children []RecordCursorContinuation) ([]byte, error) {
	allEnd := true
	for _, child := range children {
		if !child.IsEnd() {
			allEnd = false
			break
		}
	}
	if allEnd {
		return nil, nil
	}
	cont := &gen.UnionContinuation{}
	for i, cc := range children {
		var b []byte
		if !cc.IsEnd() {
			var err error
			if b, err = cc.ToBytes(); err != nil {
				return nil, fmt.Errorf("union continuation child %d: %w", i, err)
			}
		}
		switch i {
		case 0:
			if cc.IsEnd() {
				cont.FirstExhausted = proto.Bool(true)
			} else if len(b) > 0 {
				cont.FirstContinuation = b
			}
		case 1:
			if cc.IsEnd() {
				cont.SecondExhausted = proto.Bool(true)
			} else if len(b) > 0 {
				cont.SecondContinuation = b
			}
		default:
			state := &gen.UnionContinuation_CursorState{}
			if cc.IsEnd() {
				state.Exhausted = proto.Bool(true)
			} else if len(b) > 0 {
				state.Continuation = b
			} else {
				state.Exhausted = proto.Bool(false)
			}
			cont.OtherChildState = append(cont.OtherChildState, state)
		}
	}
	data, err := cont.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("union continuation marshal: %w", err)
	}
	return data, nil
}

// DecodeUnionContinuation decodes shared Java child framing; nil starts every child.
// Non-nil data must contain exactly n child slots (UnionCursorContinuation.from).
func DecodeUnionContinuation(data []byte, n int) ([]RecordCursorContinuation, error) {
	out := make([]RecordCursorContinuation, n)
	if data == nil {
		for i := range out {
			out[i] = &StartContinuation{}
		}
		return out, nil
	}
	var parsed gen.UnionContinuation
	if err := UnmarshalVTAsJava(&parsed, data); err != nil {
		return nil, &RecordCoreError{Message: "invalid continuation", Cause: err}
	}
	child := func(b []byte, exhausted bool) RecordCursorContinuation {
		switch {
		case b != nil:
			return NewBytesContinuation(b)
		case exhausted:
			return &EndContinuation{}
		default:
			return &StartContinuation{}
		}
	}
	states := []RecordCursorContinuation{
		child(parsed.FirstContinuation, parsed.GetFirstExhausted()),
		child(parsed.SecondContinuation, parsed.GetSecondExhausted()),
	}
	for _, s := range parsed.OtherChildState {
		states = append(states, child(s.Continuation, s.GetExhausted()))
	}
	if len(states) != n {
		msg := fmt.Sprintf("invalid continuation (expected continuation count does not match read): expected %d, read %d", n, len(states))
		return nil, &RecordCoreArgumentError{Message: msg}
	}
	copy(out, states)
	return out, nil
}

// newUnionChildren restores Java UnionCursor/UnorderedUnionCursor merge states via the shared codec.
func newUnionChildren[T any](fns []CursorFunction[T], compKeyFunc ComparisonKeyFunc[T], continuation []byte) ([]*mergeChildState[T], error) {
	conts, err := DecodeUnionContinuation(continuation, len(fns))
	if err != nil {
		return nil, err
	}
	children := make([]*mergeChildState[T], len(fns))
	for i, fn := range fns {
		cursor, cont, err := mergeStateCursor(fn, conts[i])
		if err != nil {
			return nil, err
		}
		children[i] = &mergeChildState[T]{cursor: cursor, compKeyFunc: compKeyFunc, continuation: cont, needsAdvance: true}
	}
	return children, nil
}

// unionCursor is Java's UnionCursor: an ordered merge of children sorted by the
// same comparison key, returning each key once.
type unionCursor[T any] struct {
	children   []*mergeChildState[T]
	reverse    bool
	closed     bool
	lastNoNext *RecordCursorResult[T]
}

// Union is UnionCursor.create(comparisonKeyFunction, reverse, cursorFunctions,
// continuation, timer) (UnionCursor.java:290-302).
func Union[T any](compKeyFunc ComparisonKeyFunc[T], reverse bool, fns []CursorFunction[T], continuation []byte) (RecordCursor[T], error) {
	if len(fns) < 2 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("not enough child cursors provided to UnionCursor: %d", len(fns))}
	}
	children, err := newUnionChildren(fns, compKeyFunc, continuation)
	if err != nil {
		return nil, err
	}
	return &unionCursor[T]{children: children, reverse: reverse}, nil
}

// OnNext is MergeCursor.onNext (MergeCursor.java:290-305) over
// UnionCursor.computeNextResultStates (UnionCursor.java:74-98).
func (c *unionCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[T]{}, err
	}
	for _, child := range c.children {
		if child.needsAdvance {
			if err := child.advance(ctx); err != nil {
				return RecordCursorResult[T]{}, err
			}
		}
	}
	anyHasNext := false
	for _, child := range c.children {
		if child.hasResult {
			anyHasNext = true
		} else if child.result.GetNoNextReason().IsLimitReached() {
			// A child stopped by a limit stops the union: continuing could
			// return keys after the stopped child's next one twice.
			return c.stop()
		}
	}
	if !anyHasNext {
		return c.stop()
	}
	// chooseStates (UnionCursor.java:101-135): the minimum key (maximum when
	// reversed), and every child that holds it.
	var chosen []*mergeChildState[T]
	for _, child := range c.children {
		if !child.hasResult {
			continue
		}
		if len(chosen) == 0 {
			chosen = append(chosen, child)
			continue
		}
		cmp, err := compareKeys(child.comparisonKey, chosen[0].comparisonKey)
		if err != nil {
			return RecordCursorResult[T]{}, err
		}
		if c.reverse {
			cmp = -cmp
		}
		if cmp < 0 {
			chosen = append(chosen[:0], child)
		} else if cmp == 0 {
			chosen = append(chosen, child)
		}
	}
	value := chosen[0].result.GetValue()
	for _, child := range chosen {
		child.consume()
	}
	cont, err := buildUnionContinuation(c.children)
	if err != nil {
		return RecordCursorResult[T]{}, err
	}
	return NewResultWithValue(value, cont), nil
}

func (c *unionCursor[T]) stop() (RecordCursorResult[T], error) {
	res, err := stopUnion(c.children)
	if err != nil {
		return res, err
	}
	c.lastNoNext = &res
	return res, nil
}

// stopUnion pairs the strongest stopped-child reason with the union continuation.
func stopUnion[T any](children []*mergeChildState[T]) (RecordCursorResult[T], error) {
	cont, err := buildUnionContinuation(children)
	if err != nil {
		return RecordCursorResult[T]{}, err
	}
	reason := strongestNoNextReason(children)
	if reason == SourceExhausted && !cont.IsEnd() {
		// Every child is exhausted only when every continuation is END.
		return RecordCursorResult[T]{}, &RecordCoreError{Message: "union exhausted with a non-end continuation"}
	}
	return NewResultNoNext[T](reason, cont), nil
}

func closeMergeChildren[T any](children []*mergeChildState[T]) error {
	var firstErr error
	for _, child := range children {
		if err := child.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *unionCursor[T]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return closeMergeChildren(c.children)
}

func (c *unionCursor[T]) IsClosed() bool { return c.closed }

// unorderedUnionCursor pulls children serially in index order, matching Java’s availability order
// when all child results are already complete (UnorderedUnionCursor.java:68-105).
type unorderedUnionCursor[T any] struct {
	children   []*mergeChildState[T]
	closed     bool
	lastNoNext *RecordCursorResult[T]
}

// UnorderedUnion is UnorderedUnionCursor.create(cursorFunctions, continuation,
// timer) (UnorderedUnionCursor.java:132-137).
func UnorderedUnion[T any](fns []CursorFunction[T], continuation []byte) (RecordCursor[T], error) {
	children, err := newUnionChildren(fns, nil, continuation)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		child.compKeyFunc = nil
	}
	return &unorderedUnionCursor[T]{children: children}, nil
}

func (c *unorderedUnionCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[T]{}, err
	}
	for _, child := range c.children {
		if child.needsAdvance {
			if err := child.advanceUnkeyed(ctx); err != nil {
				return RecordCursorResult[T]{}, err
			}
		}
		if child.hasResult {
			value := child.result.GetValue()
			child.consume()
			cont, err := buildUnionContinuation(c.children)
			if err != nil {
				return RecordCursorResult[T]{}, err
			}
			return NewResultWithValue(value, cont), nil
		}
	}
	res, err := stopUnion(c.children)
	if err != nil {
		return res, err
	}
	c.lastNoNext = &res
	return res, nil
}

func (c *unorderedUnionCursor[T]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return closeMergeChildren(c.children)
}

func (c *unorderedUnionCursor[T]) IsClosed() bool { return c.closed }

// advanceUnkeyed is advance for a MergeCursorState without a comparison key
// (MergeCursorState.handleNextCursorResult, MergeCursorState.java:58-63).
func (s *mergeChildState[T]) advanceUnkeyed(ctx context.Context) error {
	result, err := s.cursor.OnNext(ctx)
	if err != nil {
		return err
	}
	s.result = result
	s.hasResult = result.HasNext()
	s.needsAdvance = false
	if !s.hasResult {
		s.continuation = result.GetContinuation()
	}
	return nil
}
