package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// noProxyDB has no GRV proxies, so a GRV RPC backs off until its context ends.
func noProxyDB(t *testing.T) (*database, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &database{ctx: ctx, proxiesChanged: make(chan struct{}), failMon: newFailureMonitor()}, cancel
}

func batcherInFlight(b *grvBatcher) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

// A lone request's RPC is bounded by its caller's ctx: when that deadline
// returns the caller, its batch is already retired.
func TestGRVSoleRequestBoundedByCallerContext(t *testing.T) {
	t.Parallel()
	db, _ := noProxyDB(t)
	b := &grvBatcher{priority: grvPriorityDefault}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, _, err := b.getReadVersion(db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if n := batcherInFlight(b); n != 0 {
		t.Fatalf("GRV_INLINE: %d batch still in flight after its only caller returned", n)
	}
}

// Close still ends an RPC run on a caller whose own context never fires.
func TestGRVSoleRequestEndsOnClose(t *testing.T) {
	t.Parallel()
	db, closeDB := noProxyDB(t)
	b := &grvBatcher{priority: grvPriorityDefault}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := b.getReadVersion(db, context.Background(), grvPriorityDefault, types.SpanContext{}, nil, false, false)
		done <- err
	}()
	closeDB()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the database's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GRV run on the caller ignored database Close")
	}
	if n := batcherInFlight(b); n != 0 {
		t.Fatalf("%d batch still in flight after Close", n)
	}
}

// A batch with other members keeps running under db.ctx: one caller's deadline
// must never end its siblings' GRV.
func TestGRVSharedBatchOutlivesOneCallersContext(t *testing.T) {
	t.Parallel()
	db, closeDB := noProxyDB(t)
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, window: &fakeWindow{}}
	siblings := make([]grvRequest, grvMaxBatchSize-1)
	for i := range siblings {
		siblings[i] = batchRequest()
		if b.admit(db, siblings[i]) != nil {
			t.Fatal("precondition: sibling dispatched early")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, _, err := b.getReadVersion(db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	for i, s := range siblings {
		select {
		case r := <-s.reply:
			t.Fatalf("GRV_SHARED_CANCELLED: sibling %d got %v from another caller's deadline", i, r.err)
		default:
		}
	}
	if n := batcherInFlight(b); n != 2 {
		t.Fatalf("inFlight = %d, want the shared batch still running", n)
	}
	closeDB()
	for i, s := range siblings {
		select {
		case r := <-s.reply:
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("sibling %d: %v, want Close", i, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("sibling %d never answered after Close", i)
		}
	}
}

// A window's timerfd expires on its own: the worker detaches the batch and
// sends it under db.ctx, and Close then fails the RPC for its waiter.
func TestGRVWindowExpirySendsTheBatch(t *testing.T) {
	t.Parallel()
	db, closeDB := noProxyDB(t)
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Millisecond}
	req := batchRequest()
	if b.admit(db, req) != nil {
		t.Fatal("a request behind an in-flight GRV was sent at once")
	}
	if _, ok := b.window.(hrWindow); !ok {
		t.Fatalf("window is %T, want the timerfd window on Linux", b.window)
	}
	deadline := time.Now().Add(time.Minute)
	for windowArmed(b) {
		if time.Now().After(deadline) {
			t.Fatal("the window never expired")
		}
		time.Sleep(time.Millisecond)
	}
	if n := batcherInFlight(b); n != 2 {
		t.Fatalf("inFlight = %d after expiry, want the held batch plus the expired one", n)
	}
	closeDB()
	select {
	case r := <-req.reply:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("expired batch after Close: %v, want the database's cancellation", r.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("expired batch never answered")
	}
}

// Close wakes the window's worker: a request waiting in an open window gets an
// error, the worker exits, and a window opened after Close sends at once.
func TestGRVWindowCloseFailsPendingAndStopsWorker(t *testing.T) {
	t.Parallel()
	db, closeDB := noProxyDB(t)
	b := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour}
	req := batchRequest()
	if b.admit(db, req) != nil {
		t.Fatal("a request behind an in-flight GRV was sent at once")
	}
	db.closeMu.Lock()
	db.closed = true // what Database.Close sets before cancelling
	db.closeMu.Unlock()
	closeDB()
	select {
	case r := <-req.reply:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("pending request after Close: %v, want the database's cancellation", r.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Close stranded a request waiting in a window")
	}
	exited := make(chan struct{})
	go func() {
		db.wg.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatal("the window worker did not exit after Close")
	}
	if got := b.admit(db, batchRequest()); len(got) != 1 {
		t.Fatalf("a window opened after Close held its request (batch %d)", len(got))
	}
}
