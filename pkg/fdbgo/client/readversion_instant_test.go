package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// The read-version instant answers "when did FDB's 5-second MVCC window open for
// the version this transaction is reading at". A layer above budgets against that
// window, so the field's RESET discipline is the whole contract: it must track the
// current read version and nothing else.
//
// It is deliberately NOT an accessor onto metricStart. metricStart survives OnError
// so RFC-114's total latency spans retries; this must not, because OnError takes a
// NEW read version and opens a NEW window. The tests below pin both disciplines in
// the same assertions, so collapsing the two fields into one cannot pass.

// TestReadVersionInstant_UnsetBeforeAnyRead pins that a transaction which has not
// read reports no instant. This is the case the naive "anchor at transaction start"
// proxy gets wrong: BeginTx alone opens no MVCC window, so there is nothing to
// budget against yet, and reporting a zero time as if it were an anchor would start
// a 5-second clock that the cluster has not started.
func TestReadVersionInstant_UnsetBeforeAnyRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("ReadVersionInstant ok=true before any read; a transaction with no read version has no MVCC window to report")
	}
}

// TestReadVersionInstant_StampedAtGRV pins that the first read stamps the instant,
// and that the stamp lands inside the window the test itself brackets.
func TestReadVersionInstant_StampedAtGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	before := time.Now()
	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get: %v", err)
	}
	after := time.Now()

	inst, ok := tx.ReadVersionInstant()
	if !ok {
		t.Fatal("ReadVersionInstant ok=false after a read that took a read version")
	}
	if inst.Before(before) || inst.After(after) {
		t.Fatalf("instant %v outside the bracketed GRV window [%v, %v]", inst, before, after)
	}
}

// TestReadVersionInstant_ClearedAndRestampedOnOnError is the load-bearing one: it
// pins the instant's OPPOSITE-to-metricStart discipline in a single run.
//
// Revert-proof in both directions. Drop the clear in reset() and the post-OnError
// instant stays equal to the pre-OnError one, failing the "advanced" assertion.
// Make it an accessor onto metricStart and the same assertion fails, because
// metricStart is preserved across OnError by design. Conversely, adding a clear of
// metricStart to reset() reddens the RFC-114 tests that pin its spanning.
func TestReadVersionInstant_ClearedAndRestampedOnOnError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get 1: %v", err)
	}
	first, ok := tx.ReadVersionInstant()
	if !ok {
		t.Fatal("ReadVersionInstant ok=false after the first read")
	}
	tx.readVersionMu.Lock()
	metricStartBefore := tx.metricStart
	tx.readVersionMu.Unlock()
	if metricStartBefore.IsZero() {
		t.Fatal("metricStart unset after the first GRV; the RFC-114 anchor is not being stamped and this test cannot compare the two disciplines")
	}

	// A retryable error drives the same reset() path the Run loop uses.
	if err := tx.OnError(ctx, &wire.FDBError{Code: ErrNotCommitted}); err != nil {
		t.Fatalf("OnError(1020): %v", err)
	}

	// Cleared: the retry has not taken its new read version yet, so there is no
	// window to report. A budget that kept counting here would charge the retry
	// for the previous attempt's time.
	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("ReadVersionInstant ok=true immediately after OnError; the instant must be cleared with the read version it described")
	}

	// metricStart, by contrast, is PRESERVED — the documented RFC-114 divergence.
	// This assertion is what makes the two fields provably distinct.
	tx.readVersionMu.Lock()
	metricStartAfter := tx.metricStart
	tx.readVersionMu.Unlock()
	if !metricStartAfter.Equal(metricStartBefore) {
		t.Fatalf("metricStart moved across OnError (%v → %v); it must span retries (RFC-114). The read-version instant must NOT share this field.",
			metricStartBefore, metricStartAfter)
	}

	// Re-stamped at the retry's GRV, and strictly later than the first: the new
	// window opened later than the old one.
	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get 2: %v", err)
	}
	second, ok := tx.ReadVersionInstant()
	if !ok {
		t.Fatal("ReadVersionInstant ok=false after the retry's read; the instant is stamped at EVERY GRV, not only the first")
	}
	if !second.After(first) {
		t.Fatalf("instant did not advance across OnError (first=%v, second=%v); a per-attempt anchor must track the retry's own GRV",
			first, second)
	}
}

// TestReadVersionInstant_UnsetForUserSuppliedReadVersion pins the honest not-ok
// answer for SetReadVersion. That version's window opened on the cluster at an
// instant this client never observed, so reporting the local call time would claim
// a full window for a version that may be nearly expired.
func TestReadVersionInstant_UnsetForUserSuppliedReadVersion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Take a real version to hand back, via a throwaway transaction.
	src := db.CreateTransaction()
	rv, err := src.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("GetReadVersion: %v", err)
	}
	src.Cancel()

	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetReadVersion(rv)

	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("ReadVersionInstant ok=true for a user-supplied read version; the client never saw when that version's window opened")
	}

	// And a read does not invent one: the version is already set, so no GRV fires.
	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("ReadVersionInstant ok=true after reading at a user-supplied read version; no GRV happened, so no instant exists")
	}
}

// NativeAPI rejects SetReadVersion after a GRV. A rejected setter must not
// overwrite either the cached version or its original observation instant.
func TestReadVersionInstant_RejectedUserVersionPreservesStamp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	// A real GRV first, so the field holds a genuine stamp.
	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get: %v", err)
	}
	before, ok := tx.ReadVersionInstant()
	if !ok {
		t.Fatal("ReadVersionInstant ok=false after a read")
	}

	// The version is already set, even when supplying the same value.
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("GetReadVersion: %v", err)
	}
	tx.SetReadVersion(rv)

	if _, err := tx.GetReadVersion(ctx); fdbCodeOf(err) != 2010 {
		t.Fatalf("rejected setter error=%v, want 2010", err)
	}
	if inst, ok := tx.ReadVersionInstant(); !ok || !inst.Equal(before) {
		t.Fatalf("rejected setter changed instant: got %v,%v want %v,true", inst, ok, before)
	}
}

// TestReadVersionInstant_ClearedOnCommitReuse pins the reuse boundary: a handle
// reused after a successful commit begins a new logical transaction whose window
// has not opened yet.
func TestReadVersionInstant_ClearedOnCommitReuse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.Set([]byte(t.Name()), []byte("v"))
	if value, err := tx.Get(ctx, []byte(t.Name())); err != nil || string(value) != "v" {
		t.Fatalf("local Get=%q, %v; want v", value, err)
	}
	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("a local RYW read opened the MVCC window")
	}
	if rv, err := tx.GetReadVersion(ctx); err != nil || rv <= 0 {
		t.Fatalf("GetReadVersion=%d, %v; want a positive read version", rv, err)
	}
	if inst, ok := tx.ReadVersionInstant(); !ok || inst.IsZero() {
		t.Fatal("explicit GRV did not stamp the MVCC window before commit")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, ok := tx.ReadVersionInstant(); ok {
		t.Fatal("ReadVersionInstant ok=true after commit-reuse reset; the next transaction on this handle has not opened its window yet")
	}
}
