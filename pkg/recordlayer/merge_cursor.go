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
	// continuation is the child's safe resume point — a faithful port of Java
	// MergeCursorState.continuation. It is initialized to the child's starting
	// continuation (START for a fresh child, the resume bytes for a resumed
	// one — see IntersectionResume) and is updated ONLY when the child yields
	// no value (advance: limit/exhausted) or when its current value is consumed
	// (consume: a merge result was emitted). It is NOT updated for a value that
	// is merely loaded but not yet matched — so an out-of-band stop captures the
	// position BEFORE the held value, and resume re-reads it rather than losing
	// it. buildIntersectionContinuation derives the START/MID/END encoding from
	// this continuation.
	//
	// Consequence (Java-faithful): because the continuation sits at the last
	// CONSUMED (matched) position, an out-of-band stop resumes a child from
	// there and re-scans any non-matching rows discarded since the last match
	// (bounded by the inter-match gap; the prefix-to-first-match for a
	// never-matched child). This is correct (no dup/no loss) and matches Java
	// MergeCursorState exactly. Tracking the position just before the currently
	// held candidate to skip that re-scan would be a Go-only optimization beyond
	// Java — out of scope for RFC-071; see TODO.
	continuation RecordCursorContinuation
}

// advance fetches the next result from this child's cursor. Mirrors Java
// MergeCursorState.handleNextCursorResult: a no-value result (limit/exhausted)
// advances the cached continuation; a value is loaded but the continuation is
// left at the last consumed position until consume() is called.
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
		s.continuation = result.GetContinuation()
	}
	return nil
}

// consume records that this child's current value was emitted as part of a
// merge result, advancing the cached continuation past it (Java
// MergeCursorState.consume).
func (s *mergeChildState[T]) consume() {
	s.continuation = s.result.GetContinuation()
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
	return IntersectionResume(cursors, compKeyFunc, reverse, nil)
}

// IntersectionResume is Intersection with per-child resume state from
// DecodeIntersectionContinuation. Each child's cached continuation is seeded
// from resume[i] (START → fresh, MID → its bytes, END → end), so a resumed
// child re-encodes correctly on the next checkpoint (RFC-071). resume may be
// nil (all children fresh, == Intersection).
func IntersectionResume[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	reverse bool,
	resume []IntersectionChildResume,
) RecordCursor[T] {
	if len(cursors) == 0 {
		return Empty[T]()
	}
	return &intersectionCursor[T]{
		children: newMergeChildren(cursors, compKeyFunc, resume),
		reverse:  reverse,
	}
}

// newMergeChildren wraps cursors in mergeChildState, seeding each child's cached
// continuation from resume[i] (nil → all START/fresh). Shared by the
// intersection and multi-intersection resume constructors.
func newMergeChildren[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	resume []IntersectionChildResume,
) []*mergeChildState[T] {
	children := make([]*mergeChildState[T], len(cursors))
	for i, c := range cursors {
		children[i] = &mergeChildState[T]{
			cursor:       c,
			compKeyFunc:  compKeyFunc,
			continuation: initialIntersectionContinuation(resume, i),
		}
	}
	return children
}

// initialIntersectionContinuation maps a decoded per-child resume state to the
// child's initial cached continuation: START (!Started) → StartContinuation,
// MID (Started + bytes) → those bytes, END (Started + empty) → EndContinuation.
func initialIntersectionContinuation(resume []IntersectionChildResume, i int) RecordCursorContinuation {
	if i >= len(resume) || !resume[i].Started {
		return &StartContinuation{}
	}
	if len(resume[i].Continuation) > 0 {
		return NewBytesContinuation(resume[i].Continuation)
	}
	return &EndContinuation{}
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
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
		// Check if any child is exhausted
		for _, child := range c.children {
			if !child.hasResult {
				reason := weakestNoNextReason(c.children)
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
			// All match! Return from first cursor. consume() advances each
			// child's cached continuation past the matched row, then we capture
			// the continuation BEFORE the in-memory advance — so resume re-reads
			// from just after the match and finds the next one (no skip, no
			// dup). Building it after the advance would point each child a row
			// too far, losing every other match on resume (RFC-071).
			result := c.children[0].result
			for _, child := range c.children {
				child.consume()
			}
			cont, contErr := c.buildContinuation()
			if contErr != nil {
				return RecordCursorResult[T]{}, contErr
			}
			for _, child := range c.children {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), err
				}
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

// weakestNoNextReason returns the weakest reason among stopped children.
// Intersection uses the weakest because if ANY child is exhausted, the
// intersection can produce no more results. Shared by intersectionCursor and
// intersectionMultiCursor (both hold []*mergeChildState[T]). Matches Java's
// IntersectionCursorBase.mergeNoNextReasons():
//   - If ANY child is SourceExhausted, return SourceExhausted immediately
//   - Otherwise, return the weakest non-exhaustion reason
//   - If no stopped children, return SourceExhausted
func weakestNoNextReason[T any](children []*mergeChildState[T]) NoNextReason {
	found := false
	weakest := TimeLimitReached // start with strongest, find weakest
	for _, child := range children {
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
	return buildIntersectionContinuation(c.children)
}

// buildIntersectionContinuation encodes a per-child IntersectionContinuation proto
// (each child's continuation + per-child started flag). Shared by intersectionCursor
// and intersectionMultiCursor so the two never drift in continuation encoding.
//
// decodeIntersectionContinuation is the exact inverse, consumed executor-side by
// executeIntersection / executeMultiIntersection to resume each child from its
// saved position (RFC-071). The started flag is read from the per-child
// mergeChildState (set on advance + seeded on resume) rather than a cursor-level
// flag, so the START/MID/END distinction survives any OnNext/checkpoint ordering.
func buildIntersectionContinuation[T any](children []*mergeChildState[T]) (RecordCursorContinuation, error) {
	cont := &gen.IntersectionContinuation{}
	for i, child := range children {
		cc := child.continuation
		if cc == nil {
			cc = &StartContinuation{}
		}
		contBytes, err := cc.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("intersection continuation child %d: %w", i, err)
		}
		// Mirror Java MergeCursorContinuation: an empty-but-not-end continuation
		// is a START position (the child has not progressed past its start),
		// encoded as started=false. An empty end continuation is END
		// (started=true, empty); a non-empty continuation is MID (started=true,
		// bytes). This keeps START (resume re-reads) distinct from END (resume
		// treats as exhausted) — they would otherwise be indistinguishable.
		childStarted := true
		if len(contBytes) == 0 && !cc.IsEnd() {
			childStarted = false
		}

		if i == 0 {
			cont.FirstContinuation = contBytes
			cont.FirstStarted = proto.Bool(childStarted)
		} else if i == 1 {
			cont.SecondContinuation = contBytes
			cont.SecondStarted = proto.Bool(childStarted)
		} else {
			cont.OtherChildState = append(cont.OtherChildState, &gen.IntersectionContinuation_CursorState{
				Continuation: contBytes,
				Started:      proto.Bool(childStarted),
			})
		}
	}
	data, err := cont.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("intersection continuation marshal: %w", err)
	}
	return &BytesContinuation{bytes: data}, nil
}

// IntersectionChildResume is one child's decoded resume state, the output of
// decodeIntersectionContinuation. Started + (empty) Continuation classify the
// child as START (!Started → begin fresh), MID (Started + bytes → resume from
// bytes), or END (Started + empty → exhausted).
type IntersectionChildResume struct {
	Continuation []byte
	Started      bool
}

// DecodeIntersectionContinuation is the exact inverse of
// buildIntersectionContinuation: it splits a parent IntersectionContinuation
// proto into n per-child resume states (RFC-071). A nil/empty continuation
// yields n all-fresh children (START). A proto that fails to parse, or whose
// child count does not match n, is a hard error (mirroring Java
// IntersectionCursorContinuation's RecordCoreArgumentException) — never a
// silent truncate. Consumed executor-side by executeIntersection /
// executeMultiIntersection.
func DecodeIntersectionContinuation(data []byte, n int) ([]IntersectionChildResume, error) {
	out := make([]IntersectionChildResume, n)
	if len(data) == 0 {
		return out, nil // all-fresh
	}
	var cont gen.IntersectionContinuation
	if err := cont.UnmarshalVT(data); err != nil {
		return nil, fmt.Errorf("decode intersection continuation: %w", err)
	}
	// Validate the encoded child count against n on ALL three field groups —
	// the producer always sets FirstStarted (n>=1), SecondStarted (n>=2), and
	// exactly n-2 OtherChildState entries. Checking only OtherChildState (always
	// 0 for n<=2) would silently accept a wrong-count token: a 2-child token
	// decoded as n=1 would drop child 2, and an n=2 token missing
	// SecondStarted would silently restart child 2 at START. Mirrors Java
	// IntersectionCursorContinuation's strict count check (RecordCoreArgumentException).
	wantOthers := n - 2
	if wantOthers < 0 {
		wantOthers = 0
	}
	haveFirst := cont.FirstStarted != nil
	haveSecond := cont.SecondStarted != nil
	if haveFirst != (n >= 1) || haveSecond != (n >= 2) || len(cont.OtherChildState) != wantOthers {
		return nil, fmt.Errorf("intersection continuation child-count mismatch (n=%d): first=%v second=%v others=%d",
			n, haveFirst, haveSecond, len(cont.OtherChildState))
	}
	out[0] = IntersectionChildResume{Continuation: cont.GetFirstContinuation(), Started: cont.GetFirstStarted()}
	if n >= 2 {
		out[1] = IntersectionChildResume{Continuation: cont.GetSecondContinuation(), Started: cont.GetSecondStarted()}
	}
	for i := 2; i < n; i++ {
		cs := cont.OtherChildState[i-2]
		out[i] = IntersectionChildResume{Continuation: cs.GetContinuation(), Started: cs.GetStarted()}
	}
	return out, nil
}

func (c *intersectionCursor[T]) Close() error {
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

func (c *intersectionCursor[T]) IsClosed() bool { return c.closed }

// intersectionMultiCursor merges multiple ordered cursors, returning, for
// each set of matching elements (by comparison key), the list of matching
// elements — one per child. This differs from intersectionCursor, which
// returns only the first child's element. Mirrors Java's
// IntersectionMultiCursor (getNextResult collects every child's result).
//
// It is used by RecordQueryMultiIntersectionOnValuesPlan, where each child
// contributes a distinct aggregate column that must be picked up into the
// merged result row — keeping only the first child would drop every other
// aggregate.
type intersectionMultiCursor[T any] struct {
	children []*mergeChildState[T]
	reverse  bool
	started  bool
	closed   bool
}

// IntersectionMulti creates a merge-intersection cursor that returns, for
// each set of elements present in ALL child cursors (by comparison key),
// the list of those elements (index i is the element from child i). All
// cursors must be ordered by the same key. Matches Java's
// IntersectionMultiCursor.create().
func IntersectionMulti[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	reverse bool,
) RecordCursor[[]T] {
	return IntersectionMultiResume(cursors, compKeyFunc, reverse, nil)
}

// IntersectionMultiResume is IntersectionMulti with per-child resume state
// (see IntersectionResume). resume may be nil (all children fresh).
func IntersectionMultiResume[T any](
	cursors []RecordCursor[T],
	compKeyFunc ComparisonKeyFunc[T],
	reverse bool,
	resume []IntersectionChildResume,
) RecordCursor[[]T] {
	if len(cursors) == 0 {
		return Empty[[]T]()
	}
	return &intersectionMultiCursor[T]{
		children: newMergeChildren(cursors, compKeyFunc, resume),
		reverse:  reverse,
	}
}

func (c *intersectionMultiCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[[]T], error) {
	if c.closed {
		return NewResultNoNext[[]T](SourceExhausted, &EndContinuation{}), nil
	}

	if !c.started {
		for _, child := range c.children {
			if err := child.advance(ctx); err != nil {
				return NewResultNoNext[[]T](SourceExhausted, &EndContinuation{}), err
			}
		}
		c.started = true
	}

	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[[]T]{}, err
		}
		// If any child has no result, the intersection can produce no more on
		// this pass. Distinguish exhaustion (truly done → END) from an
		// out-of-band limit (Scan/Byte/Time → checkpoint and propagate the
		// reason so the caller can resume), mirroring intersectionCursor — a
		// bare END here would silently terminate the intersection on a
		// limit-page and drop the remaining groups (RFC-071).
		for _, child := range c.children {
			if !child.hasResult {
				reason := weakestNoNextReason(c.children)
				if reason.IsSourceExhausted() {
					return NewResultNoNext[[]T](SourceExhausted, &EndContinuation{}), nil
				}
				cont, contErr := buildIntersectionContinuation(c.children)
				if contErr != nil {
					return RecordCursorResult[[]T]{}, contErr
				}
				return NewResultNoNext[[]T](reason, cont), nil
			}
		}

		// Find the maximum comparison key.
		maxKey := c.children[0].comparisonKey
		for _, child := range c.children[1:] {
			cmp, cmpErr := compareKeys(child.comparisonKey, maxKey)
			if cmpErr != nil {
				return RecordCursorResult[[]T]{}, cmpErr
			}
			if c.reverse {
				cmp = -cmp
			}
			if cmp > 0 {
				maxKey = child.comparisonKey
			}
		}

		// Check if all children agree on the max key.
		allMatch := true
		for _, child := range c.children {
			eq, eqErr := compareKeys(child.comparisonKey, maxKey)
			if eqErr != nil {
				return RecordCursorResult[[]T]{}, eqErr
			}
			if eq != 0 {
				allMatch = false
				break
			}
		}

		if allMatch {
			results := make([]T, len(c.children))
			for i, child := range c.children {
				results[i] = child.result.GetValue()
			}
			// consume() then capture BEFORE the in-memory advance — identical to
			// intersectionCursor (shared buildIntersectionContinuation). The
			// executor decodes it via DecodeIntersectionContinuation +
			// IntersectionMultiResume to resume each child from its saved
			// position (RFC-071).
			for _, child := range c.children {
				child.consume()
			}
			cont, contErr := buildIntersectionContinuation(c.children)
			if contErr != nil {
				return RecordCursorResult[[]T]{}, contErr
			}
			for _, child := range c.children {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[[]T](SourceExhausted, &EndContinuation{}), err
				}
			}
			return NewResultWithValue[[]T](results, cont), nil
		}

		// Advance all non-maximal children toward the max key.
		for _, child := range c.children {
			neq, neqErr := compareKeys(child.comparisonKey, maxKey)
			if neqErr != nil {
				return RecordCursorResult[[]T]{}, neqErr
			}
			if neq != 0 {
				if err := child.advance(ctx); err != nil {
					return NewResultNoNext[[]T](SourceExhausted, &EndContinuation{}), err
				}
			}
		}
	}
}

func (c *intersectionMultiCursor[T]) Close() error {
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

func (c *intersectionMultiCursor[T]) IsClosed() bool { return c.closed }
