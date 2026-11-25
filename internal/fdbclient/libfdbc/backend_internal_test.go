//go:build cgo && libfdbc

package libfdbc

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// TestErrorShim_RetryRecognizedAndContextPreserved pins the two-way error
// bridge: an fdb.Error the record layer propagated up — wrapped with
// %w context — must (1) still be recognized as a cgofdb.Error by cgofdb's
// retryable() loop so the retryable code is delegated to libfdb_c's OnError, and
// (2) on a terminal failure, round-trip back through convErr with its raw code AND
// its wrapped context intact (the pure-Go backend keeps that context; this backend
// must too).
func TestErrorShim_RetryRecognizedAndContextPreserved(t *testing.T) {
	t.Parallel()

	// Record layer returns a wrapped retryable fdb error (1020 = not_committed).
	orig := fmt.Errorf("save record xyz: %w", fdb.Error{Code: 1020})

	// On the way OUT of the Transact callback.
	shimmed := toCgoErr(orig)

	// cgofdb's retryable() does exactly this to decide retry + call OnError.
	var ce cgofdb.Error
	if !errors.As(shimmed, &ce) {
		t.Fatal("cgofdb.retryable() must still see a cgofdb.Error (else no retry delegation)")
	}
	if ce.Code != 1020 {
		t.Fatalf("cgofdb.Error.Code = %d, want 1020", ce.Code)
	}

	// Terminal failure: the libfdbc database boundary maps it back. Context AND code
	// must both survive.
	back := convErr(shimmed)
	if back.Error() != orig.Error() {
		t.Fatalf("context lost across round-trip:\n got=%q\nwant=%q", back.Error(), orig.Error())
	}
	var fe fdb.Error
	if !errors.As(back, &fe) || fe.Code != 1020 {
		t.Fatalf("fdb.Error code lost across round-trip: %v", back)
	}
}

// TestErrorShim_NonFDBPassThrough confirms a record-layer semantic error (not an
// fdb.Error) is NOT shimmed — it passes through both directions unchanged, so
// cgofdb treats it as terminal (no spurious retry) and the caller sees it verbatim.
func TestErrorShim_NonFDBPassThrough(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("record already exists")
	out := toCgoErr(sentinel)
	if out != sentinel {
		t.Fatalf("non-fdb error must pass through toCgoErr unchanged, got %v", out)
	}
	var ce cgofdb.Error
	if errors.As(out, &ce) {
		t.Fatal("a non-fdb error must NOT look like a cgofdb.Error (would trigger a bogus retry)")
	}
	if got := convErr(out); got != sentinel {
		t.Fatalf("non-fdb error must pass through convErr unchanged, got %v", got)
	}
}

// TestConvErr_PlainCgoError maps a genuine cgofdb.Error (as produced inside the
// callback by a future's Get) to an fdb.Error with the same raw code.
func TestConvErr_PlainCgoError(t *testing.T) {
	t.Parallel()

	out := convErr(cgofdb.Error{Code: 1007})
	var fe fdb.Error
	if !errors.As(out, &fe) || fe.Code != 1007 {
		t.Fatalf("convErr(cgofdb.Error{1007}) = %v, want fdb.Error{1007}", out)
	}
}

// TestWithCancelWatcher_NoCancelAfterCallbackReturns pins the commit-detachment
// contract: once the callback returns successfully, the cancel
// watcher must NEVER cancel the transaction, even if the ctx is canceled immediately
// afterward. Otherwise cgofdb's auto-commit — which runs after the callback returns —
// could be aborted with transaction_cancelled, which the pure-Go backend never does
// (it detaches the commit via context.WithoutCancel, client/transaction.go:1437).
//
// The race is inherent: close(stop) signals the watcher but a concurrently-firing
// ctx.Done() could still win a naive select. The loop makes the regression
// deterministic. The fixed watcher (stop-priority re-check + join) records ZERO
// cancels across every iteration because the join guarantees the goroutine has exited
// before withCancelWatcher returns — so the later cancel() can never reach cancelTx.
// Revert withCancelWatcher to the plain-select/no-join form and the goroutine is still
// live when cancel() fires, both select cases are ready, and ~half the iterations call
// cancelTx — the assertion fails.
func TestWithCancelWatcher_NoCancelAfterCallbackReturns(t *testing.T) {
	t.Parallel()

	const iters = 2000
	var canceled atomic.Int64
	for i := 0; i < iters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		// The callback returns successfully with the ctx still live — in the real path
		// cgofdb's auto-commit would run now.
		if _, err := withCancelWatcher(ctx, func() { canceled.Add(1) }, func() (any, error) {
			return nil, nil
		}); err != nil {
			cancel()
			t.Fatalf("iter %d: withCancelWatcher returned err %v", i, err)
		}
		// ctx fires AFTER the callback returned and the watcher was joined: too late to
		// cancel anything.
		cancel()
	}
	if n := canceled.Load(); n != 0 {
		t.Fatalf("watcher canceled the transaction %d/%d times AFTER the callback returned "+
			"successfully — an in-flight auto-commit would be aborted (pure-Go detaches it)", n, iters)
	}
}

// TestWithCancelWatcher_CancelsDuringBlockedCallback proves the stop-priority fix did
// NOT break the legit cancel path: when the ctx fires while the callback is still
// running (a read stuck on a C future), the watcher must call cancelTx — the cgo analog
// of unblocking a ctx-bound read RPC. The callback blocks until cancelTx releases it,
// so the test deadlocks (caught by the test timeout) if the watcher fails to cancel.
func TestWithCancelWatcher_CancelsDuringBlockedCallback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	canceledCh := make(chan struct{}, 1)
	release := make(chan struct{})
	go cancel() // cancel the ctx while fn is (about to be) blocked

	_, err := withCancelWatcher(ctx, func() { canceledCh <- struct{}{}; close(release) }, func() (any, error) {
		<-release // unblocked only by the watcher's cancelTx
		return nil, ctx.Err()
	})
	if err == nil {
		t.Fatal("expected the canceled ctx to surface as the callback error")
	}
	select {
	case <-canceledCh:
	default:
		t.Fatal("watcher did not cancel the transaction while the callback was blocked on the ctx")
	}
}

// TestWithCancelWatcher_ReturnsFDBErrorPanic proves an fdb.Error panic from the callback
// (e.g. an adapter MustGet on a conflict) is recovered into the RETURNED error with its
// code intact — runLoop then classifies it (a retryable code goes to OnError). It also
// exercises the join on the panic path (close(stop) + <-exited run before the recover, no
// leak, no deadlock).
func TestWithCancelWatcher_ReturnsFDBErrorPanic(t *testing.T) {
	t.Parallel()

	_, e := withCancelWatcher(context.Background(), func() {}, func() (any, error) {
		panic(fdb.Error{Code: 1020})
	})
	var fe fdb.Error
	if !errors.As(e, &fe) || fe.Code != 1020 {
		t.Fatalf("an fdb.Error panic must be returned as fdb.Error{1020}, got %v", e)
	}
}

// TestWithCancelWatcher_ReturnsNonFDBErrorPanic proves a non-fdb ERROR panic is converted
// to the RETURNED error, not re-panicked. cgofdb's panicToError recovers only cgofdb.Error
// and re-throws anything else, so re-panicking a non-fdb error here would escape Transact
// and crash the process; the pure-Go backend recovers the full error interface
// (fdb/transaction.go:506) and returns it. Revert the defer to `panic(p)` for non-fdb
// errors and this returns e==nil (the panic escapes) — red.
func TestWithCancelWatcher_ReturnsNonFDBErrorPanic(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	r, e := withCancelWatcher(context.Background(), func() {}, func() (any, error) {
		panic(sentinel)
	})
	if !errors.Is(e, sentinel) {
		t.Fatalf("a non-fdb error panic must be returned as the error, got r=%v e=%v", r, e)
	}
}

// TestCtxBoundedWait_AbortsOnCtxCancel proves the ctx-bounded backoff wait (what runLoop
// uses for tr.OnError) is interrupted by ctx. get() here blocks until the cancel callback
// releases it — modeling a libfdb_c OnError future that only errors once the transaction is
// canceled. Without the ctx.Done() arm (the P2-A bug: cgofdb's retryable() backoff has no
// ctx), ctxBoundedWait would block forever; with it, ctx cancel → cancel() → get() returns
// → ctx.Err(). Revert ctxBoundedWait to a plain `return convErr(get())` and this deadlocks
// (caught by the test timeout).
func TestCtxBoundedWait_AbortsOnCtxCancel(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	get := func() error {
		<-released                      // blocks until "the transaction is canceled"
		return cgofdb.Error{Code: 1025} // transaction_cancelled — what a canceled future returns
	}
	cancel := func() { close(released) }
	ctx, ctxCancel := context.WithCancel(context.Background())
	go ctxCancel() // ctx fires while get() is blocked

	if err := ctxBoundedWait(ctx, get, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctxBoundedWait must return ctx.Err() when ctx fires during the wait, got %v", err)
	}
}

// TestCtxBoundedWait_ReturnsResult proves that when the future completes before ctx fires,
// ctxBoundedWait returns its convErr'd result (nil for a retryable reset; the fdb error
// OnError re-raised otherwise) and does NOT invoke cancel.
func TestCtxBoundedWait_ReturnsResult(t *testing.T) {
	t.Parallel()

	canceled := false
	noCancel := func() { canceled = true }
	if err := ctxBoundedWait(context.Background(), func() error { return nil }, noCancel); err != nil {
		t.Fatalf("retryable reset must return nil, got %v", err)
	}
	err := ctxBoundedWait(context.Background(), func() error { return cgofdb.Error{Code: 1007} }, noCancel)
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1007 {
		t.Fatalf("a terminal OnError must return the convErr'd fdb.Error{1007}, got %v", err)
	}
	if canceled {
		t.Fatal("cancel must not be called when the future completes first")
	}
}

// TestWithCancelWatcher_RepanicsNonErrorPanic proves a non-ERROR panic (e.g. a string)
// still re-panics unchanged — matching both Apple's binding and the pure-Go panicToError,
// which only recover the error interface and re-throw everything else.
func TestWithCancelWatcher_RepanicsNonErrorPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if p := recover(); p != "boom-string" {
			t.Fatalf("a non-error panic must re-panic unchanged, got %v", p)
		}
	}()
	_, _ = withCancelWatcher(context.Background(), func() {}, func() (any, error) {
		panic("boom-string")
	})
}
