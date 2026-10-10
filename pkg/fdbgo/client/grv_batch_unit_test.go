package client

import (
	"sync"
	"testing"
	"time"
)

func batchRequest() grvRequest {
	return grvRequest{reply: make(chan grvResult, 1)}
}

func ignoreBatchTimer(uint64) {}

func TestGRVBatchIdleDispatch(t *testing.T) {
	t.Parallel()
	for _, window := range []time.Duration{0, 50 * time.Microsecond, 5 * time.Millisecond} {
		t.Run(window.String(), func(t *testing.T) {
			t.Parallel()
			b := &grvBatcher{priority: grvPriorityDefault, batchTime: window}
			t.Cleanup(func() {
				b.mu.Lock()
				defer b.mu.Unlock()
				if b.timer != nil {
					b.timer.Stop()
				}
			})
			req := batchRequest()
			batch := b.admit(req, ignoreBatchTimer)
			if len(batch) != 1 || batch[0].reply != req.reply || b.timer != nil || len(b.pending) != 0 || b.inFlight != 1 {
				t.Fatalf("idle GRV waited for a batching timer: dispatched=%d timer=%v pending=%d", len(batch), b.timer != nil, len(b.pending))
			}
		})
	}
}

func TestGRVBatchCompletionPreservesQueuedTimer(t *testing.T) {
	t.Parallel()
	b := &grvBatcher{priority: grvPriorityDefault, batchTime: time.Hour}
	first, second, third := batchRequest(), batchRequest(), batchRequest()
	batch := b.admit(first, ignoreBatchTimer)
	if len(batch) != 1 || batch[0].reply != first.reply {
		t.Fatal("first idle request did not detach")
	}
	if b.admit(second, ignoreBatchTimer) != nil || len(b.pending) != 1 {
		t.Fatal("later request did not get its own queue")
	}
	timer, seq := b.timer, b.timerSeq
	b.finishBatch()
	if b.inFlight != 0 || len(b.pending) != 1 || b.timer != timer || b.timerSeq != seq {
		t.Fatal("completion changed an already queued batch")
	}
	if b.admit(third, ignoreBatchTimer) != nil || b.timer != timer || b.timerSeq != seq {
		t.Fatal("a new arrival bypassed an existing queue's timer")
	}
	queued := b.timerBatch(seq)
	if len(queued) != 2 || queued[0].reply != second.reply || queued[1].reply != third.reply || b.inFlight != 1 {
		t.Fatal("timer did not dispatch the separate pending population")
	}
	b.finishBatch()
	if b.inFlight != 0 {
		t.Fatal("completed batches were not retired")
	}
	if got := b.admit(batchRequest(), ignoreBatchTimer); len(got) != 1 {
		t.Fatal("completed work left the batcher busy")
	}
	b.finishBatch()
}

func TestGRVBatchPanicRetiresBeforeError(t *testing.T) {
	t.Parallel()
	b := &grvBatcher{priority: grvPriorityDefault}
	req := batchRequest()
	batch := b.admit(req, ignoreBatchTimer)
	b.flush(nil, batch) // panic before dispatch, through the real flush backstop
	select {
	case result := <-req.reply:
		if result.err == nil {
			t.Fatal("flush panic did not fail the waiter")
		}
	default:
		t.Fatal("flush panic orphaned the waiter")
	}
	if b.inFlight != 0 {
		t.Fatalf("panic left inFlight=%d, want 0", b.inFlight)
	}
	if got := b.admit(batchRequest(), ignoreBatchTimer); len(got) != 1 {
		t.Fatal("panic prevented the next idle dispatch")
	}
	b.finishBatch()
}

func TestGRVBatchSizeAndTimerGeneration(t *testing.T) {
	t.Parallel()
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour}
	requests := make([]grvRequest, 1000)
	var batch []grvRequest
	for i := range requests {
		requests[i] = batchRequest()
		batch = b.admit(requests[i], ignoreBatchTimer)
		if i < 999 && batch != nil {
			t.Fatalf("request %d dispatched before the 1000-request cap", i+1)
		}
	}
	if len(batch) != 1000 || b.timer != nil || len(b.pending) != 0 {
		t.Fatalf("full batch=%d timer=%v pending=%d", len(batch), b.timer != nil, len(b.pending))
	}
	for i := range requests {
		if batch[i].reply != requests[i].reply {
			t.Fatalf("request %d lost or reordered", i)
		}
	}
	stale := b.timerSeq - 1
	next := batchRequest()
	if b.admit(next, ignoreBatchTimer) != nil {
		t.Fatal("a lone queued request bypassed the adaptive window")
	}
	if got := b.timerBatch(stale); got != nil || len(b.pending) != 1 {
		t.Fatal("stopped timer callback consumed a newer queue")
	}
	seq := b.timerSeq
	if got := b.timerBatch(seq); len(got) != 1 || got[0].reply != next.reply {
		t.Fatalf("current timer returned %v, want the next request", got)
	}
	if b.timerBatch(seq) != nil {
		t.Fatal("timer dispatched the same batch twice")
	}
}

func TestGRVBatchBusyZeroWindowUsesTimer(t *testing.T) {
	t.Parallel()
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1}
	fired := make(chan uint64, 1)
	req := batchRequest()
	if b.admit(req, func(seq uint64) { fired <- seq }) != nil {
		t.Fatal("zero-window admission bypassed the batching callback")
	}
	select {
	case seq := <-fired:
		if got := b.timerBatch(seq); len(got) != 1 || got[0].reply != req.reply {
			t.Fatalf("zero-window callback returned %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("zero-window timer never fired")
	}
}

func TestGRVBatchAdaptiveWindowNoFloor(t *testing.T) {
	t.Parallel()
	b := &grvBatcher{priority: grvPriorityDefault}
	b.observeLatency(200 * time.Microsecond)
	if b.batchTime != 10*time.Microsecond {
		t.Fatalf("initial EWMA=%v, want 10us", b.batchTime)
	}
	b.observeLatency(time.Second)
	if b.batchTime != 5*time.Millisecond {
		t.Fatalf("EWMA cap=%v, want 5ms", b.batchTime)
	}
	for range 50 {
		b.observeLatency(20 * time.Microsecond)
	}
	if b.batchTime >= 100*time.Microsecond {
		t.Fatalf("fast replies cannot decay below the former floor: %v", b.batchTime)
	}
	b.batchTime = time.Hour
	b.inFlight = 1
	b.admit(batchRequest(), ignoreBatchTimer)
	b.observeLatency(200 * time.Microsecond)
	if len(b.pending) != 1 || b.timer == nil {
		t.Fatal("a reply flushed pending requests instead of only updating the EWMA")
	}
	b.timerBatch(b.timerSeq)
}

func TestGRVBatchersSeparateFullFlags(t *testing.T) {
	t.Parallel()
	root := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour}
	if root.forFlags(0x08000000) != root || root.variants != nil {
		t.Fatal("default requests must retain the priority root without a variants map")
	}
	risky := root.forFlags(0x08000001)
	other := root.forFlags(0x08000002)
	if risky == root || other == root || risky == other || root.forFlags(0x08000001) != risky {
		t.Fatal("distinct option flags share a batcher, or identical flags do not")
	}
	if risky.priority != 0x08000000 || risky.extraFlags != 1 || risky.batchTime != 0 {
		t.Fatalf("risky batcher inherited root options/window: priority=%x flags=%x window=%v", risky.priority, risky.extraFlags, risky.batchTime)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if root.forFlags(0x08000001) != risky {
				t.Error("concurrent lookup produced another batcher for identical flags")
			}
		})
	}
	wg.Wait()
	root.admit(batchRequest(), ignoreBatchTimer)
	if got := risky.admit(batchRequest(), ignoreBatchTimer); len(got) != 1 {
		t.Fatal("ordinary in-flight work prevented an idle risky dispatch")
	}
	risky.batchTime = time.Hour
	for range 999 {
		if risky.admit(batchRequest(), ignoreBatchTimer) != nil {
			t.Fatal("risky queue inherited the ordinary request")
		}
	}
	if got := risky.admit(batchRequest(), ignoreBatchTimer); len(got) != 1000 {
		t.Fatalf("risky full batch=%d, want 1000", len(got))
	}
	if len(root.pending) != 1 || root.timer == nil || other.timer != nil {
		t.Fatal("dispatch crossed full-flags queue boundaries")
	}
	root.timerBatch(root.timerSeq)
	oldRoot := root.batchTime
	risky.observeLatency(200 * time.Microsecond)
	if root.batchTime != oldRoot || other.batchTime != 0 {
		t.Fatal("reply latency crossed full-flags batcher boundaries")
	}
}
