package client

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

// Design D4 (DESIGN.md): one runtime timer per transaction, nothing when TIMEOUT is unset.

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
	for fdbCodeOf(tx.readEntryError(ctx)) != 1031 {
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
