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
}
