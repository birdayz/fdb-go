package client

import (
	"sync"
	"testing"
	"time"
)

func batchRequest() grvRequest {
	return grvRequest{reply: make(chan grvResult, 1)}
}

// fakeWindow records arms and stops; tests drive expiry through expireWindow.
type fakeWindow struct {
	arms  []time.Duration
	stops int
}

func (w *fakeWindow) arm(d time.Duration) bool { w.arms = append(w.arms, d); return true }
func (w *fakeWindow) stop()                    { w.stops++ }

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// testBatcher is a batcher on a fake window and clock: no timer ever fires.
func testBatcher(inFlight int, batchTime time.Duration) (*grvBatcher, *fakeWindow, *fakeClock) {
	w, c := &fakeWindow{}, &fakeClock{t: time.Unix(1000, 0)}
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: inFlight, batchTime: batchTime, window: w, now: c.now}
	return b, w, c
}

// expireWindow moves the clock to the open window's deadline and expires it.
func expireWindow(b *grvBatcher, c *fakeClock) []grvRequest {
	b.mu.Lock()
	if c.t.Before(b.windowDeadline) {
		c.t = b.windowDeadline
	}
	b.mu.Unlock()
	return b.expiredBatch()
}

func windowArmed(b *grvBatcher) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.windowArmed
}

func TestGRVBatchFreshBatcherSendsAtOnce(t *testing.T) {
	t.Parallel()
	for _, window := range []time.Duration{0, 50 * time.Microsecond, 5 * time.Millisecond} {
		b, w, _ := testBatcher(0, window)
		req := batchRequest()
		batch := b.admit(nil, req)
		if len(batch) != 1 || batch[0].reply != req.reply || len(w.arms) != 0 || len(b.pending) != 0 || b.inFlight != 1 {
			t.Fatalf("window %v: fresh GRV waited: dispatched=%d arms=%v pending=%d", window, len(batch), w.arms, len(b.pending))
		}
	}
}

func TestGRVBatchSerialRequestsKeepTheFastPath(t *testing.T) {
	t.Parallel()
	b, w, _ := testBatcher(0, time.Hour)
	for i := range 5 {
		if got := b.admit(nil, batchRequest()); len(got) != 1 {
			t.Fatalf("serial request %d waited for a window", i)
		}
		b.finishBatch()
	}
	if len(w.arms) != 0 {
		t.Fatalf("serial requests armed windows: %v", w.arms)
	}
}

// A request that arrives while a GRV is in flight opens a window, and every
// first request after that waits too, until a window batch of one that found
// nothing in flight completes with no other request arriving.
func TestGRVBatchOverlapReturnsToWindows(t *testing.T) {
	t.Parallel()
	b, w, c := testBatcher(0, time.Millisecond)
	first, second, third := batchRequest(), batchRequest(), batchRequest()
	if got := b.admit(nil, first); len(got) != 1 {
		t.Fatal("first request did not take the fast path")
	}
	if b.admit(nil, second) != nil || len(w.arms) != 1 || w.arms[0] != time.Millisecond {
		t.Fatalf("request behind an in-flight GRV did not open a window: arms=%v", w.arms)
	}
	b.finishBatch()
	if b.inFlight != 0 || len(b.pending) != 1 || !windowArmed(b) {
		t.Fatal("completion changed an already open window")
	}
	if b.admit(nil, third) != nil || len(w.arms) != 1 {
		t.Fatal("a later arrival bypassed or re-armed the open window")
	}
	c.advance(time.Microsecond)
	if got := b.expiredBatch(); got != nil {
		t.Fatal("window detached before its deadline")
	}
	queued := expireWindow(b, c)
	if len(queued) != 2 || queued[0].reply != second.reply || queued[1].reply != third.reply || b.inFlight != 1 || w.stops != 1 {
		t.Fatalf("window expiry returned %d requests, stops=%d", len(queued), w.stops)
	}
	if b.expiredBatch() != nil {
		t.Fatal("the same window detached twice")
	}
	b.finishBatch()
	c.advance(time.Hour) // idle time alone is no evidence of serial use
	if b.admit(nil, batchRequest()) != nil || len(w.arms) != 2 {
		t.Fatal("first request after an overlapping batch skipped the window")
	}
	if got := expireWindow(b, c); len(got) != 1 {
		t.Fatalf("lone window batch held %d requests", len(got))
	}
	b.finishBatch()
	if got := b.admit(nil, batchRequest()); len(got) != 1 {
		t.Fatal("a lone window batch with no overlap did not restore the fast path")
	}
	b.finishBatch()
	b.admit(nil, batchRequest())                                // fast path again
	if b.admit(nil, batchRequest()) != nil || !windowArmed(b) { // arrives during its flight
		t.Fatal("a request during a fast-path flight did not wait")
	}
}

// Two callers whose GRVs overlap converge on one GRV per round; with the old
// idle rule each round sent two singleton batches.
func TestGRVBatchStaggeredPairShareOneGRV(t *testing.T) {
	t.Parallel()
	b, _, c := testBatcher(0, 100*time.Microsecond)
	if got := b.admit(nil, batchRequest()); len(got) != 1 {
		t.Fatal("A did not take the fast path")
	}
	if b.admit(nil, batchRequest()) != nil {
		t.Fatal("B, arriving while A is in flight, did not wait")
	}
	if got := expireWindow(b, c); len(got) != 1 {
		t.Fatal("B's window did not send B")
	}
	b.finishBatch() // A's reply
	// A's next request arrives while B's GRV is in flight, and B's next one
	// right after B's reply: inside A's window.
	if b.admit(nil, batchRequest()) != nil {
		t.Fatal("A's next request skipped the window while B was in flight")
	}
	b.finishBatch() // B's reply
	if b.admit(nil, batchRequest()) != nil {
		t.Fatal("B's next request did not join A's window")
	}
	if got := expireWindow(b, c); len(got) != 2 {
		t.Fatalf("the callers' requests went in a batch of %d, want 2", len(got))
	}
	b.finishBatch()
	// From then on both callers return together and keep sharing one GRV.
	for round := range 3 {
		c.advance(10 * time.Microsecond)
		for caller := range 2 {
			if b.admit(nil, batchRequest()) != nil {
				t.Fatalf("round %d: caller %d skipped the window right after a shared batch", round, caller)
			}
		}
		if got := expireWindow(b, c); len(got) != 2 {
			t.Fatalf("round %d sent a batch of %d, want 2", round, len(got))
		}
		b.finishBatch()
	}
}

// Rounds separated by an idle gap longer than the window, with the second
// caller arriving a little after the first: from round 2 on, one GRV per
// round. A rule that trusts idle time sends the first caller alone each round.
func TestGRVBatchIdleGapsDoNotSplitRounds(t *testing.T) {
	t.Parallel()
	const window = 50 * time.Microsecond
	b, _, c := testBatcher(0, window)
	if got := b.admit(nil, batchRequest()); len(got) != 1 {
		t.Fatal("round 1: A did not take the fast path")
	}
	c.advance(20 * time.Microsecond)
	if b.admit(nil, batchRequest()) != nil {
		t.Fatal("round 1: B, arriving during A's flight, did not wait")
	}
	b.finishBatch()
	if got := expireWindow(b, c); len(got) != 1 {
		t.Fatalf("round 1: B's window sent %d", len(got))
	}
	b.finishBatch()
	for round := 2; round <= 5; round++ {
		c.advance(130 * time.Microsecond) // idle for more than a window
		if b.admit(nil, batchRequest()) != nil {
			t.Fatalf("round %d: A went alone after an idle gap", round)
		}
		c.advance(20 * time.Microsecond) // B arrives inside A's window
		if b.admit(nil, batchRequest()) != nil {
			t.Fatalf("round %d: B did not join A's window", round)
		}
		if got := expireWindow(b, c); len(got) != 2 {
			t.Fatalf("round %d sent a batch of %d, want 2", round, len(got))
		}
		b.finishBatch()
	}
}

// The window ends at the deadline taken on admission, even when creating its
// timer takes time.
func TestGRVBatchWindowArmsTheRemainingTime(t *testing.T) {
	t.Parallel()
	c := &fakeClock{t: time.Unix(1000, 0)}
	w := &fakeWindow{}
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Millisecond, now: c.now}
	b.newWindow = func(*database) batchWindow {
		c.advance(300 * time.Microsecond)
		return w
	}
	start := c.t
	if b.admit(nil, batchRequest()) != nil {
		t.Fatal("request behind an in-flight GRV was sent at once")
	}
	if len(w.arms) != 1 || w.arms[0] != 700*time.Microsecond {
		t.Fatalf("arms=%v, want the 700µs left of the window", w.arms)
	}
	if !b.windowDeadline.Equal(start.Add(time.Millisecond)) {
		t.Fatalf("deadline %v, want admission plus the window", b.windowDeadline.Sub(start))
	}
}

func TestGRVBatchPanicRetiresBeforeError(t *testing.T) {
	t.Parallel()
	b, _, _ := testBatcher(0, 0)
	req := batchRequest()
	batch := b.admit(nil, req)
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
	if got := b.admit(nil, batchRequest()); len(got) != 1 {
		t.Fatal("panic prevented the next serial dispatch")
	}
	b.finishBatch()
}

func TestGRVBatchFullBatchClosesItsWindow(t *testing.T) {
	t.Parallel()
	b, w, c := testBatcher(1, time.Hour)
	requests := make([]grvRequest, grvMaxBatchSize)
	var batch []grvRequest
	for i := range requests {
		requests[i] = batchRequest()
		batch = b.admit(nil, requests[i])
		if i < grvMaxBatchSize-1 && batch != nil {
			t.Fatalf("request %d dispatched before the %d-request cap", i+1, grvMaxBatchSize)
		}
	}
	if len(batch) != grvMaxBatchSize || windowArmed(b) || w.stops != 1 || len(b.pending) != 0 {
		t.Fatalf("full batch=%d armed=%v stops=%d pending=%d", len(batch), windowArmed(b), w.stops, len(b.pending))
	}
	for i := range requests {
		if batch[i].reply != requests[i].reply {
			t.Fatalf("request %d lost or reordered", i)
		}
	}
	stale := b.windowDeadline
	if b.expiredBatch() != nil {
		t.Fatal("expiry of a closed window detached something")
	}
	next := batchRequest()
	c.advance(time.Minute)
	if b.admit(nil, next) != nil {
		t.Fatal("a request behind in-flight GRVs bypassed the window")
	}
	c.t = stale // the closed window's timer firing late detaches nothing early
	if got := b.expiredBatch(); got != nil {
		t.Fatal("a superseded expiry consumed a newer window")
	}
	if got := expireWindow(b, c); len(got) != 1 || got[0].reply != next.reply {
		t.Fatalf("current window returned %v, want the next request", got)
	}
}

func TestGRVBatchZeroWindowDetachesOnExpiryNotAdmission(t *testing.T) {
	t.Parallel()
	b, w, c := testBatcher(1, 0)
	req := batchRequest()
	if b.admit(nil, req) != nil {
		t.Fatal("zero-window admission sent at once")
	}
	if len(w.arms) != 1 || w.arms[0] != 0 {
		t.Fatalf("arms=%v, want one zero window", w.arms)
	}
	if got := expireWindow(b, c); len(got) != 1 || got[0].reply != req.reply {
		t.Fatalf("zero-window expiry returned %v", got)
	}
}

func TestGRVBatchAdaptiveWindowNoFloor(t *testing.T) {
	t.Parallel()
	b, _, c := testBatcher(0, 0)
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
	b.admit(nil, batchRequest())
	b.observeLatency(200 * time.Microsecond)
	if len(b.pending) != 1 || !windowArmed(b) {
		t.Fatal("a reply flushed pending requests instead of only updating the EWMA")
	}
	expireWindow(b, c)
}

func TestGRVBatchersSeparateFullFlags(t *testing.T) {
	t.Parallel()
	root, _, rootClock := testBatcher(1, time.Hour)
	if root.forFlags(0x08000000) != root || root.variants != nil {
		t.Fatal("default requests must retain the priority root without a variants map")
	}
	risky := root.forFlags(0x08000001)
	other := root.forFlags(0x08000002)
	if risky == root || other == root || risky == other || root.forFlags(0x08000001) != risky {
		t.Fatal("distinct option flags share a batcher, or identical flags do not")
	}
	if risky.priority != 0x08000000 || risky.extraFlags != 1 || risky.batchTime != 0 || risky.window != nil {
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
	riskyClock := &fakeClock{t: rootClock.t}
	risky.window, risky.now = &fakeWindow{}, riskyClock.now
	other.window = &fakeWindow{}
	root.admit(nil, batchRequest())
	if got := risky.admit(nil, batchRequest()); len(got) != 1 {
		t.Fatal("ordinary in-flight work prevented an idle risky dispatch")
	}
	risky.batchTime = time.Hour
	for range grvMaxBatchSize - 1 {
		if risky.admit(nil, batchRequest()) != nil {
			t.Fatal("risky queue inherited the ordinary request")
		}
	}
	if got := risky.admit(nil, batchRequest()); len(got) != grvMaxBatchSize {
		t.Fatalf("risky full batch=%d, want %d", len(got), grvMaxBatchSize)
	}
	if len(root.pending) != 1 || !windowArmed(root) || windowArmed(other) {
		t.Fatal("dispatch crossed full-flags queue boundaries")
	}
	expireWindow(root, rootClock)
	oldRoot := root.batchTime
	risky.observeLatency(200 * time.Microsecond)
	if root.batchTime != oldRoot || other.batchTime != 0 {
		t.Fatal("reply latency crossed full-flags batcher boundaries")
	}
}

// A batch's storage is reused only after its flush has published to every
// waiter: while batch A is still publishing, later batches C and B reuse each
// other's storage, never A's, and A's waiter descriptors stay intact.
func TestGRVBatchReuseOverlapsPublication(t *testing.T) {
	t.Parallel()
	b, _, c := testBatcher(1, time.Millisecond) // a request in flight: every batch opens a window
	batchOf := func(reqs ...grvRequest) []grvRequest {
		for _, req := range reqs {
			if b.admit(nil, req) != nil {
				t.Fatal("windowed request was sent at once")
			}
		}
		return expireWindow(b, c)
	}
	recv := func(ch chan grvResult, who string) grvResult {
		t.Helper()
		select {
		case result := <-ch:
			return result
		case <-time.After(10 * time.Second):
			t.Fatalf("%s received no reply", who)
			return grvResult{}
		}
	}
	spare := func() []grvRequest {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.spare
	}
	// Without a database each flush panics before dispatch and its backstop
	// publishes an error to every waiter; unbuffered replies hold A there.
	a1, a2 := grvRequest{reply: make(chan grvResult)}, grvRequest{reply: make(chan grvResult)}
	batchA := batchOf(a1, a2)
	publishedA := make(chan struct{})
	go func() { defer close(publishedA); b.flush(nil, batchA) }()
	if result := recv(a1.reply, "first waiter of A"); result.err == nil {
		t.Fatal("first waiter of A received no error")
	}

	batchC := batchOf(batchRequest(), batchRequest())
	b.flush(nil, batchC)
	if got := spare(); cap(got) == 0 || &got[:1][0] != &batchC[0] {
		t.Fatal("C's storage was not kept after its flush")
	}
	b1, b2 := batchRequest(), batchRequest()
	batchB := batchOf(b1, b2)
	if &batchB[0] != &batchC[0] || &batchB[0] == &batchA[0] || spare() != nil {
		t.Fatal("B did not take C's storage, or took A's while A was publishing")
	}
	if batchA[0].reply != a1.reply || batchA[1].reply != a2.reply {
		t.Fatal("a later batch overwrote A while it was publishing")
	}

	if result := recv(a2.reply, "second waiter of A"); result.err == nil {
		t.Fatal("second waiter of A received no error")
	}
	<-publishedA
	if got := spare(); cap(got) == 0 || &got[:1][0] != &batchA[0] || batchA[0].reply != nil || batchA[1].reply != nil {
		t.Fatal("A's storage was not kept, cleared, once it had published")
	}
	b.flush(nil, batchB)
	if recv(b1.reply, "B").err == nil || recv(b2.reply, "B").err == nil {
		t.Fatal("B's waiters received no error")
	}
	if got := spare(); &got[:1][0] != &batchA[0] || batchB[0].reply != nil {
		t.Fatal("B replaced the kept spare or kept its waiters")
	}
	if b.inFlight != 1 {
		t.Fatalf("inFlight = %d, want the initial 1", b.inFlight)
	}
}
