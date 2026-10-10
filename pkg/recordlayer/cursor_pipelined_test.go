package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// pipelineProbe records the order in which mappings are issued and resolved.
type pipelineProbe struct {
	events []string
	fail   map[int]error
	ready  map[int]bool
}

func (p *pipelineProbe) issue(v int) PendingRead[int] {
	p.events = append(p.events, fmt.Sprintf("issue %d", v))
	return NewPendingRead(func() (int, error) {
		p.events = append(p.events, fmt.Sprintf("resolve %d", v))
		if err := p.fail[v]; err != nil {
			return 0, err
		}
		return v * 10, nil
	}, func() bool { return p.ready[v] })
}

func (p *pipelineProbe) take() []string {
	events := p.events
	p.events = nil
	return events
}

func expectEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestMapPipelinedIssuesAWindowBeforeWaiting(t *testing.T) {
	t.Parallel()
	probe := &pipelineProbe{}
	cursor := MapPipelined(FromList([]int{0, 1, 2, 3, 4}), probe.issue, 3)
	defer cursor.Close()
	ctx := context.Background()
	r, err := cursor.OnNext(ctx)
	if err != nil || r.GetValue() != 0 {
		t.Fatalf("first: %v, %v", r, err)
	}
	expectEvents(t, probe.take(), "issue 0", "issue 1", "issue 2", "resolve 0")
	for want := 1; want < 5; want++ {
		r, err := cursor.OnNext(ctx)
		if err != nil || !r.HasNext() || r.GetValue() != want*10 {
			t.Fatalf("row %d: %v, %v", want, r, err)
		}
		cont, err := r.GetContinuation().ToBytes()
		if err != nil || !bytes.Equal(cont, ListCursorContinuation(want+1)) {
			t.Fatalf("row %d continuation = %x, %v", want, cont, err)
		}
	}
	end, err := cursor.OnNext(ctx)
	if err != nil || end.HasNext() || end.GetNoNextReason() != SourceExhausted {
		t.Fatalf("end: %v, %v", end, err)
	}
}

func TestMapPipelinedErrorIsOrderedAndSticky(t *testing.T) {
	t.Parallel()
	boom := errors.New("second record failed")
	probe := &pipelineProbe{fail: map[int]error{1: boom}}
	cursor := MapPipelined(FromList([]int{0, 1, 2}), probe.issue, 3)
	defer cursor.Close()
	r, err := cursor.OnNext(context.Background())
	if err != nil || !r.HasNext() || r.GetValue() != 0 {
		t.Fatalf("earlier result lost: %v, %v", r, err)
	}
	for range 2 {
		if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
	}
	expectEvents(t, probe.take(), "issue 0", "issue 1", "issue 2", "resolve 0", "resolve 1")
}

func TestMapPipelinedSourceErrorFailsBeforeQueuedRows(t *testing.T) {
	t.Parallel()
	boom := errors.New("source failed during prefill")
	source := MapErrCursor(FromList([]int{0, 1}), func(v int) (int, error) {
		if v == 1 {
			return 0, boom
		}
		return v, nil
	})
	probe := &pipelineProbe{}
	cursor := MapPipelined(source, probe.issue, 3)
	defer cursor.Close()
	for range 2 {
		if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("prefill error = %v", err)
		}
	}
	expectEvents(t, probe.take(), "issue 0")
}

func TestMapPipelinedTimeLimitKeepsOnlyTheCompletedPrefix(t *testing.T) {
	t.Parallel()
	probe := &pipelineProbe{ready: map[int]bool{1: true, 3: true}}
	source := newOOBStopCursorUnit([]int{0, 1, 2, 3}, TimeLimitReached, NewBytesContinuation(ListCursorContinuation(4)))
	cursor := MapPipelined[int, int](source, probe.issue, 4)
	defer cursor.Close()
	first, err := cursor.OnNext(context.Background())
	if err != nil || first.GetValue() != 0 {
		t.Fatalf("first: %v, %v", first, err)
	}
	second, err := cursor.OnNext(context.Background())
	if err != nil || !second.HasNext() || second.GetValue() != 10 {
		t.Fatalf("completed entry dropped: %v, %v", second, err)
	}
	for range 2 {
		stop, err := cursor.OnNext(context.Background())
		if err != nil || stop.HasNext() || stop.GetNoNextReason() != TimeLimitReached {
			t.Fatalf("stop: %v, %v", stop, err)
		}
		cont, err := stop.GetContinuation().ToBytes()
		if err != nil || !bytes.Equal(cont, ListCursorContinuation(2)) {
			t.Fatalf("stop continuation skipped the unfinished row: %x, %v", cont, err)
		}
	}
	expectEvents(t, probe.take(), "issue 0", "issue 1", "issue 2", "issue 3", "resolve 0", "resolve 1")
}

func TestMapPipelinedTimeLimitFailsOnACompletedFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("completed load failed")
	probe := &pipelineProbe{ready: map[int]bool{1: true}, fail: map[int]error{1: boom}}
	source := newOOBStopCursorUnit([]int{0, 1, 2}, TimeLimitReached, NewBytesContinuation(ListCursorContinuation(3)))
	cursor := MapPipelined[int, int](source, probe.issue, 3)
	defer cursor.Close()
	if r, err := cursor.OnNext(context.Background()); err != nil || r.GetValue() != 0 {
		t.Fatalf("first: %v, %v", r, err)
	}
	if _, err := cursor.OnNext(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("time-limited stop hid a completed failure: %v", err)
	}
}

func TestMapPipelinedClosedCursorDoesNotPullSource(t *testing.T) {
	t.Parallel()
	inner := newCloseTrackerUnit(FromList([]int{1, 2}))
	probe := &pipelineProbe{}
	cursor := MapPipelined[int, int](inner, probe.issue, 2)
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if !cursor.IsClosed() || !inner.wasClosed() {
		t.Fatal("Close not reported")
	}
	if _, err := cursor.OnNext(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("OnNext after Close: %v", err)
	}
	if len(probe.take()) != 0 {
		t.Fatal("closed cursor issued work")
	}
}

func TestMapPipelinedStopBeforeFirstRowOrOtherLimitsDrain(t *testing.T) {
	t.Parallel()
	for _, reason := range []NoNextReason{TimeLimitReached, ByteLimitReached, ScanLimitReached, ReturnLimitReached} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			t.Parallel()
			stopCont := NewBytesContinuation(ListCursorContinuation(2))
			cursor := MapPipelined[int, int](newOOBStopCursorUnit([]int{0, 1}, reason, stopCont), (&pipelineProbe{}).issue, 3)
			defer cursor.Close()
			for want := range 2 {
				r, err := cursor.OnNext(context.Background())
				if err != nil || !r.HasNext() || r.GetValue() != want*10 {
					t.Fatalf("row %d: %v, %v", want, r, err)
				}
			}
			r, err := cursor.OnNext(context.Background())
			if err != nil || r.HasNext() || r.GetNoNextReason() != reason {
				t.Fatalf("stop: %v, %v", r, err)
			}
			cont, err := r.GetContinuation().ToBytes()
			if err != nil || !bytes.Equal(cont, ListCursorContinuation(2)) {
				t.Fatalf("stop continuation = %x, %v", cont, err)
			}
		})
	}
}

func TestMapPipelinedInvalidWidths(t *testing.T) {
	t.Parallel()
	for _, width := range []int{0, -1} {
		source := FromList([]int{1})
		cursor := MapPipelined(source, (&pipelineProbe{}).issue, width)
		if _, err := cursor.OnNext(context.Background()); err == nil {
			t.Fatalf("accepted width %d", width)
		}
		if !source.IsClosed() {
			t.Fatal("invalid mapper retained its source")
		}
	}
}

func TestMapPipelinedCanceledContextDoesNotPullSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inner := FromList([]int{1})
	probe := &pipelineProbe{}
	cursor := MapPipelined(inner, probe.issue, 2)
	defer cursor.Close()
	if _, err := cursor.OnNext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled OnNext: %v", err)
	}
	r, err := inner.OnNext(context.Background())
	if err != nil || !r.HasNext() || r.GetValue() != 1 || len(probe.take()) != 0 {
		t.Fatalf("canceled pipeline consumed source: %v, %v", r, err)
	}
}
