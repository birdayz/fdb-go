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
// GetReadVersion must capture the version under the same mutex. Concurrent use of one
// handle is in-contract: libfdb_c marshals every fdb_transaction_* call onto the network
// thread (ThreadSafeTransaction.cpp onMainThread), and the Go facade documents
// concurrent use as safe. MUST run under -race to catch a regression — revert-proof:
// restoring GetReadVersion's bare `return tx.readVersion, nil` makes this test fail
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
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			rv, err := tx.GetReadVersion(ctx)
			if err != nil {
				t.Errorf("concurrent GetReadVersion: %v", err)
				return
			}
			// rv == 0 is a legal interleaving (this GetReadVersion's ensureReadVersion
			// completed, then a commit's postCommitReset zeroed the version before the
			// capture). Negative or otherwise-torn values are not.
			if rv < 0 {
				t.Errorf("concurrent GetReadVersion returned negative version %d", rv)
				return
			}
		}
	}()

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
// rywPoisonErr: SetReadYourWritesDisable (a concurrency-safe option call) CAS-writes the
// poison while the ensureReadVersion / Commit / metrics gates Load it. Concurrent
// write||read must be -race clean. MUST run under -race to catch a regression —
// reverting the field to a plain `error` makes this a data race (the sibling contract
// test for invalidAtomicOpErr is TestAtomic_InvalidOpPoison_RaceFree).
func TestSetRYWDisable_PoisonRaceFree(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		tx := newTestTx()
		tx.hadRead.Store(true) // a prior read makes the disable poison (RFC-059)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tx.SetReadYourWritesDisable() }() // CAS-writes the poison
		go func() { defer wg.Done(); _ = tx.rywPoisonErr.Load() }()    // gate-entry read
		wg.Wait()
		if e := tx.rywPoisonErr.Load(); e == nil || e.Code != 2000 {
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
	first := tx.rywPoisonErr.Load()
	if first == nil {
		t.Fatal("first disable-with-prior-read did not poison")
	}
	tx.SetReadYourWritesDisable()
	if got := tx.rywPoisonErr.Load(); got != first {
		t.Fatalf("second disable replaced the deferred error: got %p, want %p (first wins)", got, first)
	}
}
