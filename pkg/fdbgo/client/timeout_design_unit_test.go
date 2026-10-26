package client

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/wire"
)

// Design D4 (RFC-282): one runtime timer per transaction, nothing when TIMEOUT is unset.

func TestDesignTimeoutOneTimerPerTransaction(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.readErrMu.Lock()
	tx.timeoutArm++
	tx.timeoutArmed = true
	tx.armDesignTimerLocked(time.Hour)
	first := tx.designTimer
	tx.timeoutArm++ // a re-arm, as SetTimeout or a reset makes
	tx.armDesignTimerLocked(time.Millisecond)
	reused := tx.designTimer == first && first != nil
	tx.readErrMu.Unlock()
	if !reused {
		t.Fatal("a re-arm created a second timer")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fdbErr *wire.FDBError
		if errors.As(tx.readEntryError(ctx), &fdbErr) && fdbErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the transaction's timer never delivered transaction_timed_out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDesignTimeoutStaleArmIsRejected(t *testing.T) {
	t.Parallel()
	tx := &Transaction{creationTime: time.Now()}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.SetTimeout(60000)
	tx.SetTimeout(0) // disarm via SetTimeout(0)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("a disarmed timer poisoned the transaction: %v", err)
	}

	// Re-arm with a future deadline (e.g. SetTimeout or Reset while an earlier
	// AfterFunc callback was already runnable before acquiring readErrMu):
	// fireDesignTimeout must re-arm for the remaining budget and NOT deliver 1031.
	tx.SetTimeout(60000)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("an in-flight callback from an earlier arm poisoned a re-armed transaction: %v", err)
	}

	// Reset also disarms the old generation and re-arms the new one with a future deadline.
	release()
	tx.Reset()
	ctx2, release2 := tx.opContext(context.Background())
	defer release2()
	tx.SetTimeout(60000)
	tx.fireDesignTimeout()
	if err := tx.readEntryError(ctx2); err != nil {
		t.Fatalf("an in-flight callback after Reset poisoned the new generation: %v", err)
	}

	// Once designDeadline has elapsed, fireDesignTimeout delivers 1031.
	tx.readErrMu.Lock()
	tx.designDeadline = monoNow()
	tx.readErrMu.Unlock()
	tx.fireDesignTimeout()
	var fdbErr *wire.FDBError
	if err := tx.readEntryError(ctx2); !errors.As(err, &fdbErr) || fdbErr.Code != 1031 {
		t.Fatalf("expired weak timer fire = %v, want FDBError 1031", err)
	}
}

// The timer holds the transaction weakly: an armed transaction that is
// dropped is collectable before its deadline.
func TestDesignTimeoutDoesNotRetainTransaction(t *testing.T) {
	t.Parallel()
	arm := func() weak.Pointer[Transaction] {
		tx := &Transaction{}
		tx.readErrMu.Lock()
		tx.timeoutArm++
		tx.timeoutArmed = true
		tx.armDesignTimerLocked(time.Hour) // still armed when dropped
		tx.readErrMu.Unlock()
		return weak.Make(tx)
	}
	w := arm()
	for i := 0; i < 20 && w.Value() != nil; i++ {
		runtime.GC()
	}
	if w.Value() != nil {
		t.Fatal("an armed timer kept a dropped transaction alive")
	}
}

func TestDesignTimeoutUnsetArmsNothing(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	tx.SetTimeout(0)
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.designTimer != nil || tx.timeoutNode != nil || tx.timeoutArmed {
		t.Fatal("TIMEOUT unset armed a timer")
	}
}

// The production check recognizes an unmodified database queue and rejects
// a queue that a test drives by hand.
func TestDesignTimeoutProductionQueue(t *testing.T) {
	t.Parallel()
	if !newTimeoutQueue().production() {
		t.Fatal("an unmodified queue is not recognized")
	}
	q := newTimeoutQueue()
	q.now = func() int64 { return 0 } // a hand-driven clock
	if q.production() {
		t.Fatal("a manual queue was taken for the production one")
	}
}

func TestDesignTimeoutTransactScopedPoolAndExpiry(t *testing.T) {
	t.Parallel()
	db := &Database{db: &database{timeouts: newTimeoutQueue()}}
	var observedTx *Transaction
	var attempts int
	_, err := db.Transact(context.Background(), func(tx *Transaction) (any, error) {
		observedTx = tx
		attempts++
		tx.SetTimeout(30000)
		tx.readErrMu.Lock()
		hasPooled := tx.transactScoped && tx.designEntry != nil && tx.timeoutNode == nil && tx.designTimer == nil
		tx.readErrMu.Unlock()
		if !hasPooled {
			t.Fatal("Database.Transact with TIMEOUT did not select a pooled designTimerEntry")
		}
		if attempts == 1 {
			return nil, &wire.FDBError{Code: ErrNotCommitted}
		}
		return "ok", nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("Database.Transact = %v after %d attempts, want nil after 2", err, attempts)
	}
	observedTx.readErrMu.Lock()
	releasedAfterTransact := !observedTx.transactScoped && observedTx.designEntry == nil && !observedTx.timeoutArmed
	observedTx.readErrMu.Unlock()
	if !releasedAfterTransact {
		t.Fatal("Database.Transact did not release its pooled designTimerEntry on exit")
	}

	var observedReadTx *Transaction
	_, err = db.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		observedReadTx = tx
		tx.SetTimeout(30000)
		tx.readErrMu.Lock()
		hasPooled := tx.transactScoped && tx.designEntry != nil && tx.timeoutNode == nil && tx.designTimer == nil
		tx.readErrMu.Unlock()
		if !hasPooled {
			t.Fatal("Database.ReadTransact with TIMEOUT did not select a pooled designTimerEntry")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Database.ReadTransact = %v", err)
	}
	observedReadTx.readErrMu.Lock()
	releasedAfterReadTransact := !observedReadTx.transactScoped && observedReadTx.designEntry == nil && !observedReadTx.timeoutArmed
	observedReadTx.readErrMu.Unlock()
	if !releasedAfterReadTransact {
		t.Fatal("Database.ReadTransact did not release its pooled designTimerEntry on exit")
	}

	tx := db.createTransaction(true)
	defer tx.releaseTransactTimeout()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.SetTimeout(30000)
	tx.SetTimeout(1) // tighten to 1 ms -> resets pooled timer
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fdbErr *wire.FDBError
		if errors.As(tx.readEntryError(ctx), &fdbErr) && fdbErr.Code == 1031 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transactScoped pooled timer never delivered transaction_timed_out")
		}
		time.Sleep(time.Millisecond)
	}
}

// Deterministic test for pooled designTimerEntry reuse: when an entry armed
// for transaction A is reused by transaction B with a later deadline (skipping
// Timer.Reset because nextFire is in the future and <= B's deadline), an early
// fire from A's schedule re-arms for B's remaining budget without delivering a
// false 1031; firing once B's deadline has elapsed delivers 1031.
func TestDesignTimeoutPooledReuseEarlyFireReArmsWithoutFalseTimeout(t *testing.T) {
	t.Parallel()
	// Use a dedicated entry owned by this test so parallel tests pulling from
	// designTimerPool cannot race with our deterministic fire() calls.
	entry := &designTimerEntry{}
	txA := &Transaction{transactScoped: true, designEntry: entry}
	txA.readErrMu.Lock()
	txA.timeoutArm = 1
	txA.timeoutArmed = true
	txA.armDesignTimerLocked(10 * time.Minute)
	txA.stopDesignTimerLocked()
	txA.designEntry = nil
	txA.readErrMu.Unlock()
	if entry.t != nil {
		defer entry.t.Stop()
	}

	// Firing while disarmed (tx == nil) is a no-op and clears nextFire.
	entry.fire()
	entry.mu.Lock()
	if entry.tx != nil || entry.nextFire != 0 {
		entry.mu.Unlock()
		t.Fatalf("disarmed fire left tx=%v nextFire=%d, want nil/0", entry.tx, entry.nextFire)
	}
	// Simulate an entry whose underlying timer is still scheduled for an earlier
	// deadline (10 minutes from now).
	earlyFire := monoNow() + int64(10*time.Minute)
	entry.nextFire = earlyFire
	entry.mu.Unlock()

	txB := &Transaction{transactScoped: true, designEntry: entry}
	defer func() {
		txB.readErrMu.Lock()
		txB.stopDesignTimerLocked()
		txB.designEntry = nil
		txB.readErrMu.Unlock()
	}()
	ctx, release := txB.opContext(context.Background())
	defer release()
	txB.readErrMu.Lock()
	txB.timeoutArm = 1
	txB.timeoutArmed = true
	txB.armDesignTimerLocked(time.Hour) // deadline > earlyFire -> skips Reset
	txB.readErrMu.Unlock()

	entry.mu.Lock()
	skippedReset := entry.nextFire == earlyFire
	wantDeadline := entry.deadline
	entry.mu.Unlock()
	if !skippedReset {
		t.Fatalf("expected armDesignTimerLocked to keep nextFire=%d, got %d", earlyFire, entry.nextFire)
	}

	// Deliver the early fire while txB's deadline is still ~1h in the future.
	entry.fire()
	if err := txB.readEntryError(ctx); err != nil {
		t.Fatalf("early fire from previous owner falsely timed out txB: %v", err)
	}
	entry.mu.Lock()
	rearmed := entry.nextFire == wantDeadline
	// Now advance txB's deadline to the present and fire again.
	entry.deadline = monoNow()
	entry.mu.Unlock()
	if !rearmed {
		t.Fatalf("early fire did not re-arm nextFire to %d", wantDeadline)
	}

	entry.fire()
	var fdbErr *wire.FDBError
	if err := txB.readEntryError(ctx); !errors.As(err, &fdbErr) || fdbErr.Code != 1031 {
		t.Fatalf("expired pooled timer fire = %v, want FDBError 1031", err)
	}
}
