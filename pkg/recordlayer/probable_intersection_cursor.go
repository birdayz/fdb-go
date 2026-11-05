// Portions derived from FoundationDB Record Layer
// (ProbableIntersectionCursor.java, ProbableIntersectionCursorState.java,
// ProbableIntersectionCursorContinuation.java,
// BloomFilterCursorContinuation.java, MergeCursor.java),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Defaults of ProbableIntersectionCursor (ProbableIntersectionCursor.java:85-89).
const (
	ProbableIntersectionDefaultExpectedResults         int64   = 500
	ProbableIntersectionDefaultFalsePositivePercentage float64 = 0.01
)

// probableChild remembers consumed comparison keys in a persisted Bloom filter
// and a run-local exact set, as Java ProbableIntersectionCursorState does.
type probableChild[T any] struct {
	*mergeChildState[T]
	bloom          *guavaBloomFilter
	seen           map[string]struct{}
	firstIteration bool
}

// consume is ProbableIntersectionCursorState.consume
// (ProbableIntersectionCursorState.java:78-83).
func (s *probableChild[T]) consumeKey() error {
	if err := s.bloom.put(s.comparisonKey); err != nil {
		return err
	}
	s.seen[string(s.comparisonKey.Pack())] = struct{}{}
	s.consume()
	return nil
}

// mightContain is ProbableIntersectionCursorState.mightContain
// (ProbableIntersectionCursorState.java:110-115).
func (s *probableChild[T]) mightContain(key tuple.Tuple, packed string) (bool, error) {
	if _, ok := s.seen[packed]; ok {
		return true, nil
	}
	if s.firstIteration {
		return false, nil
	}
	return s.bloom.mightContain(key)
}

func (s *probableChild[T]) isDefiniteDuplicate() bool {
	_, ok := s.seen[string(s.comparisonKey.Pack())]
	return ok
}

// probableIntersectionCursor emits likely intersections by serial child-index order,
// matching Java when every child’s next result is already complete.
type probableIntersectionCursor[T any] struct {
	children   []*probableChild[T]
	closed     bool
	lastNoNext *RecordCursorResult[T]
}

// ProbableIntersection creates Java-compatible probable intersections and resumes
// from per-child Bloom-filter continuation state.
func ProbableIntersection[T any](compKeyFunc ComparisonKeyFunc[T], fns []CursorFunction[T], expectedResults int64, falsePositivePercentage float64, continuation []byte) (RecordCursor[T], error) {
	states, err := decodeProbableIntersectionContinuation(continuation, len(fns))
	if err != nil {
		return nil, err
	}
	children := make([]*probableChild[T], len(fns))
	for i, fn := range fns {
		st := states[i]
		var bloom *guavaBloomFilter
		if st.bloom == nil {
			bloom, err = newGuavaBloomFilter(expectedResults, falsePositivePercentage)
		} else {
			bloom, err = readGuavaBloomFilter(st.bloom)
		}
		if err != nil {
			return nil, err
		}
		cursor, cont, err := mergeStateCursor(fn, st.child)
		if err != nil {
			return nil, err
		}
		children[i] = &probableChild[T]{
			mergeChildState: &mergeChildState[T]{cursor: cursor, compKeyFunc: compKeyFunc, continuation: cont, needsAdvance: true},
			bloom:           bloom,
			seen:            map[string]struct{}{},
			firstIteration:  !st.child.IsEnd() && st.bloom == nil,
		}
	}
	return &probableIntersectionCursor[T]{children: children}, nil
}

func (c *probableIntersectionCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
		allDone := true
		for _, child := range c.children {
			if child.needsAdvance {
				if err := child.advance(ctx); err != nil {
					return RecordCursorResult[T]{}, err
				}
			}
			if !child.hasResult {
				continue
			}
			allDone = false
			if !child.isDefiniteDuplicate() {
				inRest, err := c.checkIfInRest(child)
				if err != nil {
					return RecordCursorResult[T]{}, err
				}
				if inRest {
					value := child.result.GetValue()
					if err := child.consumeKey(); err != nil {
						return RecordCursorResult[T]{}, err
					}
					cont, err := c.continuation()
					if err != nil {
						return RecordCursorResult[T]{}, err
					}
					return NewResultWithValue(value, cont), nil
				}
			}
			// Not (yet) in every other child, so not a result now.
			if err := child.consumeKey(); err != nil {
				return RecordCursorResult[T]{}, err
			}
		}
		if allDone {
			break
		}
	}
	cont, err := c.continuation()
	if err != nil {
		return RecordCursorResult[T]{}, err
	}
	states := make([]*mergeChildState[T], len(c.children))
	for i, child := range c.children {
		states[i] = child.mergeChildState
	}
	reason := strongestNoNextReason(states)
	if reason == SourceExhausted && !cont.IsEnd() {
		return RecordCursorResult[T]{}, &RecordCoreError{Message: "probable intersection exhausted with a non-end continuation"}
	}
	res := NewResultNoNext[T](reason, cont)
	c.lastNoNext = &res
	return res, nil
}

// checkIfInRest is ProbableIntersectionCursor.checkIfInRest
// (ProbableIntersectionCursor.java:104-117).
func (c *probableIntersectionCursor[T]) checkIfInRest(state *probableChild[T]) (bool, error) {
	packed := string(state.comparisonKey.Pack())
	for _, other := range c.children {
		if other == state {
			continue
		}
		ok, err := other.mightContain(state.comparisonKey, packed)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// continuation uses Java BloomFilterCursorContinuation framing; END requires every child at END.
func (c *probableIntersectionCursor[T]) continuation() (RecordCursorContinuation, error) {
	allEnd := true
	for _, child := range c.children {
		if !child.continuation.IsEnd() {
			allEnd = false
			break
		}
	}
	if allEnd {
		return &EndContinuation{}, nil
	}
	cont := &gen.ProbableIntersectionContinuation{}
	for i, child := range c.children {
		state := &gen.ProbableIntersectionContinuation_CursorState{}
		if child.continuation.IsEnd() {
			state.Exhausted = proto.Bool(true)
		} else {
			b, err := child.continuation.ToBytes()
			if err != nil {
				return nil, fmt.Errorf("probable intersection continuation child %d: %w", i, err)
			}
			if len(b) > 0 {
				state.Continuation = b
			}
		}
		state.BloomFilter = child.bloom.writeTo()
		cont.ChildState = append(cont.ChildState, state)
	}
	data, err := cont.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("probable intersection continuation marshal: %w", err)
	}
	return NewBytesContinuation(data), nil
}

func (c *probableIntersectionCursor[T]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var firstErr error
	for _, child := range c.children {
		if err := child.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *probableIntersectionCursor[T]) IsClosed() bool { return c.closed }

// probableChildState is one decoded BloomFilterCursorContinuation: the child's
// continuation and its serialized Bloom filter (nil when the cursor starts).
type probableChildState struct {
	child RecordCursorContinuation
	bloom []byte
}

// decodeProbableIntersectionContinuation follows Java’s decoder; a missing Bloom filter
// becomes empty bytes, which Guava readFrom rejects.
func decodeProbableIntersectionContinuation(data []byte, n int) ([]probableChildState, error) {
	out := make([]probableChildState, n)
	if data == nil {
		for i := range out {
			out[i] = probableChildState{child: &StartContinuation{}}
		}
		return out, nil
	}
	var parsed gen.ProbableIntersectionContinuation
	if err := UnmarshalVTAsJava(&parsed, data); err != nil {
		return nil, &RecordCoreError{Message: "invalid continuation", Cause: err}
	}
	if len(parsed.ChildState) != n {
		msg := fmt.Sprintf("invalid continuation (extraneous child state information present): expected %d, read %d", n, len(parsed.ChildState))
		return nil, &RecordCoreArgumentError{Message: msg}
	}
	for i, s := range parsed.ChildState {
		bloom := s.BloomFilter
		if bloom == nil {
			bloom = []byte{}
		}
		switch {
		case s.GetExhausted():
			out[i] = probableChildState{child: &EndContinuation{}, bloom: bloom}
		case s.Continuation != nil:
			out[i] = probableChildState{child: NewBytesContinuation(s.Continuation), bloom: bloom}
		default:
			out[i] = probableChildState{child: &StartContinuation{}, bloom: bloom}
		}
	}
	return out, nil
}
