package recordlayer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// sliceResumeCursor is a deterministic, continuation-resumable in-memory cursor
// over a sorted []int64 — the test double the SQL path's time-based pagination
// can't provide. Its continuation encodes the next index (single byte; test
// slices are tiny), so a child rebuilt from that continuation resumes exactly
// where it left off, exercising the RFC-071 per-child resume path end to end.
type sliceResumeCursor struct {
	items  []int64
	pos    int
	closed bool
}

func newSliceResumeCursor(items []int64, cont []byte) *sliceResumeCursor {
	pos := 0
	if len(cont) > 0 {
		pos = int(cont[0])
	}
	return &sliceResumeCursor{items: items, pos: pos}
}

func (c *sliceResumeCursor) OnNext(_ context.Context) (RecordCursorResult[int64], error) {
	if c.pos >= len(c.items) {
		return NewResultNoNext[int64](SourceExhausted, &EndContinuation{}), nil
	}
	v := c.items[c.pos]
	c.pos++
	return NewResultWithValue[int64](v, NewBytesContinuation([]byte{byte(c.pos)})), nil
}

func (c *sliceResumeCursor) Close() error   { c.closed = true; return nil }
func (c *sliceResumeCursor) IsClosed() bool { return c.closed }

func intResumeKey(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }

// pageIntersection drives an intersection one row per page — the exact loop
// executeIntersection runs (decode the parent continuation, rebuild each child
// from its per-child slice, seed `started`, IntersectionResume) — so a
// duplicate/omission bug across a continuation boundary surfaces here.
func pageIntersection(t *testing.T, srcs [][]int64) []int64 {
	t.Helper()
	var cont []byte
	var got []int64
	for iter := 0; iter < 10000; iter++ {
		resume, err := DecodeIntersectionContinuation(cont, len(srcs))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		cursors := make([]RecordCursor[int64], len(srcs))
		for i, src := range srcs {
			if resume[i].Started && len(resume[i].Continuation) == 0 {
				cursors[i] = Empty[int64]() // END: exhausted child
			} else {
				cursors[i] = newSliceResumeCursor(src, resume[i].Continuation)
			}
		}
		cur := IntersectionResume(cursors, intResumeKey, false, resume)
		res, err := cur.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			cur.Close()
			if res.GetContinuation().IsEnd() {
				return got
			}
			b, _ := res.GetContinuation().ToBytes()
			cont = b
			continue
		}
		got = append(got, res.GetValue())
		b, _ := res.GetContinuation().ToBytes()
		cur.Close()
		cont = b
	}
	t.Fatal("pageIntersection did not terminate")
	return nil
}

func pageMultiIntersection(t *testing.T, srcs [][]int64) []int64 {
	t.Helper()
	var cont []byte
	var got []int64
	for iter := 0; iter < 10000; iter++ {
		resume, err := DecodeIntersectionContinuation(cont, len(srcs))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		cursors := make([]RecordCursor[int64], len(srcs))
		for i, src := range srcs {
			if resume[i].Started && len(resume[i].Continuation) == 0 {
				cursors[i] = Empty[int64]()
			} else {
				cursors[i] = newSliceResumeCursor(src, resume[i].Continuation)
			}
		}
		cur := IntersectionMultiResume(cursors, intResumeKey, false, resume)
		res, err := cur.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			cur.Close()
			if res.GetContinuation().IsEnd() {
				return got
			}
			b, _ := res.GetContinuation().ToBytes()
			cont = b
			continue
		}
		// Each multi result is one element per child; all equal the key.
		got = append(got, res.GetValue()[0])
		b, _ := res.GetContinuation().ToBytes()
		cur.Close()
		cont = b
	}
	t.Fatal("pageMultiIntersection did not terminate")
	return nil
}

func TestIntersectionResume_PagedNoDupNoLoss(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		srcs [][]int64
		want []int64
	}{
		{"common", [][]int64{{1, 2, 3, 4, 5, 6}, {2, 4, 6, 8}}, []int64{2, 4, 6}},
		{"all_match", [][]int64{{1, 2, 3}, {1, 2, 3}}, []int64{1, 2, 3}},
		{"no_common", [][]int64{{1, 3, 5}, {2, 4, 6}}, nil},
		{"asymmetric_exhaustion", [][]int64{{1, 2, 3, 4, 5}, {3}}, []int64{3}},
		{"three_children", [][]int64{{1, 2, 3, 4}, {2, 3, 4, 5}, {3, 4, 6}}, []int64{3, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pageIntersection(t, tc.srcs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("paged intersection = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIntersectionMultiResume_PagedNoDupNoLoss(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		srcs [][]int64
		want []int64
	}{
		{"common", [][]int64{{1, 2, 3, 4, 5, 6}, {2, 4, 6, 8}}, []int64{2, 4, 6}},
		{"all_match", [][]int64{{1, 2, 3}, {1, 2, 3}}, []int64{1, 2, 3}},
		{"no_common", [][]int64{{1, 3, 5}, {2, 4, 6}}, nil},
		{"three_children", [][]int64{{1, 2, 3, 4}, {2, 3, 4, 5}, {3, 4, 6}}, []int64{3, 4}},
		{"asymmetric_exhaustion", [][]int64{{1, 2, 3, 4, 5}, {3}}, []int64{3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pageMultiIntersection(t, tc.srcs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("paged multi-intersection = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecodeIntersectionContinuation_RoundTrip pins the encode↔decode symmetry,
// especially that an exhausted child round-trips as END (started + empty), NOT
// START — the exact property the per-child started flag exists to guarantee
// (RFC-071 review).
func TestDecodeIntersectionContinuation_RoundTrip(t *testing.T) {
	t.Parallel()
	// child0: mid-stream cached continuation (non-empty bytes) → MID.
	// child1: exhausted cached continuation (EndContinuation) → END.
	// child2: start cached continuation (StartContinuation, empty+not-end) → START.
	children := []*mergeChildState[int64]{
		{continuation: NewBytesContinuation([]byte{5})},
		{continuation: &EndContinuation{}},
		{continuation: &StartContinuation{}},
	}
	cont, err := buildIntersectionContinuation(children)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	got, err := DecodeIntersectionContinuation(data, 3)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// child0 → MID: started + non-empty bytes.
	if !got[0].Started || len(got[0].Continuation) == 0 {
		t.Errorf("child0: want MID (started+bytes), got %+v", got[0])
	}
	// child1 → END: started + empty bytes (must NOT be START).
	if !got[1].Started {
		t.Errorf("child1: exhausted child must round-trip as END (started=true), got %+v", got[1])
	}
	if len(got[1].Continuation) != 0 {
		t.Errorf("child1: END must have empty continuation, got %+v", got[1])
	}
	// child2 → START: not started.
	if got[2].Started {
		t.Errorf("child2: never-started child must round-trip as START (started=false), got %+v", got[2])
	}
}

// limitOnceCursor returns a single out-of-band limit stop with a
// StartContinuation (empty bytes, NOT end) — the shape an index scan produces
// when it hits a scan/byte limit before emitting its first row. It models the
// case the RFC-071 review flagged: such a child must round-trip as START
// (resume re-reads it), never END (which would Empty() it and silently drop the
// rest of the intersection).
type limitOnceCursor struct{ closed bool }

func (c *limitOnceCursor) OnNext(_ context.Context) (RecordCursorResult[int64], error) {
	return NewResultNoNext[int64](ScanLimitReached, &StartContinuation{}), nil
}
func (c *limitOnceCursor) Close() error   { c.closed = true; return nil }
func (c *limitOnceCursor) IsClosed() bool { return c.closed }

// TestIntersectionResume_LimitBeforeFirstRow_NoLoss is the required
// regression: child B hits a scan limit before its first row on page 1 (while
// child A has loaded its first value), the intersection checkpoints, and on
// resume NO match is lost — in particular the match A had already loaded (2)
// must survive because its cached continuation was captured BEFORE the held
// value, not after.
func TestIntersectionResume_LimitBeforeFirstRow_NoLoss(t *testing.T) {
	t.Parallel()
	a := []int64{2, 4, 6}
	b := []int64{2, 4, 6}
	bLimitedOnce := false
	childFrom := func(src []int64, r IntersectionChildResume) RecordCursor[int64] {
		if r.Started && len(r.Continuation) == 0 {
			return Empty[int64]()
		}
		return newSliceResumeCursor(src, r.Continuation)
	}
	var cont []byte
	var got []int64
	for iter := 0; iter < 1000; iter++ {
		resume, err := DecodeIntersectionContinuation(cont, 2)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		cursors := make([]RecordCursor[int64], 2)
		cursors[0] = childFrom(a, resume[0])
		// child B hits a scan limit before its first row, exactly once, on the
		// very first (fresh) page.
		if !bLimitedOnce && !resume[1].Started && len(resume[1].Continuation) == 0 {
			bLimitedOnce = true
			cursors[1] = &limitOnceCursor{}
		} else {
			cursors[1] = childFrom(b, resume[1])
		}
		cur := IntersectionResume(cursors, intResumeKey, false, resume)
		res, err := cur.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			cur.Close()
			if res.GetContinuation().IsEnd() {
				break
			}
			bts, _ := res.GetContinuation().ToBytes()
			cont = bts
			continue
		}
		got = append(got, res.GetValue())
		bts, _ := res.GetContinuation().ToBytes()
		cur.Close()
		cont = bts
	}
	if !bLimitedOnce {
		t.Fatal("test bug: limit branch never fired")
	}
	if !reflect.DeepEqual(got, []int64{2, 4, 6}) {
		t.Errorf("limit-before-first-row lost matches: got %v, want [2 4 6]", got)
	}
}

// TestIntersectionMultiResume_LimitMidStream_NoLoss is the multi-cursor analog
// of TestIntersectionResume_LimitBeforeFirstRow_NoLoss: it pins that
// intersectionMultiCursor checkpoints (rather than returning a bare END) when a
// child hits an out-of-band limit before its first row, so no match is dropped
// on resume. Without the stopped-child fix the multi cursor silently terminated
// the intersection on any limit, losing the remaining matches.
func TestIntersectionMultiResume_LimitMidStream_NoLoss(t *testing.T) {
	t.Parallel()
	a := []int64{2, 4, 6}
	b := []int64{2, 4, 6}
	bLimitedOnce := false
	childFrom := func(src []int64, r IntersectionChildResume) RecordCursor[int64] {
		if r.Started && len(r.Continuation) == 0 {
			return Empty[int64]()
		}
		return newSliceResumeCursor(src, r.Continuation)
	}
	var cont []byte
	var got []int64
	for iter := 0; iter < 1000; iter++ {
		resume, err := DecodeIntersectionContinuation(cont, 2)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		cursors := make([]RecordCursor[int64], 2)
		cursors[0] = childFrom(a, resume[0])
		if !bLimitedOnce && !resume[1].Started && len(resume[1].Continuation) == 0 {
			bLimitedOnce = true
			cursors[1] = &limitOnceCursor{}
		} else {
			cursors[1] = childFrom(b, resume[1])
		}
		cur := IntersectionMultiResume(cursors, intResumeKey, false, resume)
		res, err := cur.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !res.HasNext() {
			cur.Close()
			if res.GetContinuation().IsEnd() {
				break
			}
			bts, _ := res.GetContinuation().ToBytes()
			cont = bts
			continue
		}
		got = append(got, res.GetValue()[0])
		bts, _ := res.GetContinuation().ToBytes()
		cur.Close()
		cont = bts
	}
	if !bLimitedOnce {
		t.Fatal("test bug: limit branch never fired")
	}
	if !reflect.DeepEqual(got, []int64{2, 4, 6}) {
		t.Errorf("multi-intersection lost matches on limit-stop resume: got %v, want [2 4 6]", got)
	}
}

func TestDecodeIntersectionContinuation_NilIsAllFresh(t *testing.T) {
	t.Parallel()
	got, err := DecodeIntersectionContinuation(nil, 3)
	if err != nil {
		t.Fatalf("nil continuation must be all-fresh, got error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	for i, c := range got {
		if c.Started || len(c.Continuation) != 0 {
			t.Errorf("child %d: nil continuation must be START (fresh), got %+v", i, c)
		}
	}
}

func TestDecodeIntersectionContinuation_Errors(t *testing.T) {
	t.Parallel()
	// Corrupt proto bytes → hard error.
	if _, err := DecodeIntersectionContinuation([]byte{0xff, 0xff, 0xff, 0xff}, 2); err == nil {
		t.Error("corrupt proto must be a hard error")
	}
	// Child-count mismatch: a continuation built for 3 children decoded as 2.
	children := []*mergeChildState[int64]{
		{continuation: NewBytesContinuation([]byte{1})},
		{continuation: NewBytesContinuation([]byte{1})},
		{continuation: NewBytesContinuation([]byte{1})},
	}
	cont, err := buildIntersectionContinuation(children)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data, _ := cont.ToBytes()
	if _, err := DecodeIntersectionContinuation(data, 2); err == nil {
		t.Error("child-count mismatch (3 encoded, n=2) must be a hard error")
	}

	// Wrong first/second count — not caught by an OtherChildState-length check
	// alone (it is 0 for n<=2), so these exercise the presence validation.
	// 2-child token decoded as n=1 (second present but n=1 → mismatch).
	two, _ := buildIntersectionContinuation([]*mergeChildState[int64]{
		{continuation: NewBytesContinuation([]byte{1})},
		{continuation: NewBytesContinuation([]byte{1})},
	})
	d2, _ := two.ToBytes()
	if _, err := DecodeIntersectionContinuation(d2, 1); err == nil {
		t.Error("2-child token decoded as n=1 must be a hard error")
	}
	// 1-child token decoded as n=2 (second absent but n=2 → mismatch).
	one, _ := buildIntersectionContinuation([]*mergeChildState[int64]{
		{continuation: NewBytesContinuation([]byte{1})},
	})
	d1, _ := one.ToBytes()
	if _, err := DecodeIntersectionContinuation(d1, 2); err == nil {
		t.Error("1-child token decoded as n=2 must be a hard error")
	}
}

// stopAfterFirstCursor emits its inner's first row, then reports an
// out-of-band limit carrying the inner's post-row continuation (the
// honest stop position of a real limited cursor).
type stopAfterFirstCursor struct {
	inner   RecordCursor[int64]
	stopAt  RecordCursorContinuation
	emitted bool
	closed  bool
}

func (c *stopAfterFirstCursor) OnNext(ctx context.Context) (RecordCursorResult[int64], error) {
	if c.emitted {
		return NewResultNoNext[int64](ScanLimitReached, c.stopAt), nil
	}
	c.emitted = true
	r, err := c.inner.OnNext(ctx)
	if err == nil && r.HasNext() {
		c.stopAt = r.GetContinuation()
	}
	return r, err
}
func (c *stopAfterFirstCursor) Close() error   { c.closed = true; return c.inner.Close() }
func (c *stopAfterFirstCursor) IsClosed() bool { return c.closed }

// TestIntersectionMultiResume_DiscardedRowsConsumed pins Java
// IntersectionCursorBase.computeNextResultStates: a non-maximal child's
// discarded row is CONSUMED (its cached continuation advances past it),
// so a checkpoint triggered while that child HOLDS a later, unmatched
// row resumes it after the DISCARD — never re-scanning the inter-match
// gap. Without consume the cache still points before the discarded row
// (correct rows on resume, wasted I/O — the binary intersectionCursor
// already had the consume; the multi cursor lacked it).
func TestIntersectionMultiResume_DiscardedRowsConsumed(t *testing.T) {
	t.Parallel()
	// Sequence: A=1 vs B=2 → A discards 1 (consume) and loads 3 (held,
	// unmatched). Then B discards 2 toward 3 and stops with a limit.
	// Checkpoint: A's saved position must be AFTER the discarded 1 and
	// BEFORE the held 3.
	a := []int64{1, 3}
	cursors := []RecordCursor[int64]{
		newSliceResumeCursor(a, nil),
		&stopAfterFirstCursor{inner: newSliceResumeCursor([]int64{2, 4}, nil)},
	}
	resume := make([]IntersectionChildResume, 2)
	cur := IntersectionMultiResume(cursors, intResumeKey, false, resume)
	res, err := cur.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if res.HasNext() {
		t.Fatalf("expected a checkpoint stop, got value %v", res.GetValue())
	}
	if res.GetContinuation().IsEnd() {
		t.Fatal("expected a resumable checkpoint, got END")
	}
	bts, err := res.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("checkpoint bytes: %v", err)
	}
	cur.Close()

	decoded, err := DecodeIntersectionContinuation(bts, 2)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Child A's saved position: past the discarded 1, before the held 3 —
	// resuming yields 3 first. The consume-less shape saved the
	// pre-discard position and re-read 1.
	resumedA := newSliceResumeCursor(a, decoded[0].Continuation)
	first, err := resumedA.OnNext(context.Background())
	if err != nil {
		t.Fatalf("resumed A OnNext: %v", err)
	}
	if !first.HasNext() || first.GetValue() != int64(3) {
		t.Fatalf("resumed child A must start AFTER the discarded row: got %v (HasNext=%v), want 3", first.GetValue(), first.HasNext())
	}
}

// errReadAhead is returned by errorAfterOneCursor's second call — distinct from
// any sentinel the production code might match on, so a test failure clearly
// names its source.
var errReadAhead = errors.New("errorAfterOneCursor: simulated read-ahead failure")

// errorAfterOneCursor emits a single value, then errors on every subsequent
// call — modeling a child whose NEXT pull (a statement-timeout tick, a
// cancelled context, a transient FDB error) fails right after a row was
// loaded. It never errors on the call that loads its only value, so a bug
// that folds the read-ahead pull into the SAME OnNext call as a delivered
// match will observe the error before ever returning that match.
type errorAfterOneCursor struct {
	value   int64
	emitted bool
	closed  bool
}

func (c *errorAfterOneCursor) OnNext(_ context.Context) (RecordCursorResult[int64], error) {
	if !c.emitted {
		c.emitted = true
		return NewResultWithValue[int64](c.value, NewBytesContinuation([]byte{1})), nil
	}
	return RecordCursorResult[int64]{}, errReadAhead
}

func (c *errorAfterOneCursor) Close() error   { c.closed = true; return nil }
func (c *errorAfterOneCursor) IsClosed() bool { return c.closed }

// TestIntersectionCursor_MatchDeliveredBeforeReadAheadError is the required
// regression for the read-ahead bug: both children yield 42 exactly once and
// then error on their NEXT pull. A match on 42 is fully decided (both
// children's comparison keys agree) before either child is asked again, so
// intersectionCursor.OnNext must return that match — HasNext=true, value=42,
// err=nil — on the FIRST call. The read-ahead pull (and its error) may only
// surface on a SECOND, later call to OnNext, exactly as Java's
// MergeCursor.onNext (MergeCursor.java:294-304) returns immediately after
// resultStates.forEach(consume) with no further pull; the next child fetch
// happens lazily at the top of the NEXT call (MergeCursorState.java:66-74,
// getOnNextFuture's null-check memoization).
//
// The pre-fix code called child.advance(ctx) on every child INSIDE the
// allMatch branch, before returning — so this test reds without the fix: the
// first OnNext call itself returns errReadAhead and the match is never
// delivered.
func TestIntersectionCursor_MatchDeliveredBeforeReadAheadError(t *testing.T) {
	t.Parallel()
	cursors := []RecordCursor[int64]{
		&errorAfterOneCursor{value: 42},
		&errorAfterOneCursor{value: 42},
	}
	cur := Intersection(cursors, intResumeKey, false)
	defer cur.Close()

	res, err := cur.OnNext(context.Background())
	if err != nil {
		t.Fatalf("the already-decided match must be delivered before the read-ahead error surfaces, got error: %v", err)
	}
	if !res.HasNext() || res.GetValue() != int64(42) {
		t.Fatalf("want match 42, got HasNext=%v value=%v", res.HasNext(), res.GetValue())
	}

	// The read-ahead pull is deferred to this second call.
	_, err = cur.OnNext(context.Background())
	if !errors.Is(err, errReadAhead) {
		t.Fatalf("want the deferred read-ahead error on the second call, got: %v", err)
	}
}

// TestIntersectionMultiCursor_MatchDeliveredBeforeReadAheadError is the
// intersectionMultiCursor analog — identical shape
// (intersectionMultiCursor.OnNext had the same inline advance-after-consume
// bug as intersectionCursor.OnNext).
func TestIntersectionMultiCursor_MatchDeliveredBeforeReadAheadError(t *testing.T) {
	t.Parallel()
	cursors := []RecordCursor[int64]{
		&errorAfterOneCursor{value: 42},
		&errorAfterOneCursor{value: 42},
	}
	cur := IntersectionMulti(cursors, intResumeKey, false)
	defer cur.Close()

	res, err := cur.OnNext(context.Background())
	if err != nil {
		t.Fatalf("the already-decided match must be delivered before the read-ahead error surfaces, got error: %v", err)
	}
	if !res.HasNext() || len(res.GetValue()) != 2 || res.GetValue()[0] != 42 || res.GetValue()[1] != 42 {
		t.Fatalf("want match [42 42], got HasNext=%v value=%v", res.HasNext(), res.GetValue())
	}

	_, err = cur.OnNext(context.Background())
	if !errors.Is(err, errReadAhead) {
		t.Fatalf("want the deferred read-ahead error on the second call, got: %v", err)
	}
}
