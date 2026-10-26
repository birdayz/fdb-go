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
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.readErrMu.Lock()
	tx.timeoutArm++
	tx.timeoutArmed = true
	tx.armDesignTimerLocked(time.Hour)
	tx.disarmTimeoutLocked() // e.g. SetTimeout(0) after the arm
	tx.readErrMu.Unlock()
	tx.fireDesignTimeout() // a callback that raced the disarm
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("a disarmed timer poisoned the transaction: %v", err)
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
	for range 10 {
		tx := &Transaction{transactScoped: true}
		tx.SetTimeout(30000)
		tx.releaseTransactTimeout()
	}
	tx := &Transaction{transactScoped: true}
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
// for transaction A is released and reused by transaction B with a later
// deadline (skipping Timer.Reset because nextFire is in the future and <= B's
// deadline), an early fire from A's schedule re-arms for B's remaining budget
// without delivering a false 1031; firing once B's deadline has elapsed
// delivers 1031.
func TestDesignTimeoutPooledReuseEarlyFireReArmsWithoutFalseTimeout(t *testing.T) {
	t.Parallel()
	txA := &Transaction{transactScoped: true}
	txA.readErrMu.Lock()
	txA.timeoutArm = 1
	txA.timeoutArmed = true
	txA.armDesignTimerLocked(10 * time.Minute)
	entry := txA.designEntry
	txA.readErrMu.Unlock()
	if entry == nil {
		t.Fatal("expected pooled designTimerEntry on txA")
	}
	txA.releaseTransactTimeout()

	// Firing while the entry is in the pool (tx == nil) is a no-op and clears nextFire.
	entry.fire()
	entry.mu.Lock()
	if entry.tx != nil || entry.nextFire != 0 {
		entry.mu.Unlock()
		t.Fatalf("pooled fire left tx=%v nextFire=%d, want nil/0", entry.tx, entry.nextFire)
	}
	// Simulate an entry in the pool whose underlying timer is still scheduled
	// for an earlier deadline (10 minutes from now).
	earlyFire := monoNow() + int64(10*time.Minute)
	entry.nextFire = earlyFire
	entry.mu.Unlock()

	txB := &Transaction{transactScoped: true, designEntry: entry}
	defer txB.releaseTransactTimeout()
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
