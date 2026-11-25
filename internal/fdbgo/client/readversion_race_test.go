package client

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestGetReadVersion_ConcurrentWithCommit_RaceFree hammers GetReadVersion against the
// commit path on ONE transaction handle (RFC-175 E1). Every successful Commit runs
// postCommitReset, which writes readVersion/hasReadVersion under readVersionMu;
// GetReadVersion must retain the version captured by its own GRV. Concurrent use of one
// handle is in-contract: libfdb_c marshals every fdb_transaction_* call onto the network
// thread (ThreadSafeTransaction.cpp onMainThread), and the Go facade documents
// concurrent use as safe. MUST run under -race to catch a regression — revert-proof:
// restoring a bare `return tx.readVersion, nil` makes this test fail
// under -race (verified at introduction).
func TestGetReadVersion_ConcurrentWithCommit_RaceFree(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	key := []byte(t.Name() + "_key")

	done := make(chan struct{})
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var first sync.Once
		defer first.Do(func() { close(ready) })
		for {
			select {
			case <-done:
				return
			default:
			}
			rv, err := tx.GetReadVersion(ctx)
			if fdbCodeOf(err) == 1025 {
				// Confirmed commit retires outstanding reads in Go's auto-reuse
				// incarnation. This is not a failure of the next incarnation.
				continue
			}
			if err != nil {
				t.Errorf("concurrent GetReadVersion: %v", err)
				return
			}
			// A successful GRV owns its answer; resetting the mutable handle
			// must never substitute zero or another incarnation's version.
			if rv <= 0 {
				t.Errorf("concurrent GetReadVersion returned non-positive version %d", rv)
				return
			}
			first.Do(func() { close(ready) })
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("concurrent reader did not establish a real initial GRV")
	}

	// Write-only commits: no read conflict ranges, so these cannot hit not_committed —
	// every iteration exercises commit success → postCommitReset → readVersion write.
	for i := 0; i < 40; i++ {
		tx.Set(key, []byte{byte(i)})
		if err := tx.Commit(ctx); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()

	// Single-goroutine epilogue: a quiesced handle must produce a real version.
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("final GetReadVersion: %v", err)
	}
	if rv <= 0 {
		t.Fatalf("final GetReadVersion: got %d, want > 0", rv)
	}
}

// TestSetRYWDisable_PoisonRaceFree pins the RFC-175 E2 deferred-error contract for
// the RYW-disable source: SetReadYourWritesDisable (a concurrency-safe option call) CAS-writes the
// poison while the ensureReadVersion / Commit / metrics gates Load it. Concurrent
// write||read must be -race clean. MUST run under -race to catch a regression —
// reverting the field to a plain `error` makes this a data race (the sibling contract
// test for the invalid-atomic source is TestAtomic_InvalidOpPoison_RaceFree).
func TestSetRYWDisable_PoisonRaceFree(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		tx := newTestTx()
		tx.hadRead.Store(true) // a prior read makes the disable poison (RFC-059)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tx.SetReadYourWritesDisable() }() // CAS-writes the poison
		go func() { defer wg.Done(); _ = tx.deferredErr.Load() }()     // gate-entry read
		wg.Wait()
		if e := tx.deferredErr.Load(); e == nil || e.Code != 2000 {
			t.Fatalf("poison after disable-with-prior-read: got %v, want code 2000", e)
		}
	}
}

// TestSetRYWDisable_FirstErrorWins pins the write-once half of the E2 contract: a second
// SetReadYourWritesDisable never replaces the already-recorded deferred error (C++
// doOnMainThreadVoid returns without running the op when deferredError is already set,
// ThreadHelper.actor.h:44-55).
func TestSetRYWDisable_FirstErrorWins(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.hadRead.Store(true)
	tx.SetReadYourWritesDisable()
	first := tx.deferredErr.Load()
	if first == nil {
		t.Fatal("first disable-with-prior-read did not poison")
	}
	tx.SetReadYourWritesDisable()
	if got := tx.deferredErr.Load(); got != first {
		t.Fatalf("second disable replaced the deferred error: got %p, want %p (first wins)", got, first)
	}
}
