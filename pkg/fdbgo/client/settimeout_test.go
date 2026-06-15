package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestSetTimeout_BoundsHungRead is the RFC-112 gold test: a read whose reply is
// permanently dropped, under SetTimeout(300ms), returns transaction_timed_out
// (1031) in ≈300ms — NOT the ~maxReadTimeoutRetries×readRPCTimeout (~50s) the old
// re-send loop would burn. Deterministic by construction: the reply is dropped, so
// the SetTimeout deadline always wins (no timer-vs-reply race — the RFC-288 lesson).
//
// Revert-proof: remove opContext (and the loop checkTimeout) and the read runs to
// the re-send cap and returns transaction_too_old after ~50s instead of 1031.
func TestSetTimeout_BoundsHungRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, dd := newDropReplyTestDB(t, ctx)

	key := []byte(t.Name() + "_key")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Warm the location cache and pre-fetch a read version so the only RPC during
	// the fault window is the getValue read itself.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	dd.armAll() // every read reply is now dropped

	tx := db.CreateTransaction()
	tx.readVersion = rv
	tx.hasReadVersion = true
	tx.SetTimeout(300) // ms

	start := time.Now()
	_, err = tx.Get(ctx, key)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Get returned nil error for a dropped reply under SetTimeout")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Fatalf("err = %v (%T), want transaction_timed_out (%d)", err, err, ErrTransactionTimedOut)
	}
	// Generous upper bound: 300ms timeout + scheduling slack, but FAR below the
	// ~50s the un-fixed re-send loop would take (and below one 5s readRPCTimeout,
	// proving opContext cancels the in-flight wait rather than waiting it out).
	if elapsed > 3*time.Second {
		t.Fatalf("Get took %v — SetTimeout(300ms) did not bound the in-flight read", elapsed)
	}
	// transaction_timed_out must be terminal, not retried by Transact/OnError.
	if onErrorRetryable(ErrTransactionTimedOut) {
		t.Fatal("transaction_timed_out must be non-retryable")
	}
}

// TestMapTimeout pins the error-mapping contract (RFC-112): our SetTimeout deadline
// → 1031; the caller's own cancellation → preserved; no timeout set → preserved.
func TestMapTimeout(t *testing.T) {
	t.Parallel()
	live := context.Background()
	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		timeout   time.Duration
		deadline  time.Time
		parent    context.Context
		in        error
		wantTimed bool // expect transaction_timed_out
		wantSame  bool // expect the input error unchanged
	}{
		{"our-deadline-fired", time.Second, time.Now().Add(-time.Millisecond), live, context.DeadlineExceeded, true, false},
		{"our-cancel-fired", time.Second, time.Now().Add(-time.Millisecond), live, context.Canceled, true, false},
		{"caller-ctx-done", time.Second, time.Now().Add(-time.Millisecond), doneCtx, context.DeadlineExceeded, false, true},
		{"no-timeout-set", 0, time.Time{}, live, context.DeadlineExceeded, false, true},
		{"deadline-not-reached", time.Hour, time.Now().Add(time.Hour), live, context.DeadlineExceeded, false, true},
		{"nil-error", time.Second, time.Now().Add(-time.Millisecond), live, nil, false, true},
		{"unrelated-error", time.Second, time.Now().Add(-time.Millisecond), live, errReplyTimeout, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{timeout: tc.timeout, deadline: tc.deadline}
			got := tx.mapTimeout(tc.parent, tc.in)
			var fdbErr *wire.FDBError
			isTimed := errors.As(got, &fdbErr) && fdbErr.Code == ErrTransactionTimedOut
			if isTimed != tc.wantTimed {
				t.Fatalf("mapTimeout timed-out=%v, want %v (got %v)", isTimed, tc.wantTimed, got)
			}
			if tc.wantSame && !errors.Is(got, tc.in) {
				t.Fatalf("mapTimeout = %v, want unchanged %v", got, tc.in)
			}
		})
	}
}

// TestOpContext proves opContext bounds the ctx by the deadline only when a timeout
// is set, and otherwise returns the ctx unchanged.
func TestOpContext(t *testing.T) {
	t.Parallel()
	// No timeout → same ctx, no deadline added.
	txNo := &Transaction{}
	ctx, cancel := txNo.opContext(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("opContext added a deadline with no timeout set")
	}
	// Timeout → ctx carries the transaction deadline.
	want := time.Now().Add(time.Hour)
	txYes := &Transaction{timeout: time.Hour, deadline: want}
	ctx2, cancel2 := txYes.opContext(context.Background())
	defer cancel2()
	dl, ok := ctx2.Deadline()
	if !ok || !dl.Equal(want) {
		t.Fatalf("opContext deadline = %v (ok=%v), want %v", dl, ok, want)
	}
}
