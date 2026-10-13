package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func TestMapPipelinedReturnedResultsAndCachedTerminal(t *testing.T) {
	t.Parallel()
	for _, reason := range []NoNextReason{SourceExhausted, TimeLimitReached} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			t.Parallel()
			var stopCont RecordCursorContinuation = &EndContinuation{}
			if reason == TimeLimitReached {
				stopCont = NewBytesContinuation(ListCursorContinuation(2))
			}
			probe := &pipelineProbe{ready: map[int]bool{1: true}}
			cursor := MapPipelined[int, int](newOOBStopCursorUnit([]int{0, 1}, reason, stopCont), probe.issue, 2)
			defer cursor.Close()
			ctx := context.Background()
			var rows [2]RecordCursorResult[int]
			for i := range rows {
				r, err := cursor.OnNext(ctx)
				if err != nil || !r.HasNext() || r.GetValue() != i*10 {
					t.Fatalf("row %d: %v, %v", i, r, err)
				}
				rows[i] = r
			}
			terminal, err := cursor.OnNext(ctx)
			if err != nil || terminal.HasNext() || terminal.GetNoNextReason() != reason || terminal.GetContinuation() != stopCont {
				t.Fatalf("terminal: %v, %v", terminal, err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			cached, err := cursor.OnNext(canceled)
			if err != nil || cached != terminal {
				t.Fatalf("cached terminal after cancellation: %v, %v; want %v", cached, err, terminal)
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			for _, nextCtx := range []context.Context{ctx, canceled} {
				cached, err = cursor.OnNext(nextCtx)
				if err != nil || cached != terminal {
					t.Fatalf("cached terminal after close: %v, %v; want %v", cached, err, terminal)
				}
			}
			for i, r := range rows {
				if !r.HasNext() || r.GetValue() != i*10 {
					t.Fatalf("previously returned row %d changed: %v", i, r)
				}
				cont, err := r.GetContinuation().ToBytes()
				if err != nil || !bytes.Equal(cont, ListCursorContinuation(i+1)) {
					t.Fatalf("previously returned row %d continuation = %x, %v", i, cont, err)
				}
			}
			expectEvents(t, probe.take(), "issue 0", "issue 1", "resolve 0", "resolve 1")
		})
	}
}

// pipelineScanSource returns preallocated results so allocation samples include
// the public mapper's construction, refill and pop path, not fixture work.
type pipelineScanSource struct {
	rows   []RecordCursorResult[int]
	stop   RecordCursorResult[int]
	next   int
	pulls  int
	closed bool
	failAt int
	err    error
}

func (s *pipelineScanSource) OnNext(context.Context) (RecordCursorResult[int], error) {
	s.pulls++
	if s.err != nil && s.next == s.failAt {
		return RecordCursorResult[int]{}, s.err
	}
	if s.next == len(s.rows) {
		return s.stop, nil
	}
	r := s.rows[s.next]
	s.next++
	return r, nil
}

func (s *pipelineScanSource) Close() error   { s.closed = true; return nil }
func (s *pipelineScanSource) IsClosed() bool { return s.closed }

type pipelineScanPending struct {
	value int
	gets  int
}

func (p *pipelineScanPending) Get() (int, error) { p.gets++; return p.value, nil }
func (*pipelineScanPending) IsReady() bool       { return true }

var pipelineAllocationCursor RecordCursor[int]

func TestMapPipelinedFullTraversalAllocations(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^BenchmarkMapPipelinedFullTraversalAllocations$", "-test.benchtime=1x").CombinedOutput()
	t.Logf("%s", output)
	if err != nil || !bytes.Contains(output, []byte("pipeline full traversal: samples=100 rows=1000")) || !bytes.Contains(output, []byte("pipeline full traversal: samples=100 rows=4000")) {
		t.Fatalf("isolated full traversal allocation benchmark: %v\n%s", err, output)
	}
}

func BenchmarkMapPipelinedFullTraversalAllocations(b *testing.B) {
	const samples = 100
	ctx := context.Background()
	var counts [2]float64
	for i, rows := range []int{1000, 4000} {
		source := &pipelineScanSource{
			rows: make([]RecordCursorResult[int], rows),
			stop: NewResultNoNext[int](SourceExhausted, &EndContinuation{}),
		}
		pending := make([]pipelineScanPending, rows)
		for row := range rows {
			source.rows[row] = NewResultWithValue(row, NewBytesContinuation(ListCursorContinuation(row+1)))
			pending[row].value = row * 10
		}
		issues, returned, continuations, terminals := 0, 0, 0, 0
		issue := func(row int) PendingRead[int] { issues++; return &pending[row] }
		counts[i] = testing.AllocsPerRun(samples, func() {
			source.next, source.closed = 0, false
			pipelineAllocationCursor = MapPipelined[int, int](source, issue, 10)
			for row := range rows {
				var err error
				pipelineAllocationResult, err = pipelineAllocationCursor.OnNext(ctx)
				if err != nil || !pipelineAllocationResult.HasNext() || pipelineAllocationResult.GetValue() != row*10 {
					b.Fatalf("row %d: %v, %v", row, pipelineAllocationResult, err)
				}
				returned++
				if pipelineAllocationResult.GetContinuation() != source.rows[row].GetContinuation() {
					b.Fatalf("row %d continuation changed", row)
				}
				continuations++
			}
			var err error
			pipelineAllocationResult, err = pipelineAllocationCursor.OnNext(ctx)
			if err != nil || pipelineAllocationResult.HasNext() || pipelineAllocationResult.GetNoNextReason() != SourceExhausted || pipelineAllocationResult.GetContinuation() != source.stop.GetContinuation() {
				b.Fatalf("terminal: %v, %v", pipelineAllocationResult, err)
			}
			terminals++
			if err := pipelineAllocationCursor.Close(); err != nil || !source.closed {
				b.Fatalf("close: closed=%v err=%v", source.closed, err)
			}
		})
		gets := 0
		for row := range pending {
			if pending[row].gets != samples+1 {
				b.Fatalf("row %d Get calls=%d, want %d", row, pending[row].gets, samples+1)
			}
			gets += pending[row].gets
		}
		want := (samples + 1) * rows
		if source.pulls != (samples+1)*(rows+1) || issues != want || gets != want || returned != want || continuations != want || terminals != samples+1 {
			b.Fatalf("population: pulls=%d issues=%d gets=%d rows=%d continuations=%d terminals=%d", source.pulls, issues, gets, returned, continuations, terminals)
		}
		b.Logf("pipeline full traversal: samples=%d rows=%d pulls=%d issues=%d gets=%d returned=%d continuations=%d terminals=%d allocations=%.0f", samples, rows, source.pulls, issues, gets, returned, continuations, terminals, counts[i])
	}
	// One cursor, one terminal result, and bounded queue backing. Ready heads
	// need only one slot; pending heads can grow through sizes 1, 2, 4, 8, 10.
	if counts[0] != counts[1] || counts[0] > 7 {
		b.Fatalf("full traversal allocations: 1000 rows=%g, 4000 rows=%g; want equal and at most 7", counts[0], counts[1])
	}
}

type pipelineAllocationPending struct {
	gets int
}

func (p *pipelineAllocationPending) Get() (int, error) {
	p.gets++
	return 0, nil
}

func (*pipelineAllocationPending) IsReady() bool { return true }

// Publish the by-value result just as an escaping caller would. Only the
// isolated benchmark uses this sink; allocation counts are process-wide.
var pipelineAllocationResult RecordCursorResult[int]

func TestMapPipelinedOnNextDoesNotAllocate(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^BenchmarkMapPipelinedOnNextAllocations$", "-test.benchtime=1x").CombinedOutput()
	t.Logf("%s", output)
	if err != nil || !bytes.Contains(output, []byte("mapPipelined OnNext allocations: samples=1000 pendingGets=1001")) {
		t.Fatalf("isolated allocation benchmark: %v\n%s", err, output)
	}
}

func BenchmarkMapPipelinedOnNextAllocations(b *testing.B) {
	pending := &pipelineAllocationPending{}
	continuation := NewBytesContinuation([]byte{1})
	entry := mapPipelineEntry[int]{pending: pending, continuation: continuation}
	var pipeline [1]mapPipelineEntry[int]
	stop := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
	cursor := &mapPipelinedCursor[int, int]{size: 1, stop: &stop}
	ctx := context.Background()
	const samples = 1000
	allocations := testing.AllocsPerRun(samples, func() {
		// Restore the cleared queue slot without changing the previous result.
		// The existing stop prevents source pulls, mappings, or queue growth.
		pipeline[0] = entry
		cursor.pipeline, cursor.head, cursor.count = pipeline[:], 0, 1
		var err error
		pipelineAllocationResult, err = cursor.OnNext(ctx)
		if err != nil || !pipelineAllocationResult.HasNext() || pipelineAllocationResult.GetValue() != 0 || pipelineAllocationResult.GetContinuation() != continuation {
			b.Fatalf("OnNext = %v, %v", pipelineAllocationResult, err)
		}
	})
	if pending.gets != samples+1 {
		b.Fatalf("pending Get calls = %d, want %d including warmup", pending.gets, samples+1)
	}
	cont, err := pipelineAllocationResult.GetContinuation().ToBytes()
	if err != nil || !bytes.Equal(cont, []byte{1}) || pipelineAllocationResult.GetContinuation().IsEnd() {
		b.Fatalf("returned continuation = %x, %v", cont, err)
	}
	b.Logf("mapPipelined OnNext allocations: samples=%d pendingGets=%d allocations=%.0f", samples, pending.gets, allocations)
	if allocations != 0 {
		b.Fatalf("OnNext allocations = %g, want 0", allocations)
	}
}

type pipelineRingPending struct {
	value int
	gets  int
	ready bool
	err   error
}

func (p *pipelineRingPending) Get() (int, error) { p.gets++; return p.value, p.err }
func (p *pipelineRingPending) IsReady() bool     { return p.ready }

type pipelineRingFixture struct {
	source  pipelineScanSource
	pending []pipelineRingPending
	issued  int
}

func newPipelineRingFixture(rows int, reason NoNextReason) *pipelineRingFixture {
	f := &pipelineRingFixture{pending: make([]pipelineRingPending, rows)}
	f.source.rows = make([]RecordCursorResult[int], rows)
	var continuation RecordCursorContinuation = &EndContinuation{}
	if reason != SourceExhausted {
		continuation = NewBytesContinuation(ListCursorContinuation(rows))
	}
	f.source.stop = NewResultNoNext[int](reason, continuation)
	for row := range rows {
		f.source.rows[row] = NewResultWithValue(row, NewBytesContinuation(ListCursorContinuation(row+1)))
		// Keep the head pending during issue so these queue tests exercise a
		// full window and wrapped backing, rather than the ready-head shortcut.
		f.pending[row] = pipelineRingPending{value: row * 10}
	}
	return f
}

func (f *pipelineRingFixture) issue(row int) PendingRead[int] {
	f.issued++
	return &f.pending[row]
}

func (f *pipelineRingFixture) cursor(width int) *mapPipelinedCursor[int, int] {
	return MapPipelined[int, int](&f.source, f.issue, width).(*mapPipelinedCursor[int, int])
}

func assertPipelineUnusedSlots(t *testing.T, c *mapPipelinedCursor[int, int]) {
	t.Helper()
	backing := c.pipeline[:cap(c.pipeline)]
	if c.count < 0 || c.count > len(backing) || c.count > c.size {
		t.Fatalf("invalid queue: count=%d backing=%d width=%d", c.count, len(backing), c.size)
	}
	for i, entry := range backing {
		offset := i - c.head
		if offset < 0 {
			offset += len(backing)
		}
		if offset >= c.count {
			if entry.pending != nil || entry.continuation != nil {
				t.Fatalf("unused physical slot %d retains references: head=%d count=%d entry=%v", i, c.head, c.count, entry)
			}
		} else if entry.pending == nil || entry.continuation == nil {
			t.Fatalf("live physical slot %d lost references: head=%d count=%d", i, c.head, c.count)
		}
	}
}

func assertPipelineAllCleared(t *testing.T, backing []mapPipelineEntry[int]) {
	t.Helper()
	if len(backing) == 0 {
		t.Fatal("empty clearing probe")
	}
	for i, entry := range backing {
		if entry.pending != nil || entry.continuation != nil {
			t.Fatalf("physical slot %d retains references: %v", i, entry)
		}
	}
}

func TestMapPipelinedRepeatedWraps(t *testing.T) {
	t.Parallel()
	for _, width := range []int{1, 2, 3, 7, 10, 257} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			t.Parallel()
			rows := 3*width + 5
			f := newPipelineRingFixture(rows, SourceExhausted)
			c := f.cursor(width)
			defer c.Close()
			var saved []RecordCursorResult[int]
			wraps := 0
			for row := range rows {
				oldHead := c.head
				r, err := c.OnNext(context.Background())
				if c.head < oldHead {
					wraps++
				}
				if err != nil || !r.HasNext() || r.GetValue() != row*10 || r.GetContinuation() != f.source.rows[row].GetContinuation() {
					t.Fatalf("row %d: %v, %v", row, r, err)
				}
				saved = append(saved, r)
				if f.issued != min(row+width, rows) || f.source.pulls != min(row+width, rows+1) || c.count != min(width-1, rows-row-1) {
					t.Fatalf("row %d: issued=%d pulls=%d queued=%d", row, f.issued, f.source.pulls, c.count)
				}
				assertPipelineUnusedSlots(t, c)
			}
			r, err := c.OnNext(context.Background())
			if err != nil || r.HasNext() || r.GetNoNextReason() != SourceExhausted || f.source.pulls != rows+1 {
				t.Fatalf("terminal: %v, %v pulls=%d", r, err, f.source.pulls)
			}
			for row, previous := range saved {
				if previous.GetValue() != row*10 || previous.GetContinuation() != f.source.rows[row].GetContinuation() || f.pending[row].gets != 1 {
					t.Fatalf("row %d changed or Get repeated: %v gets=%d", row, previous, f.pending[row].gets)
				}
			}
			assertPipelineAllCleared(t, c.pipeline[:cap(c.pipeline)])
			if width > 1 && wraps < 3 {
				t.Fatalf("queue did not exercise repeated wrapping: width=%d wraps=%d", width, wraps)
			}
			t.Logf("width=%d rows=%d wraps=%d issued=%d pulls=%d", width, rows, wraps, f.issued, f.source.pulls)
		})
	}
}

func TestMapPipelinedBackingIsLazyAndBounded(t *testing.T) {
	t.Parallel()
	for _, width := range []int{1, 2, 3, 7, 10, 257, int(^uint(0) >> 1)} {
		for _, rows := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("width=%d/rows=%d", width, rows), func(t *testing.T) {
				t.Parallel()
				f := newPipelineRingFixture(rows, SourceExhausted)
				c := f.cursor(width)
				defer c.Close()
				if c.pipeline != nil {
					t.Fatal("constructor eagerly allocated backing")
				}
				for row := 0; row <= rows; row++ {
					r, err := c.OnNext(context.Background())
					if err != nil || r.HasNext() != (row < rows) {
						t.Fatalf("row %d: %v, %v", row, r, err)
					}
					if cap(c.pipeline) > width || cap(c.pipeline) > 2*rows {
						t.Fatalf("excess backing: width=%d rows=%d capacity=%d", width, rows, cap(c.pipeline))
					}
					assertPipelineUnusedSlots(t, c)
				}
				if f.issued != rows || f.source.pulls != rows+1 {
					t.Fatalf("population: issued=%d pulls=%d", f.issued, f.source.pulls)
				}
			})
		}
	}
}

func TestMapPipelinedWrappedTimeLimitPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		keep int
		fail bool
	}{
		{"none", 0, false}, {"cross-end", 2, false}, {"all-ready", 4, false}, {"ready-after-unready", 1, false}, {"completed-failure", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPipelineRingFixture(8, TimeLimitReached)
			for row := 4; row < len(f.pending); row++ {
				f.pending[row].ready = true
			}
			if tc.keep < 4 {
				f.pending[4+tc.keep].ready = false
			}
			boom := errors.New("completed wrapped load failed")
			if tc.fail {
				f.pending[5].err = boom
			}
			c := f.cursor(5)
			defer c.Close()
			for row := range 4 {
				r, err := c.OnNext(context.Background())
				if err != nil || !r.HasNext() || r.GetValue() != row*10 {
					t.Fatalf("row %d: %v, %v", row, r, err)
				}
			}
			backing := c.pipeline[:cap(c.pipeline)]
			if c.head != 4 || c.count != 4 || len(backing) != 5 {
				t.Fatalf("timeout probe is not a populated wrapped window: head=%d count=%d physical=%d", c.head, c.count, len(backing))
			}
			t.Logf("before timeout: head=%d count=%d physical=%d", c.head, c.count, len(backing))
			if tc.fail {
				for range 2 {
					if _, err := c.OnNext(context.Background()); !errors.Is(err, boom) {
						t.Fatalf("completed failure: %v", err)
					}
				}
				if f.source.pulls != 9 || f.issued != 8 || f.pending[4].gets != 1 || f.pending[5].gets != 1 || f.pending[6].gets != 0 || f.pending[7].gets != 0 {
					t.Fatal("completed failure reordered or repeated work")
				}
				assertPipelineAllCleared(t, backing)
				if c.pipeline != nil || c.count != 0 {
					t.Fatal("failed queue retained backing")
				}
				return
			}
			for offset := range tc.keep {
				r, err := c.OnNext(context.Background())
				if err != nil || !r.HasNext() || r.GetValue() != (4+offset)*10 || r.GetContinuation() != f.source.rows[4+offset].GetContinuation() {
					t.Fatalf("kept %d: %v, %v", offset, r, err)
				}
				assertPipelineUnusedSlots(t, c)
			}
			stop, err := c.OnNext(context.Background())
			if err != nil || stop.HasNext() || stop.GetNoNextReason() != TimeLimitReached {
				t.Fatalf("stop: %v, %v", stop, err)
			}
			continuation, err := stop.GetContinuation().ToBytes()
			if err != nil || !bytes.Equal(continuation, ListCursorContinuation(4+tc.keep)) {
				t.Fatalf("stop continuation=%x err=%v", continuation, err)
			}
			if tc.keep == 4 && stop.GetContinuation() != f.source.stop.GetContinuation() {
				t.Fatal("all-ready changed source stop continuation")
			}
			for row := range f.pending {
				want := 0
				if row < 4 {
					want = 1
				} else if row < 4+tc.keep {
					want = 2
				}
				if f.pending[row].gets != want {
					t.Fatalf("row %d Gets=%d want=%d", row, f.pending[row].gets, want)
				}
			}
			assertPipelineAllCleared(t, backing)
			items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
			resumed := MapPipelined(FromListWithContinuation(items, continuation), func(row int) PendingRead[int] { return &pipelineScanPending{value: row} }, 3)
			defer resumed.Close()
			for row := 4 + tc.keep; row < len(items); row++ {
				r, err := resumed.OnNext(context.Background())
				if err != nil || !r.HasNext() || r.GetValue() != row {
					t.Fatalf("resume row %d: %v, %v", row, r, err)
				}
			}
			if r, err := resumed.OnNext(context.Background()); err != nil || r.HasNext() {
				t.Fatalf("resume terminal: %v, %v", r, err)
			}
			if f.source.pulls != 9 || f.issued != 8 {
				t.Fatalf("population: pulls=%d issued=%d", f.source.pulls, f.issued)
			}
		})
	}
}

func TestMapPipelinedWrappedFailureAndCloseClearAllSlots(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"source", "mapping", "close"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newPipelineRingFixture(20, SourceExhausted)
			c := f.cursor(5)
			defer c.Close()
			for row := range 4 {
				if r, err := c.OnNext(context.Background()); err != nil || !r.HasNext() || r.GetValue() != row*10 {
					t.Fatalf("row %d: %v, %v", row, r, err)
				}
			}
			backing := c.pipeline[:cap(c.pipeline)]
			assertPipelineUnusedSlots(t, c)
			boom := errors.New("wrapped failure")
			wantPulls, wantIssued := 9, 9
			switch mode {
			case "source":
				f.source.failAt, f.source.err = 8, boom
				wantIssued = 8
			case "mapping":
				f.pending[4].err = boom
			case "close":
				if err := c.Close(); err != nil || !f.source.closed {
					t.Fatalf("close: %v", err)
				}
				boom = context.Canceled
				wantPulls, wantIssued = 8, 8
			}
			for range 2 {
				if _, err := c.OnNext(context.Background()); !errors.Is(err, boom) {
					t.Fatalf("sticky %s: %v", mode, err)
				}
			}
			assertPipelineAllCleared(t, backing)
			if c.pipeline != nil || c.count != 0 || c.head != 0 {
				t.Fatal("failed or closed queue retained backing/state")
			}
			if f.source.pulls != wantPulls || f.issued != wantIssued {
				t.Fatalf("population: pulls=%d issued=%d", f.source.pulls, f.issued)
			}
			for row := 4; row < len(f.pending); row++ {
				want := 0
				if mode == "mapping" && row == 4 {
					want = 1
				}
				if f.pending[row].gets != want {
					t.Fatalf("row %d Gets=%d want=%d", row, f.pending[row].gets, want)
				}
			}
		})
	}
}

func TestMapPipelinedQueuedContextCancellationIsNotSticky(t *testing.T) {
	t.Parallel()
	f := newPipelineRingFixture(12, SourceExhausted)
	c := f.cursor(3)
	defer c.Close()
	for row := range 4 {
		if r, err := c.OnNext(context.Background()); err != nil || !r.HasNext() || r.GetValue() != row*10 {
			t.Fatalf("row %d: %v, %v", row, r, err)
		}
	}
	before := append([]mapPipelineEntry[int](nil), c.pipeline...)
	head, count := c.head, c.count
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.OnNext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: %v", err)
	}
	if c.head != head || c.count != count || c.err != nil || f.source.pulls != 6 || f.issued != 6 {
		t.Fatal("canceled call changed queued work")
	}
	for i, entry := range before {
		if c.pipeline[i] != entry {
			t.Fatalf("canceled call changed slot %d", i)
		}
	}
	for row := 4; row < 12; row++ {
		r, err := c.OnNext(context.Background())
		if err != nil || !r.HasNext() || r.GetValue() != row*10 || r.GetContinuation() != f.source.rows[row].GetContinuation() {
			t.Fatalf("retry row %d: %v, %v", row, r, err)
		}
		assertPipelineUnusedSlots(t, c)
	}
	if r, err := c.OnNext(context.Background()); err != nil || r.HasNext() {
		t.Fatalf("terminal: %v, %v", r, err)
	}
	if f.source.pulls != 13 || f.issued != 12 {
		t.Fatalf("population: pulls=%d issued=%d", f.source.pulls, f.issued)
	}
	for row := range f.pending {
		if f.pending[row].gets != 1 {
			t.Fatalf("row %d Gets=%d", row, f.pending[row].gets)
		}
	}
}

func TestMapPipelineQueueMatchesFIFOWhileGrowingWrapped(t *testing.T) {
	t.Parallel()
	f := newPipelineRingFixture(100, SourceExhausted)
	c := f.cursor(17)
	defer c.Close()
	var model []mapPipelineEntry[int]
	next := 0
	push := func() {
		entry := mapPipelineEntry[int]{pending: &f.pending[next], continuation: f.source.rows[next].GetContinuation()}
		next++
		c.push(entry)
		model = append(model, entry)
		assertPipelineUnusedSlots(t, c)
	}
	pop := func() {
		if got := c.pop(); got != model[0] {
			t.Fatalf("pop=%v want=%v", got, model[0])
		}
		model = model[1:]
		assertPipelineUnusedSlots(t, c)
	}
	// Fill each capacity, advance the head, refill, then grow while wrapped.
	for _, capacity := range []int{2, 4, 8, 16} {
		for len(model) < capacity {
			push()
		}
		pop()
		push()
		t.Logf("before grow: physical=%d head=%d count=%d", len(c.pipeline), c.head, c.count)
		push()
		if c.count != len(model) || cap(c.pipeline) > c.size {
			t.Fatalf("growth: queued=%d model=%d capacity=%d", c.count, len(model), cap(c.pipeline))
		}
	}
	for len(model) > 0 {
		pop()
	}
	for range 30 {
		push()
		pop()
	}
	assertPipelineAllCleared(t, c.pipeline[:cap(c.pipeline)])
	if next != 51 {
		t.Fatalf("push population=%d want=51", next)
	}
}
