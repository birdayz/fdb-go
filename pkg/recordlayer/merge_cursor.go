package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// ComparisonKeyFunc extracts a comparison key from a cursor element.
// The returned tuple is used for merge ordering via FDB's order-preserving
// tuple encoding. Matches Java's KeyedMergeCursorState.comparisonKeyFunction:
// there a key-extraction failure propagates as a RecordCoreException through
// the cursor's future chain — an error to the caller, never a crash — so the
// Go closure carries an error return, which advance() surfaces as the child
// cursor's error.
type ComparisonKeyFunc[T any] func(T) (tuple.Tuple, error)

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
	// Java IntersectionCursorBase.computeNextResultStates additionally calls
	// consume() on every DISCARDED non-max cursor, so the cached continuation
	// advances past each discarded row too — an out-of-band stop resumes a
	// child from its last DISCARD, never re-scanning the inter-match gap. Go
	// mirrors that in the non-max advance loop.
	continuation RecordCursorContinuation
	// needsAdvance mirrors Java's MergeCursorState.onNextFuture == null: true
	// means the cached result/comparisonKey is stale (never fetched, or its
	// value was already consumed as part of a delivered/discarded row) and
	// must be refreshed before this child can participate in the next
	// max-key comparison. consume() sets it; advance() clears it. Pulling
	// only happens where this flag says to — at the top of the merge loop,
	// on the NEXT call — never inline with the row that set it, which is
	// what keeps a read-ahead error from being bundled with (and discarding)
	// a match that was already decided.
	needsAdvance bool
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
	s.needsAdvance = false
	if s.hasResult {
		key, keyErr := s.compKeyFunc(result.GetValue())
		if keyErr != nil {
			return keyErr
		}
		s.comparisonKey = key
	} else {
		s.comparisonKey = nil
		s.continuation = result.GetContinuation()
	}
	return nil
}

// consume records that this child's current value was emitted (or discarded)
// as part of a merge result, advancing the cached continuation past it and
// marking the child stale (Java MergeCursorState.consume: nulls onNextFuture,
// updates continuation). Deliberately NO I/O here — the next pull is deferred
// to wherever the merge loop next checks needsAdvance, matching Java's lazy
// getOnNextFuture() memoization.
func (s *mergeChildState[T]) consume() {
	s.continuation = s.result.GetContinuation()
	s.needsAdvance = true
}

// NOTE: there is deliberately NO merge-union cursor in this file. The
// wire-format UnionContinuation encoder lives in the executor's
// mergeSortCursor (pkg/recordlayer/query/executor/executor_new_plans.go) — the
// single union pipeline. A previous unionCursor here snapshotted
// peeked-but-unconsumed children's POST-peek continuations, so a resume
// skipped their held rows; it was dead code (zero production callers) and was
// removed rather than fixed.

// --- IntersectionCursor ---

// intersectionCursor merges multiple ordered cursors, returning only elements
// that appear in ALL cursors (by comparison key). Uses merge-intersection:
// finds the maximum key, then advances non-maximal cursors until all agree.
// Matches Java's IntersectionCursor.
type intersectionCursor[T any] struct {
	children []*mergeChildState[T]
	reverse  bool
	closed   bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java MergeCursor.onNext: once stopped, every later onNext returns the
	// SAME cached result). Without it a re-call recomputes the terminal —
	// re-encoding every child's lazy continuation — instead of replaying it
	// verbatim.
	lastNoNext *RecordCursorResult[T]
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
			needsAdvance: true, // first pull is due, same as any post-consume() child
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
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	// Merge-intersection loop: advance non-maximal cursors until all agree
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}

		// Pull a fresh value for every child whose current one is stale — the
		// first pull (needsAdvance seeded true at construction) or one already
		// delivered/discarded by a PRIOR call to this method (consume() marks
		// it). Java: whenAll(cursorStates) at the top of
		// computeNextResultStates' loop body (IntersectionCursorBase.java:113),
		// where MergeCursorState.getOnNextFuture only actually pulls when its
		// memoized onNextFuture is null (MergeCursorState.java:66-74) — exactly
		// the transitions consume()/advance() track here via needsAdvance. This
		// is what makes a match's read-ahead LAZY: the row delivered by a PRIOR
		// call marked its children needsAdvance and returned immediately
		// without pulling (see the allMatch branch below) — the pull, and any
		// error it can produce, happens here, on the FOLLOWING call, never
		// bundled with the row it followed.
		for _, child := range c.children {
			if child.needsAdvance {
				if err := child.advance(ctx); err != nil {
					return RecordCursorResult[T]{}, err
				}
			}
		}

		// Check if any child is exhausted
		for _, child := range c.children {
			if !child.hasResult {
				reason := weakestNoNextReason(c.children)
				if reason.IsSourceExhausted() {
					res := NewResultNoNext[T](SourceExhausted, &EndContinuation{})
					c.lastNoNext = &res
					return res, nil
				}
				cont, contErr := c.buildContinuation()
				if contErr != nil {
					return RecordCursorResult[T]{}, contErr
				}
				res := NewResultNoNext[T](reason, cont)
				c.lastNoNext = &res
				return res, nil
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
			// child's cached continuation past the matched row — bookkeeping
			// only, no I/O (see mergeChildState.consume) — so the continuation
			// captured right after reflects "resume just past this match" (no
			// skip, no dup). The read-ahead pull for the NEXT row is deliberately
			// NOT done here: Java's MergeCursor.onNext (MergeCursor.java:294-304)
			// returns the value immediately after resultStates.forEach(consume),
			// with no further pull — the next child fetch happens lazily, at the
			// top of THIS method on the next call (the needsAdvance step above).
			// Pulling here — as a prior version did — meant a read-ahead error
			// (e.g. ctx cancellation between "match found" and "look ahead")
			// discarded an already-decided match instead of delivering the row
			// it owed and surfacing the error on the NEXT call, where it belongs.
			result := c.children[0].result
			for _, child := range c.children {
				child.consume()
			}
			cont, contErr := c.buildContinuation()
			if contErr != nil {
				return RecordCursorResult[T]{}, contErr
			}
			return NewResultWithValue[T](result.GetValue(), cont), nil
		}

		// Discard all non-maximal children — Java
		// (IntersectionCursorBase.computeNextResultStates) consumes each
		// discarded row first, so the child's cached continuation advances past
		// it — a stop mid-catch-up resumes from the last discard, not the last
		// match. Nothing has been decided yet at this point (no value is being
		// returned to the caller from this iteration), so marking them
		// needsAdvance and re-pulling at the top of the next loop iteration —
		// still within this SAME OnNext call — is exactly as immediate as
		// pulling inline, and any error still propagates from this call.
		for _, child := range c.children {
			neq, neqErr := compareKeys(child.comparisonKey, maxKey)
			if neqErr != nil {
				return RecordCursorResult[T]{}, neqErr
			}
			if neq != 0 {
				child.consume()
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
	closed   bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java MergeCursor.onNext's cached result) — see intersectionCursor.
	lastNoNext *RecordCursorResult[[]T]
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
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[[]T]{}, err
		}

		// Pull a fresh value for every child whose current one is stale — see
		// intersectionCursor.OnNext for the full rationale (Java's whenAll(...)
		// at the top of computeNextResultStates, mirrored here via
		// needsAdvance). This keeps the read-ahead pull lazy: it happens on the
		// call AFTER a match/discard marked a child stale, never bundled with
		// the row that marked it.
		for _, child := range c.children {
			if child.needsAdvance {
				if err := child.advance(ctx); err != nil {
					return RecordCursorResult[[]T]{}, err
				}
			}
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
					res := NewResultNoNext[[]T](SourceExhausted, &EndContinuation{})
					c.lastNoNext = &res
					return res, nil
				}
				cont, contErr := buildIntersectionContinuation(c.children)
				if contErr != nil {
					return RecordCursorResult[[]T]{}, contErr
				}
				res := NewResultNoNext[[]T](reason, cont)
				c.lastNoNext = &res
				return res, nil
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
			// consume() (bookkeeping only, no I/O) then capture the continuation —
			// identical to intersectionCursor (shared buildIntersectionContinuation).
			// The read-ahead pull is deliberately deferred to the next call's
			// needsAdvance step (see intersectionCursor.OnNext) rather than done
			// here, so a read-ahead error can never discard an already-decided
			// match. The executor decodes the continuation via
			// DecodeIntersectionContinuation + IntersectionMultiResume to resume
			// each child from its saved position (RFC-071).
			for _, child := range c.children {
				child.consume()
			}
			cont, contErr := buildIntersectionContinuation(c.children)
			if contErr != nil {
				return RecordCursorResult[[]T]{}, contErr
			}
			return NewResultWithValue[[]T](results, cont), nil
		}

		// Discard all non-maximal children toward the max key. Java
		// (IntersectionCursorBase.computeNextResultStates) consumes each
		// discarded row first, so the child's cached continuation advances
		// past it — a stop mid-catch-up resumes from the last discard, not
		// the last match. Same shape as the binary intersectionCursor: nothing
		// has been decided yet, so marking needsAdvance and re-pulling at the
		// top of the next loop iteration (still this same OnNext call) is exactly
		// as immediate as pulling inline.
		for _, child := range c.children {
			neq, neqErr := compareKeys(child.comparisonKey, maxKey)
			if neqErr != nil {
				return RecordCursorResult[[]T]{}, neqErr
			}
			if neq != 0 {
				child.consume()
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
