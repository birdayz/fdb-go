package recordlayer

import (
	"context"
	"reflect"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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

func intResumeKey(v int64) tuple.Tuple { return tuple.Tuple{v} }

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
// (Graefe + Torvalds RFC-071 review).
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
// case Graefe's RFC-071 review flagged: such a child must round-trip as START
// (resume re-reads it), never END (which would Empty() it and silently drop the
// rest of the intersection).
type limitOnceCursor struct{ closed bool }

func (c *limitOnceCursor) OnNext(_ context.Context) (RecordCursorResult[int64], error) {
	return NewResultNoNext[int64](ScanLimitReached, &StartContinuation{}), nil
}
func (c *limitOnceCursor) Close() error   { c.closed = true; return nil }
func (c *limitOnceCursor) IsClosed() bool { return c.closed }

// TestIntersectionResume_LimitBeforeFirstRow_NoLoss is the regression Graefe
// asked for: child B hits a scan limit before its first row on page 1 (while
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
}
