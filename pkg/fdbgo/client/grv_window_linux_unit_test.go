//go:build linux

package client

import (
	"testing"
	"time"
)

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
	if _, ok := b.window.(*hrWindow); !ok {
		t.Fatalf("window is %T, want the timerfd window on Linux", b.window)
	}
	awaitExpiry(t, b)
	if n := batcherInFlight(b); n != 2 {
		t.Fatalf("inFlight = %d after expiry, want the held batch plus the expired one", n)
	}
	closeNoProxyDB(db, closeDB)
	awaitReply(t, req, "expired batch after Close")
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
	closeNoProxyDB(db, closeDB)
	awaitReply(t, req, "request waiting in a window at Close")
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
