package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// ComparisonKeyFunc extracts a comparison key from a cursor element.
// The returned tuple is used for merge ordering via FDB's order-preserving
// tuple encoding. Matches Java's KeyedMergeCursorState.comparisonKeyFunction.
type ComparisonKeyFunc[T any] func(T) tuple.Tuple

// compareKeys compares two comparison keys using FDB's order-preserving tuple
// encoding. Returns negative if a < b, positive if a > b, 0 if equal.
// Returns error if values are not tuple-encodable.
// Matches Java's KeyComparisons.KEY_COMPARATOR.
func compareKeys(a, b tuple.Tuple) (c int, err error) {
	defer func() {
		if r := recover(); r != nil {
			c, err = 0, fmt.Errorf("compareKeys: unsupported tuple element: %v", r)
		}
	}()
	return bytes.Compare(a.Pack(), b.Pack()), nil
}

// mergeChildState tracks the state of a single child cursor in a merge operation.
type mergeChildState[T any] struct {
	cursor        RecordCursor[T]
	compKeyFunc   ComparisonKeyFunc[T]
	result        RecordCursorResult[T]
	comparisonKey tuple.Tuple
	hasResult     bool
}

// advance fetches the next result from this child's cursor.
func (s *mergeChildState[T]) advance(ctx context.Context) error {
	result, err := s.cursor.OnNext(ctx)
	if err != nil {
		return err
	}
	s.result = result
	s.hasResult = result.HasNext()
	if s.hasResult {
		s.comparisonKey = s.compKeyFunc(result.GetValue())
	} else {
		s.comparisonKey = nil
	}
	return nil
}

// --- UnionCursor ---

// unionCursor merges multiple ordered cursors, returning all distinct elements
// in order. When multiple cursors have the same comparison key, the element
// from the first cursor is returned and others are consumed (deduplication).
// Matches Java's UnionCursor.
type unionCursor[T any] struct {
	children         []*mergeChildState[T]
	reverse          bool
	started          bool
	closed           bool
	stopped          bool                     // set when a child hit an out-of-band limit
	stopReason       NoNextReason             // reason for stop
	stopContinuation RecordCursorContinuation // continuation at stop point
}

// Union creates a merge-union cursor that combines multiple ordered cursors.
// All child cursors must be ordered by the same comparison key.
// The compKeyFunc extracts the comparison key from each element.
// Elements with duplicate keys across cursors are deduplicated (first cursor wins).
// Matches Java's UnionCursor.create().
func Union[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	reverse bool,
) RecordCursor[T] {
	if len(cursors) == 0 {
		return Empty[T]()
	}
	children := make([]*mergeChildState[T], len(cursors))
	for i, c := range cursors {
		children[i] = &mergeChildState[T]{
			cursor:      c,
			compKeyFunc: compKeyFunc,
		}
	}
	return &unionCursor[T]{
		children: children,
		reverse:  reverse,
	}
}

func (c *unionCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	// If a child previously hit an out-of-band limit, stop the union now.
	// The previous call returned the last safe value; this call stops.
	// Matches Java: UnionCursorBase.computeNextResultStates() stops the union
	// when ANY child has !hasNext() && isLimitReached().
	if c.stopped {
		return NewResultNoNext[T](c.stopReason, c.stopContinuation), nil
	}

	// Advance all children that need it
	for _, child := range c.children {
		if !c.started || child.hasResult {
			if !c.started {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
				}
			}
		}
	}

	if !c.started {
		c.started = true
	}

	// Check if any child stopped for a non-exhaustion reason BEFORE selecting a winner.
	// Matches Java: if ANY child has !hasNext() && isLimitReached(), return empty.
	for _, child := range c.children {
		if !child.hasResult && !child.result.GetNoNextReason().IsSourceExhausted() {
			cont, contErr := c.buildContinuation()
			if contErr != nil {
				return RecordCursorResult[T]{}, contErr
			}
			return NewResultNoNext[T](child.result.GetNoNextReason(), cont), nil
		}
	}

	// Find minimum (or maximum for reverse) key across all children
	var minIdx int = -1
	var minKey tuple.Tuple
	for i, child := range c.children {
		if !child.hasResult {
			continue
		}
		if minIdx == -1 {
			minIdx = i
			minKey = child.comparisonKey
			continue
		}
		cmp, cmpErr := compareKeys(child.comparisonKey, minKey)
		if cmpErr != nil {
			return RecordCursorResult[T]{}, cmpErr
		}
		if c.reverse {
			cmp = -cmp
		}
		if cmp < 0 {
			minIdx = i
			minKey = child.comparisonKey
		}
	}

	// No children have results -> exhausted
	if minIdx == -1 {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	// Get the result from the winning child
	result := c.children[minIdx].result

	// Consume all children with the same key (deduplication)
	for _, child := range c.children {
		eq, eqErr := compareKeys(child.comparisonKey, minKey)
		if eqErr != nil {
			return RecordCursorResult[T]{}, eqErr
		}
		if child.hasResult && eq == 0 {
			if err := child.advance(ctx); err != nil {
				return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
			}
		}
	}

	// Check if any child stopped during dedup advance. If so, return this value
	// but stop the union on the next call.
	for _, child := range c.children {
		if !child.hasResult && !child.result.GetNoNextReason().IsSourceExhausted() {
			c.stopped = true
			c.stopReason = child.result.GetNoNextReason()
			var contErr error
			c.stopContinuation, contErr = c.buildContinuation()
			if contErr != nil {
				return RecordCursorResult[T]{}, contErr
			}
			return NewResultWithValue[T](result.GetValue(), c.stopContinuation), nil
		}
	}

	cont, contErr := c.buildContinuation()
	if contErr != nil {
		return RecordCursorResult[T]{}, contErr
	}
	return NewResultWithValue[T](result.GetValue(), cont), nil
}

func (c *unionCursor[T]) buildContinuation() (RecordCursorContinuation, error) {
	cont := &gen.UnionContinuation{}
	for i, child := range c.children {
		var contBytes []byte
		exhausted := false
		if child.hasResult {
			var err error
			contBytes, err = child.result.GetContinuation().ToBytes()
			if err != nil {
				return nil, fmt.Errorf("union continuation child %d: %w", i, err)
			}
		} else {
			exhausted = child.result.GetNoNextReason().IsSourceExhausted()
			var err error
			contBytes, err = child.result.GetContinuation().ToBytes()
			if err != nil {
				return nil, fmt.Errorf("union continuation child %d: %w", i, err)
			}
		}

		if i == 0 {
			cont.FirstContinuation = contBytes
			cont.FirstExhausted = proto.Bool(exhausted)
		} else if i == 1 {
			cont.SecondContinuation = contBytes
			cont.SecondExhausted = proto.Bool(exhausted)
		} else {
			cont.OtherChildState = append(cont.OtherChildState, &gen.UnionContinuation_CursorState{
				Continuation: contBytes,
				Exhausted:    proto.Bool(exhausted),
			})
		}
	}
	data, err := cont.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("union continuation marshal: %w", err)
	}
	return &BytesContinuation{bytes: data}, nil
}

func (c *unionCursor[T]) Close() error {
	c.closed = true
	var firstErr error
	for _, child := range c.children {
		if err := child.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *unionCursor[T]) IsClosed() bool { return c.closed }

// --- IntersectionCursor ---

// intersectionCursor merges multiple ordered cursors, returning only elements
// that appear in ALL cursors (by comparison key). Uses merge-intersection:
// finds the maximum key, then advances non-maximal cursors until all agree.
// Matches Java's IntersectionCursor.
type intersectionCursor[T any] struct {
	children []*mergeChildState[T]
	reverse  bool
	started  bool
	closed   bool
}

// Intersection creates a merge-intersection cursor that returns only elements
// present in ALL child cursors (by comparison key). All cursors must be ordered
// by the same key. Returns the element from the first cursor when all match.
// Matches Java's IntersectionCursor.create().
func Intersection[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	reverse bool,
) RecordCursor[T] {
	if len(cursors) == 0 {
		return Empty[T]()
	}
	children := make([]*mergeChildState[T], len(cursors))
	for i, c := range cursors {
		children[i] = &mergeChildState[T]{
			cursor:      c,
			compKeyFunc: compKeyFunc,
		}
	}
	return &intersectionCursor[T]{
		children: children,
		reverse:  reverse,
	}
}

func (c *intersectionCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	// Initial advance of all children
	if !c.started {
		for _, child := range c.children {
			if err := child.advance(ctx); err != nil {
				return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
			}
		}
		c.started = true
	}

	// Merge-intersection loop: advance non-maximal cursors until all agree
	for {
		// Check if any child is exhausted
		for _, child := range c.children {
			if !child.hasResult {
				reason := c.weakestNoNextReason()
				if reason.IsSourceExhausted() {
					return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
				}
				cont, contErr := c.buildContinuation()
				if contErr != nil {
					return RecordCursorResult[T]{}, contErr
				}
				return NewResultNoNext[T](reason, cont), nil
			}
		}

		// Find maximum key
		maxKey := c.children[0].comparisonKey
		for _, child := range c.children[1:] {
			cmp, cmpErr := compareKeys(child.comparisonKey, maxKey)
			if cmpErr != nil {
				return RecordCursorResult[T]{}, cmpErr
			}
			if c.reverse {
				cmp = -cmp
			}
			if cmp > 0 {
				maxKey = child.comparisonKey
			}
		}

		// Check if all children agree on the max key
		allMatch := true
		for _, child := range c.children {
			eq, eqErr := compareKeys(child.comparisonKey, maxKey)
			if eqErr != nil {
				return RecordCursorResult[T]{}, eqErr
			}
			if eq != 0 {
				allMatch = false
				break
			}
		}

		if allMatch {
			// All match! Return from first cursor, advance all.
			result := c.children[0].result
			for _, child := range c.children {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
				}
			}
			cont, contErr := c.buildContinuation()
			if contErr != nil {
				return RecordCursorResult[T]{}, contErr
			}
			return NewResultWithValue[T](result.GetValue(), cont), nil
		}

		// Advance all non-maximal children
		for _, child := range c.children {
			neq, neqErr := compareKeys(child.comparisonKey, maxKey)
			if neqErr != nil {
				return RecordCursorResult[T]{}, neqErr
			}
			if neq != 0 {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
				}
			}
		}
	}
}

// weakestNoNextReason returns the weakest reason among exhausted children.
// Intersection uses weakest because if ANY child is exhausted, the intersection
// can produce no more results.
// weakestNoNextReason returns the weakest reason among stopped children.
// Matches Java's IntersectionCursorBase.mergeNoNextReasons():
//   - If ANY child is SourceExhausted, return SourceExhausted immediately
//   - Otherwise, return the weakest non-exhaustion reason
//   - If no stopped children, return SourceExhausted
func (c *intersectionCursor[T]) weakestNoNextReason() NoNextReason {
	found := false
	weakest := TimeLimitReached // start with strongest, find weakest
	for _, child := range c.children {
		if !child.hasResult {
			reason := child.result.GetNoNextReason()
			if reason == SourceExhausted {
				return SourceExhausted // intersection is done
			}
			if !found || isWeaker(reason, weakest) {
				weakest = reason
				found = true
			}
		}
	}
	if found {
		return weakest
	}
	return SourceExhausted
}

// isWeaker returns true if a is weaker than b.
// SourceExhausted is weakest, out-of-band reasons are strongest.
func isWeaker(a, b NoNextReason) bool {
	return strength(a) < strength(b)
}

func strength(r NoNextReason) int {
	switch r {
	case SourceExhausted:
		return 0
	case ReturnLimitReached:
		return 1
	default: // out-of-band: ScanLimitReached, ByteLimitReached, TimeLimitReached
		return 2
	}
}

func (c *intersectionCursor[T]) buildContinuation() (RecordCursorContinuation, error) {
	cont := &gen.IntersectionContinuation{}
	for i, child := range c.children {
		var contBytes []byte
		started := child.hasResult || c.started
		if child.hasResult || !child.result.GetNoNextReason().IsSourceExhausted() {
			var err error
			contBytes, err = child.result.GetContinuation().ToBytes()
			if err != nil {
				return nil, fmt.Errorf("intersection continuation child %d: %w", i, err)
			}
		}

		if i == 0 {
			cont.FirstContinuation = contBytes
			cont.FirstStarted = proto.Bool(started)
		} else if i == 1 {
			cont.SecondContinuation = contBytes
			cont.SecondStarted = proto.Bool(started)
		} else {
			cont.OtherChildState = append(cont.OtherChildState, &gen.IntersectionContinuation_CursorState{
				Continuation: contBytes,
				Started:      proto.Bool(started),
			})
		}
	}
	data, err := cont.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("intersection continuation marshal: %w", err)
	}
	return &BytesContinuation{bytes: data}, nil
}

func (c *intersectionCursor[T]) Close() error {
	c.closed = true
	var firstErr error
	for _, child := range c.children {
		if err := child.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *intersectionCursor[T]) IsClosed() bool { return c.closed }
