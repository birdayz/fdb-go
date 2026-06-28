package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestSetTimeout_BoundsHungRead is the RFC-112 gold test: a read whose reply is
// permanently dropped, under SetTimeout(300ms), returns transaction_timed_out
// (1031) in ≈300ms — NOT the ~maxReadTimeoutRetries×readRPCTimeout (~50s) the old
// re-send loop would burn. Deterministic by construction: the reply is dropped, so
// the SetTimeout deadline always wins (no timer-vs-reply race — the RFC-288 lesson).
//
// Revert-proof: remove opContext and the read runs to the re-send cap and returns
// transaction_too_old after ~maxReadTimeoutRetries×readRPCTimeout instead of 1031.
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
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
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

// TestSetTimeout_BoundsHungGRV is the sibling of the read test for the GRV — the
// FIRST read RPC every transaction issues. With the reply dropped BEFORE a read
// version is fetched, SetTimeout(300ms) must bound the in-flight GRV (inside
// ensureReadVersion) and return transaction_timed_out (1031), not run to the GRV
// loop's ctx-only bound. Port of C++ RYWImpl::getReadVersion's
// `choose { getReadVersion() | resetPromise }` (ReadYourWrites.actor.cpp:1537).
// Revert-proof: without opContext in ensureReadVersion the GRV ignores SetTimeout.
func TestSetTimeout_BoundsHungGRV(t *testing.T) {
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
	// Warm a full Get so the GRV-proxy connection is dialed and pooled — armAll
	// only arms connections that already exist, and this test must drop the GRV
	// reply (not just the storage read) to exercise the ensureReadVersion bound.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	dd.armAll() // every reply (incl. the GRV) is now dropped

	tx := db.CreateTransaction()
	tx.SetTimeout(300) // ms; no read version pre-set → ensureReadVersion must fetch one

	start := time.Now()
	_, err := tx.Get(ctx, key)
	elapsed := time.Since(start)

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Fatalf("err = %v (%T), want transaction_timed_out (%d)", err, err, ErrTransactionTimedOut)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Get took %v — SetTimeout(300ms) did not bound the in-flight GRV", elapsed)
	}
}

// TestSetTimeout_BoundsHungPipelinedRead covers the pipelined read path
// (GetPipelined/PendingGet.Resolve) — what the fdb facade Get routes through. A
// dropped reply under SetTimeout(300ms) must return transaction_timed_out (1031)
// in well under one readRPCTimeout (5s), proving the deferred reply wait is capped
// by the deadline (RFC-112), not the fixed 5s pipeline timer. Revert-proof:
// without pipelineReplyTimeout the Resolve waits the full 5s before re-driving.
func TestSetTimeout_BoundsHungPipelinedRead(t *testing.T) {
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
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	dd.armAll()

	tx := db.CreateTransaction()
	tx.readVersion = rv
	tx.hasReadVersion = true
	tx.SetTimeout(300) // ms

	start := time.Now()
	_, pending, err := tx.GetPipelined(ctx, key)
	if err == nil && pending != nil {
		_, err = pending.Resolve()
	}
	elapsed := time.Since(start)

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Fatalf("err = %v (%T), want transaction_timed_out (%d)", err, err, ErrTransactionTimedOut)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("pipelined Get took %v — SetTimeout(300ms) did not bound the deferred reply wait", elapsed)
	}
}

// TestSetTimeout_BoundsHungLocate covers the cache-MISS locate (GetKeyServerLocations)
// on the pipelined path — the first RPC of the send phase. With the location cache
// invalidated and the locate reply dropped, SetTimeout(300ms) must bound it and
// return transaction_timed_out (1031), not run a full 5s per proxy. Revert-proof:
// if the locate uses the bare caller ctx (not opCtx) the locate ignores SetTimeout.
func TestSetTimeout_BoundsHungLocate(t *testing.T) {
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
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	}); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	db.db.locCache.invalidate(key, NoTenantID) // force a cache-miss locate
	dd.armAll()

	tx := db.CreateTransaction()
	tx.readVersion = rv
	tx.hasReadVersion = true
	tx.SetTimeout(300) // ms

	start := time.Now()
	_, pending, err := tx.GetPipelined(ctx, key)
	if err == nil && pending != nil {
		_, err = pending.Resolve()
	}
	elapsed := time.Since(start)

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Fatalf("err = %v (%T), want transaction_timed_out (%d)", err, err, ErrTransactionTimedOut)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cache-miss locate took %v — SetTimeout(300ms) did not bound it", elapsed)
	}
}

// TestSetTimeout_BoundsHungMetrics covers GetEstimatedRangeSizeBytes and
// GetRangeSplitPoints — C++ wraps both in waitOrError(op, resetPromise)
// (ReadYourWrites.actor.cpp:1863/1879). With the locate reply dropped under
// SetTimeout(300ms) each must return transaction_timed_out (1031) in <3s, not run
// to the 5s-per-proxy retry cap. Revert-proof: without opContext on these entry
// points they ignore SetTimeout (run to all_alternatives_failed/transaction_too_old).
func TestSetTimeout_BoundsHungMetrics(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, dd := newDropReplyTestDB(t, ctx)

	begin := []byte(t.Name() + "_a")
	end := []byte(t.Name() + "_z")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(begin, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		_, _, e := tx.GetRange(ctx, begin, end, 10)
		return nil, e
	}); err != nil {
		t.Fatalf("warm: %v", err)
	}

	check := func(name string, run func(tx *Transaction) error) {
		db.db.locCache.invalidate(begin, NoTenantID)
		dd.armAll()
		tx := db.CreateTransaction()
		tx.SetTimeout(300)
		start := time.Now()
		err := run(tx)
		elapsed := time.Since(start)
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
			t.Fatalf("%s: err = %v (%T), want transaction_timed_out (%d)", name, err, err, ErrTransactionTimedOut)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("%s took %v — SetTimeout(300ms) did not bound it", name, elapsed)
		}
	}
	check("GetEstimatedRangeSizeBytes", func(tx *Transaction) error {
		_, e := tx.GetEstimatedRangeSizeBytes(ctx, begin, end)
		return e
	})
	check("GetRangeSplitPoints", func(tx *Transaction) error {
		_, e := tx.GetRangeSplitPoints(ctx, begin, end, 1000)
		return e
	})
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
