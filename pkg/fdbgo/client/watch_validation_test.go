package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestWatchSetup_ChargesSlotAtRegistrationOrder pins that the outstanding-watch cap is charged
// SYNCHRONOUSLY in WatchSetup (registration order), matching C++ Transaction::watch's
// increaseWatchCounter at watch() time (NativeAPI.actor.cpp:5694) — NOT in the async WatchPoll
// goroutine. Charging it in the async poll let two Watch() calls under MAX_WATCHES=1 race for the
// slot, so the first-registered watch could lose to the second (codex). Revert-proof: with the
// charge back in WatchPoll, the second WatchSetup below returns nil instead of too_many_watches.
func TestWatchSetup_ChargesSlotAtRegistrationOrder(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	if err := db.SetMaxWatches(1); err != nil { // cap = 1
		t.Fatalf("SetMaxWatches(1): %v", err)
	}

	// First WatchSetup takes the only slot synchronously, before any async poll.
	tx1 := db.CreateTransaction()
	if _, _, _, _, err := tx1.WatchSetup(ctx, []byte(t.Name()+"_k1")); err != nil {
		t.Fatalf("first WatchSetup must get the only slot, got %v", err)
	}

	// Second WatchSetup (next in registration order) must fail with too_many_watches (1032) — at
	// SETUP, not later in the poll. This is the deterministic, registration-ordered behavior.
	tx2 := db.CreateTransaction()
	if _, _, _, _, err := tx2.WatchSetup(ctx, []byte(t.Name()+"_k2")); fdbCodeOf(err) != ErrTooManyWatches {
		t.Fatalf("second WatchSetup must be too_many_watches (1032) at registration, got %v", err)
	}

	// Releasing the first slot (as WatchPoll does on completion) frees it for the next registration.
	db.db.releaseWatch()
	tx3 := db.CreateTransaction()
	if _, _, _, _, err := tx3.WatchSetup(ctx, []byte(t.Name()+"_k3")); err != nil {
		t.Fatalf("after release a slot is free; third WatchSetup must succeed, got %v", err)
	}
	db.db.releaseWatch() // free the slot tx3 took (no WatchPoll runs in this test)
}

// TestWatchSetup_CancelledTxnDoesNotLeakSlot pins that a watch whose setup fails (here: a cancelled
// transaction, caught by ensureReadVersion's leading checkCancelled) RELEASES the outstanding-watch
// slot it reserved, rather than leaking it. WatchSetup acquires the slot synchronously (round 11),
// so every setup-error path must release it (the C++ catch → decreaseWatchCounter analogue).
// Revert-proof: drop the release on the setup-error path and the cancelled watch holds the only slot,
// so the second WatchSetup below fails 1032 instead of succeeding.
func TestWatchSetup_CancelledTxnDoesNotLeakSlot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	if err := db.SetMaxWatches(1); err != nil {
		t.Fatalf("SetMaxWatches(1): %v", err)
	}

	tx1 := db.CreateTransaction()
	tx1.Cancel()
	if _, _, _, _, err := tx1.WatchSetup(ctx, []byte(t.Name()+"_k1")); fdbCodeOf(err) != 1025 {
		t.Fatalf("WatchSetup on a cancelled txn must return transaction_cancelled (1025), got %v", err)
	}

	// The slot the cancelled watch briefly reserved must be freed — a fresh watch under cap=1 succeeds.
	tx2 := db.CreateTransaction()
	if _, _, _, _, err := tx2.WatchSetup(ctx, []byte(t.Name()+"_k2")); err != nil {
		t.Fatalf("the cancelled watch must not leak its slot; fresh WatchSetup must succeed, got %v", err)
	}
	db.db.releaseWatch() // free tx2's slot (no WatchPoll runs in this test)
}

// TestWatchSetup_CancellationOutranksWatchCap pins that transaction_cancelled (1025) out-ranks
// too_many_watches (1032): a Watch() on an already-Cancel()ed txn must report 1025 even when the cap
// is full/zero, because the cancellation check runs BEFORE the slot acquire (codex). Revert-proof:
// with the acquire first, a 0-cap returns 1032 for the cancelled txn.
func TestWatchSetup_CancellationOutranksWatchCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	if err := db.SetMaxWatches(0); err != nil { // 0-cap: any acquire would be 1032
		t.Fatalf("SetMaxWatches(0): %v", err)
	}
	tx := db.CreateTransaction()
	tx.Cancel()
	if _, _, _, _, err := tx.WatchSetup(ctx, []byte(t.Name()+"_k")); fdbCodeOf(err) != 1025 {
		t.Fatalf("Watch on a cancelled txn (cap 0) must be transaction_cancelled (1025), not 1032, got %v", err)
	}
}

// TestWatchSetup_FailedSetupDoesNotPoisonNextWatch pins that a WatchSetup which mints the per-txn
// watchCtx and then fails (here: a pre-cancelled per-call context fails the GRV) CLEARS that context,
// so a later watch on the same active transaction does not reuse a now-cancelled child and fail with
// context.Canceled (codex). Revert-proof: without the clear, the second watch's context is already
// cancelled.
func TestWatchSetup_FailedSetupDoesNotPoisonNextWatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	tx := db.CreateTransaction()

	// First watch: a pre-cancelled per-call ctx fails setup at the GRV. THIS call minted watchCtx
	// (from the cancelled ctx → itself cancelled), so the failure must clear it.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, _, _, _, err := tx.WatchSetup(cctx, []byte(t.Name()+"_k1")); err == nil {
		t.Fatal("WatchSetup with a pre-cancelled per-call ctx must fail")
	}

	// Second watch on the SAME active txn with a LIVE ctx must mint a fresh (live) context.
	_, _, _, watchCtx2, err := tx.WatchSetup(ctx, []byte(t.Name()+"_k2"))
	if err != nil {
		t.Fatalf("the failed first setup must not poison the next watch, got %v", err)
	}
	if watchCtx2 == nil {
		t.Fatal("the next watch must return a non-nil context")
	}
	if watchCtx2.Err() != nil {
		t.Fatalf("the next watch's context must be LIVE (not the poisoned cancelled one), got %v", watchCtx2.Err())
	}
	db.db.releaseWatch() // free the slot the second watch took (no WatchPoll runs here)
}

// TestWatchSetup_TerminalErrorsOutrankCap pins that a doomed watch surfaces its REAL terminal error
// rather than having a full/0 cap mask it with too_many_watches (1032): the caller-ctx and
// SetTimeout gates run BEFORE the slot acquire (codex). Revert-proof: charge the cap first and a
// cancelled/timed-out setup returns 1032 instead of context.Canceled / 1031.
func TestWatchSetup_TerminalErrorsOutrankCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	if err := db.SetMaxWatches(0); err != nil { // full cap: any acquire would be 1032
		t.Fatalf("SetMaxWatches(0): %v", err)
	}

	// An already-cancelled caller context must surface as context.Canceled, not masked by the cap.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	tx1 := db.CreateTransaction()
	if _, _, _, _, err := tx1.WatchSetup(cctx, []byte(t.Name()+"_k1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller ctx under a full cap must be context.Canceled, not 1032, got %v", err)
	}

	// An expired transaction SetTimeout must surface as transaction_timed_out (1031), not masked.
	tx2 := db.CreateTransaction()
	tx2.creationTime = time.Now().Add(-time.Second)
	tx2.SetTimeout(500) // deadline anchored 500ms in the PAST
	if _, _, _, _, err := tx2.WatchSetup(ctx, []byte(t.Name()+"_k2")); fdbCodeOf(err) != 1031 {
		t.Fatalf("expired txn timeout under a full cap must be 1031, not 1032, got %v", err)
	}
}

// TestWatchSetup_CancellationOutranksKeyValidation pins that terminal txn/ctx state out-ranks the
// key-legality validation: a Cancel()ed txn watching an ILLEGAL (system) key must return 1025, not
// key_outside_legal_range (2004) — matching the other reads' entry-timebomb precedence (codex).
// Revert-proof: with the key validation before the terminal checks, this returns 2004.
func TestWatchSetup_CancellationOutranksKeyValidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	tx := db.CreateTransaction()
	tx.Cancel()
	if _, _, _, _, err := tx.WatchSetup(ctx, []byte("\xff\x05")); fdbCodeOf(err) != 1025 {
		t.Fatalf("cancelled txn + illegal key must be transaction_cancelled (1025), not 2004, got %v", err)
	}
}

// TestWatchSetup_RejectsSystemAndOversizedKeys pins the eager legal-range + key-size validation
// C++ RYW watch performs before registering (ReadYourWrites.actor.cpp:2450-2456). A normal
// (non-system) transaction must not be able to register a watch on a \xff system key
// (key_outside_legal_range 2004) or an oversized key (key_too_large 2102) — libfdb_c rejects both.
// The checks run BEFORE the read version, so no FDB container is needed. Revert-proof: removing the
// checks lets WatchSetup proceed past them.
func TestWatchSetup_RejectsSystemAndOversizedKeys(t *testing.T) {
	t.Parallel()

	t.Run("system_key_2004", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		_, _, _, _, err := tx.WatchSetup(context.Background(), []byte("\xff\x05"))
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != 2004 {
			t.Fatalf("Watch on a \\xff system key must be key_outside_legal_range (2004), got %v", err)
		}
	})

	t.Run("oversized_key_2102", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		big := make([]byte, 20000) // > KEY_SIZE_LIMIT (10000); all-zero so it passes the legal-range gate
		_, _, _, _, err := tx.WatchSetup(context.Background(), big)
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != 2102 {
			t.Fatalf("Watch on an oversized key must be key_too_large (2102), got %v", err)
		}
	})
}
