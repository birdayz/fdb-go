package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// intCompKey extracts an int as the comparison key.
func intCompKey(v int) tuple.Tuple {
	return tuple.Tuple{v}
}

func TestIntersectionCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 4, 5})
	c2 := FromList([]int{2, 4, 6, 8})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int{2, 4}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorNoOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 3, 5})
	c2 := FromList([]int{2, 4, 6})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	if len(results) != 0 {
		t.Fatalf("expected no results, got %v", results)
	}
}

func TestIntersectionCursorReverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{5, 4, 3, 2, 1})
	c2 := FromList([]int{8, 6, 4, 2})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, true)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int{4, 2}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorAllMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3})
	c2 := FromList([]int{1, 2, 3})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int{1, 2, 3}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
}

func TestIntersectionCursorThree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 4, 5, 6})
	c2 := FromList([]int{2, 3, 5, 6, 8})
	c3 := FromList([]int{3, 5, 7, 9})
	inter := Intersection([]RecordCursor[int]{c1, c2, c3}, intCompKey, false)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int{3, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("one_empty", func(t *testing.T) {
		t.Parallel()
		c1 := FromList([]int{1, 2, 3})
		c2 := Empty[int]()
		inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

		result, err := inter.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})

	t.Run("no_cursors", func(t *testing.T) {
		t.Parallel()
		inter := Intersection([]RecordCursor[int]{}, intCompKey, false)
		result, err := inter.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})
}

func TestCompareKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     tuple.Tuple
		expected int
	}{
		{"equal", tuple.Tuple{1, "a"}, tuple.Tuple{1, "a"}, 0},
		{"less_first", tuple.Tuple{1, "a"}, tuple.Tuple{2, "a"}, -1},
		{"greater_first", tuple.Tuple{2, "a"}, tuple.Tuple{1, "a"}, 1},
		{"less_second", tuple.Tuple{1, "a"}, tuple.Tuple{1, "b"}, -1},
		{"shorter", tuple.Tuple{1}, tuple.Tuple{1, "a"}, -1},
		{"longer", tuple.Tuple{1, "a"}, tuple.Tuple{1}, 1},
		{"nil_first", tuple.Tuple{nil, "a"}, tuple.Tuple{1, "a"}, -1},
		{"both_nil", tuple.Tuple{nil}, tuple.Tuple{nil}, 0},
		{"empty", tuple.Tuple{}, tuple.Tuple{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := compareKeys(tt.a, tt.b)
			if err != nil {
				t.Fatalf("compareKeys(%v, %v): unexpected error: %v", tt.a, tt.b, err)
			}
			if (tt.expected < 0 && got >= 0) || (tt.expected > 0 && got <= 0) || (tt.expected == 0 && got != 0) {
				t.Fatalf("compareKeys(%v, %v): got %d, want sign of %d", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

// intPausingCursor emits its rows with position continuations, then pauses
// out-of-band — models a child hitting a scan/time limit mid-catch-up.
type intPausingCursor struct {
	rows   []int
	pos    int
	closed bool
}

func (p *intPausingCursor) OnNext(context.Context) (RecordCursorResult[int], error) {
	if p.pos < len(p.rows) {
		v := p.rows[p.pos]
		p.pos++
		return NewResultWithValue(v, NewBytesContinuation([]byte{byte(p.pos)})), nil
	}
	return NewResultNoNext[int](TimeLimitReached, NewBytesContinuation([]byte{0xEE})), nil
}
func (p *intPausingCursor) Close() error   { p.closed = true; return nil }
func (p *intPausingCursor) IsClosed() bool { return p.closed }

// TestIntersectionCursor_NonMaxDiscardAdvancesContinuation pins Java
// IntersectionCursorBase.computeNextResultStates: every DISCARDED non-max row
// is consume()d, so the child's continuation slot advances past each discard
// — a stop mid-catch-up resumes from the last discard, never re-scanning the
// inter-match gap. Without the consume, the slot sits at the last MATCH
// (START here) and the resumed child re-reads every discarded row.
func TestIntersectionCursor_NonMaxDiscardAdvancesContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Child A discards 1 and 2 chasing B's max key 5, then HOLDS 9; the
	// stop comes from B pausing while chasing 9. A's slot must sit at the
	// position after its LAST DISCARD (list position 2), not at START.
	a := FromList([]int{1, 2, 9})
	b := &intPausingCursor{rows: []int{5}}
	inter := Intersection([]RecordCursor[int]{a, b}, intCompKey, false)

	res, err := inter.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if res.HasNext() {
		t.Fatalf("no intersection expected before the pause, got %v", res.GetValue())
	}
	if res.GetNoNextReason() != TimeLimitReached {
		t.Fatalf("reason = %v, want the child's TimeLimitReached", res.GetNoNextReason())
	}
	contBytes, cerr := res.GetContinuation().ToBytes()
	if cerr != nil {
		t.Fatalf("ToBytes: %v", cerr)
	}
	slots, derr := DecodeIntersectionContinuation(contBytes, 2)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	// A's slot: list position 2 (after discards of 1 and 2, before the held
	// 9) — the 4-byte big-endian ListCursor encoding.
	if !slots[0].Started || string(slots[0].Continuation) != string([]byte{0, 0, 0, 2}) {
		t.Fatalf("child A slot = %+v, want position-2 after its discards (Java consume() on non-max)", slots[0])
	}
}
