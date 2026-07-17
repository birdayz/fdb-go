package recordlayer

import (
	"context"
	"testing"
)

// flakyTerminalCursor returns its rows, then an out-of-band pause ONCE, then
// a DIFFERENT terminal on later pulls. A pass-through cursor missing Java's
// terminal-result cache (SkipCursor/FilterCursor/MapCursor all begin with
// `if (nextResult != null && !nextResult.hasNext()) return nextResult`)
// re-pulls the inner after the pause and leaks the changed result — so this
// fixture makes the missing guard observable where an idempotent inner
// would not.
type flakyTerminalCursor struct {
	rows   []int
	pos    int
	paused bool
	closed bool
}

func (f *flakyTerminalCursor) OnNext(context.Context) (RecordCursorResult[int], error) {
	if f.pos < len(f.rows) {
		v := f.rows[f.pos]
		f.pos++
		return NewResultWithValue(v, NewBytesContinuation([]byte{byte(f.pos)})), nil
	}
	if !f.paused {
		f.paused = true
		return NewResultNoNext[int](TimeLimitReached, NewBytesContinuation([]byte{0xAA})), nil
	}
	return NewResultNoNext[int](SourceExhausted, &EndContinuation{}), nil
}
func (f *flakyTerminalCursor) Close() error   { f.closed = true; return nil }
func (f *flakyTerminalCursor) IsClosed() bool { return f.closed }

// requireReplay drains the cursor to its first no-next, then re-calls OnNext
// and asserts the terminal result is REPLAYED verbatim (same reason, same
// continuation bytes) — Java's cached-terminal contract.
func requireReplay[T any](t *testing.T, c RecordCursor[T]) {
	t.Helper()
	ctx := context.Background()
	var first RecordCursorResult[T]
	for {
		res, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !res.HasNext() {
			first = res
			break
		}
	}
	again, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("re-call: %v", err)
	}
	if again.HasNext() {
		t.Fatal("a re-call after a terminal result must not produce a value")
	}
	if again.GetNoNextReason() != first.GetNoNextReason() {
		t.Fatalf("re-call reason = %v, want the cached %v (Java replays the terminal result)",
			again.GetNoNextReason(), first.GetNoNextReason())
	}
	fb, _ := first.GetContinuation().ToBytes()
	ab, _ := again.GetContinuation().ToBytes()
	if string(fb) != string(ab) {
		t.Fatalf("re-call continuation %v, want the cached %v", ab, fb)
	}
}

// TestTerminalReplay_PassThroughCursors pins Java's terminal-result cache on
// the pass-through combinators: after an out-of-band no-next, a re-call
// replays the identical result and never re-pulls the inner (whose next
// terminal DIFFERS in this fixture — a missing guard surfaces the changed
// reason).
func TestTerminalReplay_PassThroughCursors(t *testing.T) {
	t.Parallel()

	t.Run("skip_mid_skip_pause", func(t *testing.T) {
		t.Parallel()
		// Pause arrives while still skipping (remaining > 0).
		requireReplay(t, SkipCursor[int](&flakyTerminalCursor{rows: []int{1}}, 5))
	})
	t.Run("skip_post_skip_pause", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, SkipCursor[int](&flakyTerminalCursor{rows: []int{1, 2, 3}}, 1))
	})
	t.Run("filter", func(t *testing.T) {
		t.Parallel()
		requireReplay[int](t, &filterCursor[int]{inner: &flakyTerminalCursor{rows: []int{1, 2}}, predicate: func(int) bool { return true }})
	})
	t.Run("map", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, MapCursor[int, int](&flakyTerminalCursor{rows: []int{1, 2}}, func(v int) int { return v * 2 }))
	})
	t.Run("map_err", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, MapErrCursor[int, int](&flakyTerminalCursor{rows: []int{1, 2}}, func(v int) (int, error) { return v * 2, nil }))
	})
	t.Run("dedup", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, Dedup[int](
			func([]byte) RecordCursor[int] { return &flakyTerminalCursor{rows: []int{1, 2}} },
			func(a, b int) bool { return a == b },
			nil, nil, nil,
		))
	})
	t.Run("chained", func(t *testing.T) {
		t.Parallel()
		// Generator is NON-idempotent past exhaustion: it returns nil ONCE,
		// then resurrects with a value — a re-call missing the guard
		// re-invokes it and surfaces the bogus value.
		calls := 0
		gen := func(prev *int) (*int, error) {
			calls++
			switch {
			case calls <= 2:
				v := calls
				return &v, nil
			case calls == 3:
				return nil, nil
			default:
				v := 99
				return &v, nil
			}
		}
		requireReplay(t, Chained[int](gen, func(v int) []byte { return []byte{byte(v)} }, nil, nil))
	})
	t.Run("concat_second_pauses", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, ConcatCursors[int](
			func([]byte) RecordCursor[int] { return FromList([]int{1}) },
			func([]byte) RecordCursor[int] { return &flakyTerminalCursor{rows: []int{2}} },
			nil,
		))
	})
	t.Run("or_else_active_pauses", func(t *testing.T) {
		t.Parallel()
		// Primary yields a row (deciding USE_INNER), then pauses out-of-band.
		requireReplay(t, OrElse[int](
			&flakyTerminalCursor{rows: []int{7}},
			func() RecordCursor[int] { return Empty[int]() },
		))
	})
	t.Run("fallback", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, Fallback[int](
			&flakyTerminalCursor{rows: []int{1}},
			func(*RecordCursorResult[int]) RecordCursor[int] { return Empty[int]() },
		))
	})
	t.Run("auto_continuing", func(t *testing.T) {
		t.Parallel()
		// The auto-continuing cursor only ever SURFACES exhaustion (out-of-band
		// stops are auto-resumed in a new transaction), so its terminal fixture
		// exhausts once, then resurrects with a value on a re-pull.
		requireReplay[int](t, &autoContinuingCursor[int]{currentCursor: &resurrectingCursor{}})
	})
	t.Run("intersection", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, Intersection[int](
			[]RecordCursor[int]{
				&lazyPauseCursor{rows: []int{1, 2}, cont: &countingContinuation{}},
				FromList([]int{1, 2, 3}),
			},
			intCompKey, false,
		))
	})
	t.Run("intersection_multi", func(t *testing.T) {
		t.Parallel()
		requireReplay(t, IntersectionMulti[int](
			[]RecordCursor[int]{
				&lazyPauseCursor{rows: []int{1, 2}, cont: &countingContinuation{}},
				FromList([]int{1, 2, 3}),
			},
			intCompKey, false,
		))
	})
}

// countingContinuation returns DIFFERENT bytes on every ToBytes call — a
// stand-in for a lazy child continuation whose re-encode is not byte-stable.
// The intersection cursors never re-pull a child after a terminal, so the
// observable recompute channel is the continuation REBUILD: a missing guard
// re-encodes each child's continuation and the terminal's bytes drift, while
// the cached replay never re-encodes.
type countingContinuation struct{ calls byte }

func (c *countingContinuation) ToBytes() ([]byte, error) {
	c.calls++
	return []byte{c.calls}, nil
}
func (c *countingContinuation) IsEnd() bool { return false }

// lazyPauseCursor emits its rows, then pauses out-of-band forever with the
// supplied (lazy) continuation.
type lazyPauseCursor struct {
	rows   []int
	pos    int
	closed bool
	cont   RecordCursorContinuation
}

func (f *lazyPauseCursor) OnNext(context.Context) (RecordCursorResult[int], error) {
	if f.pos < len(f.rows) {
		v := f.rows[f.pos]
		f.pos++
		return NewResultWithValue(v, NewBytesContinuation([]byte{byte(f.pos)})), nil
	}
	return NewResultNoNext[int](TimeLimitReached, f.cont), nil
}
func (f *lazyPauseCursor) Close() error   { f.closed = true; return nil }
func (f *lazyPauseCursor) IsClosed() bool { return f.closed }

// resurrectingCursor exhausts on its first pull, then yields a VALUE on any
// later pull — non-idempotent at the terminal, so a wrapper that re-pulls it
// after exhaustion surfaces the bogus value.
type resurrectingCursor struct {
	pulls  int
	closed bool
}

func (f *resurrectingCursor) OnNext(context.Context) (RecordCursorResult[int], error) {
	f.pulls++
	if f.pulls == 1 {
		return NewResultNoNext[int](SourceExhausted, &EndContinuation{}), nil
	}
	return NewResultWithValue(f.pulls, NewBytesContinuation([]byte{1})), nil
}
func (f *resurrectingCursor) Close() error   { f.closed = true; return nil }
func (f *resurrectingCursor) IsClosed() bool { return f.closed }
