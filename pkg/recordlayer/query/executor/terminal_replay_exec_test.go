package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// requireTerminalReplay drains the cursor to its first no-next, then re-calls
// OnNext and asserts the terminal result is REPLAYED verbatim (same reason,
// same continuation bytes) — Java's cached-terminal contract
// (`if (nextResult != null && !nextResult.hasNext()) return nextResult`).
// The TestFlatMapTerminalReplayOnRecall assertion, generalized over the
// element type. Every fixture below is NON-idempotent at the terminal (a
// second pull yields a different reason or MORE rows), so a reverted guard
// goes red.
func requireTerminalReplay[T any](t *testing.T, c recordlayer.RecordCursor[T]) {
	t.Helper()
	ctx := context.Background()
	var first recordlayer.RecordCursorResult[T]
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
		t.Fatal("a re-call after a terminal result must replay it, not produce a value")
	}
	fb, _ := first.GetContinuation().ToBytes()
	ab, _ := again.GetContinuation().ToBytes()
	if again.GetNoNextReason() != first.GetNoNextReason() || string(fb) != string(ab) {
		t.Fatalf("re-call (%v, %v) must equal the cached terminal (%v, %v)",
			again.GetNoNextReason(), ab, first.GetNoNextReason(), fb)
	}
}

// pauseThenExhaustCursor pauses out-of-band ONCE, then reports exhaustion —
// non-idempotent at the terminal, so a wrapper that re-pulls it after the
// pause surfaces the changed reason/continuation.
type pauseThenExhaustCursor[T any] struct {
	pulls  int
	closed bool
}

func (f *pauseThenExhaustCursor[T]) OnNext(context.Context) (recordlayer.RecordCursorResult[T], error) {
	f.pulls++
	if f.pulls == 1 {
		return recordlayer.NewResultNoNext[T](
			recordlayer.TimeLimitReached, recordlayer.NewBytesContinuation([]byte{0xCD})), nil
	}
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}
func (f *pauseThenExhaustCursor[T]) Close() error   { f.closed = true; return nil }
func (f *pauseThenExhaustCursor[T]) IsClosed() bool { return f.closed }

// TestTerminalReplay_ExecutorCursors pins Java's cached-terminal contract on
// the executor's wrapper/merge cursors: after a no-next, a re-call replays the
// identical result and never re-pulls the inner.
func TestTerminalReplay_ExecutorCursors(t *testing.T) {
	t.Parallel()

	t.Run("aggregate_mid_group_pause", func(t *testing.T) {
		t.Parallel()
		// CRITICAL pin: a re-call after a mid-group out-of-band stop must NOT
		// re-pull the inner — the flaky inner here yields MORE rows on a
		// re-pull, which a missing guard would consume into an accumulator
		// whose partial state was already serialized into the reported
		// continuation (rows silently lost on resume), then surface the
		// accumulated group as a bogus VALUE once the inner exhausts.
		rows := []QueryResult{qr("v", int64(10)), qr("v", int64(20)), qr("v", int64(30))}
		inner := &flakyOuterCursor{first: rows[:2], rest: rows[2:]}
		aggs := []expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
		}
		c := newAggregateCursor(inner, nil, aggs, nil, nil)
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("custom_sort_fill_pause", func(t *testing.T) {
		t.Parallel()
		// The fill-phase out-of-band stop: a re-call missing the guard
		// re-enters the fill loop, pulls the flaky inner's post-pause rows
		// into a buffer already serialized into the reported continuation,
		// then emits a bogus sorted VALUE.
		rows := []QueryResult{qr("v", int64(2)), qr("v", int64(1))}
		inner := &flakyOuterCursor{first: rows[:1], rest: rows[1:]}
		c := newCustomSortCursor(inner, func([]QueryResult) error { return nil }, nil)
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("multi_intersection_merge", func(t *testing.T) {
		t.Parallel()
		c := &multiIntersectionMergeCursor{inner: &pauseThenExhaustCursor[[]QueryResult]{}}
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("filter_result", func(t *testing.T) {
		t.Parallel()
		rows := []QueryResult{qr("v", int64(1)), qr("v", int64(2))}
		c := &filterResultCursor{
			inner: &flakyOuterCursor{first: rows[:1], rest: rows[1:]},
			pred:  func(QueryResult) (bool, error) { return true, nil },
		}
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("err_check", func(t *testing.T) {
		t.Parallel()
		rows := []QueryResult{qr("v", int64(1)), qr("v", int64(2))}
		var evalErr error
		c := &errCheckCursor{
			inner: &flakyOuterCursor{first: rows[:1], rest: rows[1:]},
			err:   &evalErr,
		}
		requireTerminalReplay(t, c)
	})

	t.Run("index_fetch", func(t *testing.T) {
		t.Parallel()
		c := &indexFetchCursor{inner: &pauseThenExhaustCursor[*recordlayer.IndexEntry]{}}
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("covering_index", func(t *testing.T) {
		t.Parallel()
		c := &coveringIndexCursor{inner: &pauseThenExhaustCursor[*recordlayer.IndexEntry]{}}
		defer c.Close()
		requireTerminalReplay(t, c)
	})

	t.Run("limit_envelope_out_of_band", func(t *testing.T) {
		t.Parallel()
		// The out-of-band branch was deliberately un-cached before (only the
		// exhaustion terminal was sticky) — a re-call re-pulled the inner,
		// which here yields MORE rows.
		rows := []QueryResult{qr("v", int64(1)), qr("v", int64(2))}
		c := newLimitEnvelopeCursor(&flakyOuterCursor{first: rows[:1], rest: rows[1:]}, 0, 5)
		defer c.Close()
		requireTerminalReplay(t, c)
	})
}
