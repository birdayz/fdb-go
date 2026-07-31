package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// The package doc at fdb.dev/pkg/fdbgo makes one central factual claim to migrators:
// a default transaction is UNBOUNDED (no internal timeout, no internal max-retry,
// matching libfdb_c's timeoutInSeconds=0.0 / maxRetries=-1 at
// ReadYourWrites.actor.cpp:2078-2082), and ONLY an explicit bound the caller chooses
// terminates it. These tests pin that behaviour rather than the prose, so the doc
// cannot silently drift away from the code.
//
// An internal cap sneaking into OnError would make the doc a lie in the dangerous
// direction (a migrator who read "unbounded" and set a bound would be fine, but one
// who reads a future "we cap at N" and drops their bound would not). Each of the
// three bounds the doc names gets its own case, because a fix that honours only one
// of them still leaves the doc wrong about the other two.

// pinTx builds a minimal active transaction with a deterministic zero backoff, so the
// retry paths run instantly instead of sleeping.
func pinTx() *Transaction {
	tx := &Transaction{db: &database{}}
	tx.state.Store(int32(txStateActive))
	tx.backoffJitter = func() float64 { return 0 }
	return tx
}

// retryableErr is not_committed (1020) — the ordinary conflict every retry loop sees.
func retryableErr() error { return &wire.FDBError{Code: ErrNotCommitted} }

// TestDefaultTransactionIsUnbounded pins the doc's core claim: with no option set,
// OnError keeps granting retries no matter how many have already happened. There is
// no internal max-retry, exactly as in libfdb_c (maxRetries=-1).
func TestDefaultTransactionIsUnbounded(t *testing.T) {
	t.Parallel()

	tx := pinTx()
	if tx.hasRetryLimit {
		t.Fatalf("a default transaction must carry no retry limit, got retryLimit=%d", tx.retryLimit)
	}
	if got := tx.timeoutNs.Load(); got != 0 {
		t.Fatalf("a default transaction must carry no timeout, got timeoutNs=%d", got)
	}

	// A retry count far past any plausible internal cap. If OnError refuses here, an
	// internal bound has appeared and the package doc's "unbounded" is false.
	tx.retryCount = 1_000_000
	if err := tx.OnError(context.Background(), retryableErr()); err != nil {
		t.Fatalf("default (unbounded) transaction stopped retrying at retryCount=%d: %v\n"+
			"An internal retry cap has been introduced. libfdb_c has none "+
			"(maxRetries=-1, ReadYourWrites.actor.cpp:2078-2082) and the package doc at "+
			"fdb.dev/pkg/fdbgo tells migrators this client has none either. "+
			"Update the doc, key.go and fdb/database.go together with this change.",
			tx.retryCount, err)
	}
}

// TestExplicitRetryLimitBounds pins that SetRetryLimit — one of the three bounds the
// package doc tells callers to choose from — actually terminates the retry loop.
func TestExplicitRetryLimitBounds(t *testing.T) {
	t.Parallel()

	tx := pinTx()
	tx.SetRetryLimit(3)
	tx.retryCount = 3 // at the limit

	err := tx.OnError(context.Background(), retryableErr())
	if err == nil {
		t.Fatal("SetRetryLimit(3) at retryCount=3 must STOP the retry loop, but OnError granted another retry.\n" +
			"The package doc at fdb.dev/pkg/fdbgo names SetRetryLimit as a bound a caller may rely on.")
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != ErrNotCommitted {
		t.Fatalf("a retry-limit stop must surface the most recent error (1020), got %v", err)
	}
}

// TestExplicitTimeoutBounds pins that SetTimeout — the direct analog of C++ `timebomb`
// (ReadYourWrites.actor.cpp:1576-1578) — terminates the retry loop with 1031, and that
// it installs a real deadline on operation contexts rather than only gating at op
// entry. The opContext half is what makes the timeout bound an in-FLIGHT RPC (RFC-112);
// without it the doc's claim that SetTimeout is a usable substitute for a ctx deadline
// would be false for a hung read.
//
// Measured while mutation-checking this pin: the timeout bound on the retry loop is
// defence-in-depth with TWO independent enforcement points — the OnError entry gate
// (checkTimeout) and backoffSleepBounded, which cuts a backoff that would cross the
// deadline and raises 1031 itself. Removing either one alone leaves the documented
// behaviour intact and this test correctly green; the claim only breaks when both go.
// That is deliberate on the code's side (the entry gate exists so a contended txn does
// not overshoot by one full backoff before the next op notices), and it means anyone
// re-testing this pin by deleting a single guard should expect green, not red.
func TestExplicitTimeoutBounds(t *testing.T) {
	t.Parallel()

	t.Run("no timeout leaves the op context unbounded", func(t *testing.T) {
		t.Parallel()
		tx := pinTx()
		ctx, cancel := tx.opContext(context.Background())
		defer cancel()
		if dl, ok := ctx.Deadline(); ok {
			t.Fatalf("a default transaction must put NO deadline on its operations, got %v.\n"+
				"The package doc at fdb.dev/pkg/fdbgo tells migrators the default is unbounded.", dl)
		}
	})

	t.Run("timeout deadlines the op context", func(t *testing.T) {
		t.Parallel()
		tx := pinTx()
		tx.SetTimeout(60_000) // 60s
		ctx, cancel := tx.opContext(context.Background())
		defer cancel()
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("SetTimeout must bound in-flight operations with a real deadline (the C++ timebomb analog, RFC-112);\n" +
				"without it SetTimeout gates only at op boundaries and cannot stop a hung RPC.")
		}
	})

	t.Run("expired timeout stops the retry loop with 1031", func(t *testing.T) {
		t.Parallel()
		tx := pinTx()
		tx.SetTimeout(1) // 1ms
		tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())

		err := tx.OnError(context.Background(), retryableErr())
		if err == nil {
			t.Fatal("an expired SetTimeout must STOP the retry loop, but OnError granted another retry.")
		}
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrTransactionTimedOut {
			t.Fatalf("an expired SetTimeout must surface transaction_timed_out (1031), matching C++ timebomb; got %v", err)
		}
	})
}

// TestExplicitContextBounds pins the third bound — Go's EXTRA one (RFC-090). A done
// caller ctx stops the retry loop, and it out-ranks the transaction timeout so a
// TransactCtx caller gets their own cancellation rather than a synthesized 1031.
func TestExplicitContextBounds(t *testing.T) {
	t.Parallel()

	t.Run("cancelled ctx stops the retry loop", func(t *testing.T) {
		t.Parallel()
		tx := pinTx()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := tx.OnError(ctx, retryableErr())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled ctx must stop the retry loop with context.Canceled, got %v.\n"+
				"The package doc at fdb.dev/pkg/fdbgo names TransactCtx as a bound a caller may rely on.", err)
		}
	})

	t.Run("caller ctx out-ranks the transaction timeout", func(t *testing.T) {
		t.Parallel()
		tx := pinTx()
		tx.SetTimeout(1)
		tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := tx.OnError(ctx, retryableErr())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("with BOTH the caller ctx and the txn timeout expired, the caller's own "+
				"cancellation must win over a synthesized 1031, got %v", err)
		}
	})
}
