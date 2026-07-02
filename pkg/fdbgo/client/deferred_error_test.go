package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func deferredCodeOf(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

// TestDeferredError_BadAtomicGatesReads pins the single-slot deferred-error model
// (RFC-175 E2): in libfdb_c a bad Atomic op-code throws on the network thread into
// ISingleThreadTransaction::deferredError, and EVERY subsequent future-returning op
// re-throws it via checkDeferredError (ThreadSafeTransaction.cpp: get :431, getKey
// :441, getReadVersion :421, commit :669, getApproximateSize :715) — reads are
// poisoned, not just Commit, and the transaction stays alive-but-poisoned (a second
// Commit returns the same 2018, never "transaction not active") until reset.
// Revert-proof: gating only Commit (the pre-E2 shape) reds every read assertion here.
func TestDeferredError_BadAtomicGatesReads(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"
	k := []byte(prefix + "k")

	tx := db.CreateTransaction()
	tx.Atomic(MutClearRange, k, []byte("v")) // op-code 1 — NOT an atomic op → deferred 2018

	if _, err := tx.Get(ctx, k); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("Get after bad Atomic: want 2018, got %v", err)
	}
	if _, _, err := tx.GetRange(ctx, k, append(k, 0xff), 10); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("GetRange after bad Atomic: want 2018, got %v", err)
	}
	if _, err := tx.GetReadVersion(ctx); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("GetReadVersion after bad Atomic: want 2018, got %v", err)
	}
	if _, err := tx.GetApproximateSize(); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("GetApproximateSize after bad Atomic: want 2018, got %v", err)
	}
	if _, err := tx.GetVersionstamp(); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("GetVersionstamp after bad Atomic: want 2018, got %v", err)
	}
	if err := tx.Commit(ctx); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("Commit after bad Atomic: want 2018, got %v", err)
	}
	// Poisoned-but-alive: the SECOND commit surfaces the SAME deferred error (C++
	// re-throws deferredError until reset) — NOT "transaction not active".
	if err := tx.Commit(ctx); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("second Commit after bad Atomic: want 2018 again, got %v", err)
	}
	// OnError(2018) is non-retryable: it returns the error and does NOT reset the txn.
	if err := tx.OnError(ctx, &wire.FDBError{Code: ErrInvalidMutationType}); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("OnError(2018): want 2018 back (non-retryable), got %v", err)
	}
	// Reset clears the slot (C++ resetRyow, ReadYourWrites.actor.cpp:2719): the handle
	// is fully usable again.
	tx.Reset()
	tx.Set(k, []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit after Reset: %v", err)
	}
	got, err := db.Transact(ctx, func(tx *Transaction) (any, error) { return tx.Get(ctx, k) })
	if err != nil || string(got.([]byte)) != "v" {
		t.Fatalf("read-back after Reset: got %v / %v, want \"v\"", got, err)
	}
}

// TestSetRYWDisable_PoisonDoesNotApplyOption pins the C++ throw-before-assign order:
// RYW setOptionImpl throws client_invalid_operation BEFORE options.readYourWritesDisabled
// is set (ReadYourWrites.actor.cpp:2534-2542), so a poisoning disable leaves the option
// UNAPPLIED (and via the ensureReadVersion gate, a Watch on the poisoned txn surfaces the
// deferred 2000 — not watches_disabled 1034, which would require the option to be live).
func TestSetRYWDisable_PoisonDoesNotApplyOption(t *testing.T) {
	t.Parallel()

	// Poisoning call: option NOT applied.
	tx := newTestTx()
	tx.hadRead.Store(true)
	tx.SetReadYourWritesDisable()
	if e := tx.deferredErr.Load(); e == nil || e.Code != 2000 {
		t.Fatalf("poisoning disable: want deferred 2000, got %v", e)
	}
	if tx.rywDisabled {
		t.Fatal("poisoning disable must NOT apply rywDisabled (C++ throws before assigning)")
	}

	// Clean call: option applied, no poison.
	tx2 := newTestTx()
	tx2.SetReadYourWritesDisable()
	if e := tx2.deferredErr.Load(); e != nil {
		t.Fatalf("clean disable must not poison, got %v", e)
	}
	if !tx2.rywDisabled {
		t.Fatal("clean disable must apply rywDisabled")
	}

	// Clean-then-poisoning: the first (valid) application SURVIVES the second call's
	// poison — C++ applied the option on call 1; call 2 only records the error.
	tx3 := newTestTx()
	tx3.SetReadYourWritesDisable()
	tx3.hadRead.Store(true)
	tx3.SetReadYourWritesDisable()
	if !tx3.rywDisabled {
		t.Fatal("re-disable poison must not UNDO the earlier valid application")
	}
	if e := tx3.deferredErr.Load(); e == nil || e.Code != 2000 {
		t.Fatalf("re-disable after a read must poison, got %v", e)
	}
}

// TestDeferredError_FirstWinsAcrossSources pins the ONE-slot property: C++ has a single
// ISingleThreadTransaction::deferredError shared by every source, and doOnMainThreadVoid
// never overwrites it — so whichever of {bad Atomic, poisoning RYW-disable} happens FIRST
// is what every gate surfaces, in both orders.
func TestDeferredError_FirstWinsAcrossSources(t *testing.T) {
	t.Parallel()

	// Bad Atomic first → 2018 wins; the later poisoning disable is a no-op on the slot.
	tx := newTestTx()
	tx.Atomic(MutClearRange, []byte("k"), []byte("v"))
	tx.hadRead.Store(true)
	tx.SetReadYourWritesDisable()
	if e := tx.deferredErr.Load(); e == nil || e.Code != ErrInvalidMutationType {
		t.Fatalf("bad-Atomic-then-disable: want 2018 (first wins), got %v", e)
	}

	// Poisoning disable first → 2000 wins; the later bad Atomic is a no-op on the slot.
	tx2 := newTestTx()
	tx2.hadRead.Store(true)
	tx2.SetReadYourWritesDisable()
	tx2.Atomic(MutClearRange, []byte("k"), []byte("v"))
	if e := tx2.deferredErr.Load(); e == nil || e.Code != 2000 {
		t.Fatalf("disable-then-bad-Atomic: want 2000 (first wins), got %v", e)
	}
}

// TestWatch_DeferredErrorBeatsWatchesDisabled pins the C++ watch gate order: the
// deferred error is checked at the ThreadSafeTransaction::watch lambda (:654) BEFORE
// RYW::watch's options.readYourWritesDisabled throw (ReadYourWrites.actor.cpp:2448-2449).
// A cleanly-RYW-disabled txn that then records a deferred error (bad Atomic op-code)
// must surface the stored 2018 from Watch — not watches_disabled (1034).
func TestWatch_DeferredErrorBeatsWatchesDisabled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()
	k := []byte(t.Name() + "_k")

	tx := db.CreateTransaction()
	tx.SetReadYourWritesDisable()            // clean (pre-op) → the option applies
	tx.Atomic(MutClearRange, k, []byte("v")) // deferred 2018
	if err := tx.Watch(ctx, k); deferredCodeOf(err) != ErrInvalidMutationType {
		t.Fatalf("Watch on RYW-disabled + poisoned txn: want 2018 (deferred beats 1034), got %v", err)
	}

	// Control: the same disabled txn WITHOUT a deferred error still gets 1034.
	tx2 := db.CreateTransaction()
	tx2.SetReadYourWritesDisable()
	if err := tx2.Watch(ctx, k); deferredCodeOf(err) != 1034 {
		t.Fatalf("Watch on RYW-disabled txn: want watches_disabled (1034), got %v", err)
	}
}
