package client

import (
	"context"
	"testing"
	"time"
)

// TestWatch_AbortBeforeCommitReleasesSlot: the RFC-170 deferral added a pre-commit wait
// (AwaitWatchCommit). When a watch aborts THERE — reset/Cancel/ctx before the txn commits — WatchPoll (the
// path that releases the outstanding-watch slot WatchSetup reserved) never runs, so the facade must release
// the slot on that abort path. Else it leaks and later watches fail with too_many_watches under MAX_WATCHES.
// This drives the exact client methods the facade's abort path uses (WatchSetup → WatchActivation → Cancel →
// AwaitWatchCommit → ReleaseWatch). Revert-proof: drop the ReleaseWatch() call → outstandingWatches stays 1.
func TestWatch_AbortBeforeCommitReleasesSlot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, _ := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_k")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx := db.CreateTransaction()
	before := db.db.outstandingWatches.Load()
	_, rv, _, watchCtx, watchCancel, setupErr := tx.WatchSetup(ctx, key) // reserves the slot
	if setupErr != nil {
		t.Fatalf("WatchSetup: %v", setupErr)
	}
	act := tx.WatchActivationFor(watchCtx)
	if got := db.db.outstandingWatches.Load(); got != before+1 {
		t.Fatalf("WatchSetup must reserve one slot: outstandingWatches=%d, want %d", got, before+1)
	}

	// Abort BEFORE commit: Cancel the txn → cancelWatches fires the activation abandoned + cancels watchCtx.
	tx.Cancel()
	if _, err := tx.AwaitWatchCommit(watchCtx, act, rv); err == nil {
		t.Fatal("AwaitWatchCommit must return an error for a Cancel'd (aborted) watch")
	}
	// The facade's abort path releases the slot + deregisters the scoped context.
	tx.ReleaseWatch()
	watchCancel()

	if got := db.db.outstandingWatches.Load(); got != before {
		t.Fatalf("#8 P1: watch slot leaked after a pre-commit abort — outstandingWatches=%d, want %d", got, before)
	}
}

func TestWatchActivationForSetupSurvivesInterveningReset(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, _ := newSimTestDB(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	_, _, _, watchCtx, watchCancel, err := tx.WatchSetup(ctx, []byte(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer watchCancel()
	defer tx.ReleaseWatch()
	captured := tx.WatchActivationFor(watchCtx)
	tx.Reset()
	if got := tx.WatchActivationFor(watchCtx); got != captured {
		t.Fatal("watch setup rebound to the replacement transaction activation")
	}
	select {
	case <-captured.done:
		if !captured.abandoned {
			t.Fatal("reset completed captured activation without abandoning it")
		}
	default:
		t.Fatal("reset did not resolve the captured watch activation")
	}
	if current := tx.WatchActivation(); current == captured {
		t.Fatal("reset did not rotate the current watch activation")
	}
}
