package client

import (
	"context"
	"sync"
	"testing"
)

// TestRegisterBackgroundGoroutine_SkipsAfterClose deterministically pins the GRV-cache refresher's
// close barrier: registerBackgroundGoroutine reserves a db.wg slot BEFORE Close() begins but SKIPS the
// reservation once `closed` is set — so the lazily-started refresher can never db.wg.Add(1) after
// Close's db.wg.Wait() is (or is about to be) waiting, which panics with "sync: WaitGroup misuse: Add
// called concurrently with Wait" once the topology-monitor slot has drained the counter to zero. No
// container. Revert-proof: drop the `closed` check in registerBackgroundGoroutine and it returns true
// after Close → this test's second call reserves a slot and fails.
func TestRegisterBackgroundGoroutine_SkipsAfterClose(t *testing.T) {
	t.Parallel()
	db := &database{}
	if !db.registerBackgroundGoroutine() {
		t.Fatal("before Close, registerBackgroundGoroutine must reserve a slot (return true)")
	}
	db.wg.Done() // release the slot we just reserved (no real goroutine runs in this test)

	db.closeMu.Lock()
	db.closed = true
	db.closeMu.Unlock()

	if db.registerBackgroundGoroutine() {
		db.wg.Done() // don't leak the slot the buggy path would have reserved
		t.Fatal("after Close, registerBackgroundGoroutine must skip the Add (return false) to avoid racing db.wg.Wait()")
	}
}

// TestRegisterBackgroundGoroutine_ConcurrentCloseNoMisuse stresses the barrier: a first-opt-in
// refresher registration racing Close() must never trip "sync: WaitGroup misuse: Add called
// concurrently with Wait". A topology-monitor-analog slot (Done on ctx cancel) keeps the counter ≥1
// until Close's cancel, matching production. The loop exercises the interleaving; a panic fails the
// test (a recovered goroutine panic still aborts the run).
func TestRegisterBackgroundGoroutine_ConcurrentCloseNoMisuse(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 300; iter++ {
		ctx, cancel := context.WithCancel(context.Background())
		db := &database{ctx: ctx, cancel: cancel}
		db.wg.Add(1) // topology-monitor analog: holds the counter until Close's cancel
		go func() {
			<-ctx.Done()
			db.wg.Done()
		}()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if db.registerBackgroundGoroutine() {
				db.wg.Done() // simulate the background refresher exiting promptly
			}
		}()
		go func() {
			defer wg.Done()
			_ = (&Database{db: db}).Close() // closed = true, cancel(), wg.Wait()
		}()
		wg.Wait()
	}
}
