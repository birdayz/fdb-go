package client

import (
	"context"
	"errors"
	"testing"
	"time"
)

const lifetimeTestTimeout = time.Second

func receiveLifetimeTest[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(lifetimeTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func requireLifetimeTestBlocked[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s completed before its release condition", what)
	default:
	}
}

func TestExecutionLeaseEnterStateWaitsForAtomicReadyPublication(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	finish := tx.startTurnover()

	entered := make(chan *executionLease, 1)
	go func() { entered <- tx.enterState() }()
	requireLifetimeTestBlocked(t, entered, "enterState")

	finish()
	lease := receiveLifetimeTest(t, entered, "enterState after ready publication")
	defer lease.release()
	tx.readErrMu.Lock()
	current, ready := tx.readLife, tx.readReady
	tx.readErrMu.Unlock()
	if ready != nil || lease.inc != current || !lease.active {
		t.Fatalf("admission was not an atomic claim on the published incarnation: ready=%v lease.inc=%p current=%p active=%v", ready != nil, lease.inc, current, lease.active)
	}
}

func TestExecutionLeaseResetDrainsBeforeClearingWrites(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set([]byte("old-key"), []byte("old-value"))
	lease := tx.enterState()
	old := lease.inc

	resetDone := make(chan struct{})
	go func() {
		tx.Reset()
		close(resetDone)
	}()
	receiveLifetimeTest(t, old.ctx.Done(), "old incarnation retirement")
	requireLifetimeTestBlocked(t, resetDone, "Reset while an old lease is active")

	tx.conflictMu.Lock()
	mutationCount := len(tx.mutations)
	tx.conflictMu.Unlock()
	if mutationCount != 1 {
		t.Fatalf("Reset cleared old writes before lease drain: got %d mutations, want 1", mutationCount)
	}
	lease.release()
	receiveLifetimeTest(t, resetDone, "Reset after lease release")
	tx.conflictMu.Lock()
	mutationCount = len(tx.mutations)
	tx.conflictMu.Unlock()
	if mutationCount != 0 {
		t.Fatalf("Reset retained writes after drain: got %d mutations", mutationCount)
	}
}

func TestExecutionLeaseSuccessiveTurnoversRevalidateIdentity(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	first := tx.enterState()
	firstInc := first.inc
	first.release()

	finish, err := tx.beginTurnover(firstInc, first)
	if err != nil {
		t.Fatal(err)
	}
	finish()
	if lease := tx.enterReadState(context.Background(), firstInc, false); lease != nil {
		lease.release()
		t.Fatal("first incarnation was admitted after turnover")
	}

	second := tx.enterState()
	secondInc := second.inc
	if secondInc == firstInc {
		t.Fatal("first turnover retained incarnation identity")
	}
	second.release()
	finish, err = tx.beginTurnover(secondInc, second)
	if err != nil {
		t.Fatal(err)
	}
	finish()
	if lease := tx.enterReadState(context.Background(), secondInc, false); lease != nil {
		lease.release()
		t.Fatal("second incarnation was admitted after successive turnover")
	}
	third := tx.enterState()
	defer third.release()
	if third.inc == firstInc || third.inc == secondInc {
		t.Fatal("successive turnover did not publish a distinct current identity")
	}
}

func TestExecutionLeaseOldReadNeverAdmitsReplacement(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	old := tx.enterState()
	oldInc := old.inc
	old.release()
	finish, err := tx.beginTurnover(oldInc, old)
	if err != nil {
		t.Fatal(err)
	}
	finish()

	if lease := tx.enterReadState(context.Background(), oldInc, true); lease != nil {
		lease.release()
		t.Fatal("old captured read rebound to replacement incarnation")
	}
	current := tx.enterReadState(context.Background(), func() *readIncarnation {
		tx.readErrMu.Lock()
		defer tx.readErrMu.Unlock()
		return tx.readIncarnationLocked()
	}(), false)
	if current == nil {
		t.Fatal("current incarnation was not admitted")
	}
	current.release()
}

func TestExecutionLeaseNestedOpContextBorrowsOuterLease(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	outer, releaseOuter := tx.opContext(context.Background())
	outerOp := tx.readOperation(outer)
	nested, releaseNested := tx.opContext(outer)
	nestedOp := tx.readOperation(nested)
	if nestedOp != outerOp {
		t.Fatal("nested opContext replaced rather than borrowed the outer operation")
	}

	releaseNested()
	tx.readErrMu.Lock()
	users, active := outerOp.inc.users, outerOp.lease.active
	tx.readErrMu.Unlock()
	if users != 1 || !active {
		t.Fatalf("nested cleanup released outer lease: users=%d active=%v", users, active)
	}
	releaseOuter()
	tx.readErrMu.Lock()
	users, active = outerOp.inc.users, outerOp.lease.active
	tx.readErrMu.Unlock()
	if users != 0 || active {
		t.Fatalf("outer cleanup did not release its lease: users=%d active=%v", users, active)
	}
}

func TestExecutionLeaseBeginTurnoverRejectsActiveAndForeignTokens(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	active := tx.enterState()
	if _, err := tx.beginTurnover(active.inc, active); fdbCodeOf(err) != ErrClientInvalidOperation {
		t.Fatalf("active token error code=%d, want %d", fdbCodeOf(err), ErrClientInvalidOperation)
	}
	active.release()

	foreignTx := &Transaction{}
	foreign := foreignTx.enterState()
	defer foreign.release()
	if _, err := tx.beginTurnover(active.inc, foreign); fdbCodeOf(err) != ErrClientInvalidOperation {
		t.Fatalf("foreign token error code=%d, want %d", fdbCodeOf(err), ErrClientInvalidOperation)
	}
}

func TestExecutionLeaseReleasedOldTokenCannotResetNewIncarnation(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	old := tx.enterState()
	oldInc := old.inc
	old.release()
	finish, err := tx.beginTurnover(oldInc, old)
	if err != nil {
		t.Fatal(err)
	}
	finish()

	finish, err = tx.beginTurnover(oldInc, old)
	if finish != nil {
		finish()
		t.Fatal("stale token obtained turnover ownership")
	}
	if fdbCodeOf(err) != ErrTransactionCancelled {
		t.Fatalf("stale token error code=%d, want %d", fdbCodeOf(err), ErrTransactionCancelled)
	}
	current := tx.enterState()
	defer current.release()
	if current.inc == oldInc {
		t.Fatal("stale turnover replaced current identity with the old incarnation")
	}
}

func TestExecutionLeaseCanceledWaitingReadHoldsNoLease(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	finish := tx.startTurnover()
	tx.readErrMu.Lock()
	replacement := tx.readIncarnationLocked()
	tx.readErrMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	admitted := tx.enterReadState(ctx, replacement, true)
	if admitted != nil {
		admitted.release()
		t.Fatal("canceled waiting read acquired a lease")
	}
	tx.readErrMu.Lock()
	users := replacement.users
	tx.readErrMu.Unlock()
	if users != 0 {
		t.Fatalf("canceled waiting read retained state lease: users=%d", users)
	}
	finish()
}

func TestExecutionLeaseGetPipelinedDoesNotReleaseBorrowedLease(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	key := []byte("borrowed-pipeline")
	tx.Set(key, []byte("local"))
	tx.readVersionMu.Lock()
	tx.hasReadVersion = true
	tx.readVersion = 1
	tx.readVersionMu.Unlock()
	outerCtx, outerCleanup := tx.opContext(context.Background())
	defer outerCleanup()
	outer := tx.readOperation(outerCtx)
	value, pending, err := tx.GetPipelined(outerCtx, key)
	if err != nil || pending != nil || string(value) != "local" {
		t.Fatalf("nested pipelined cache hit=(%q, %v, %v)", value, pending, err)
	}
	tx.readErrMu.Lock()
	users, active := outer.inc.users, outer.lease.active
	tx.readErrMu.Unlock()
	if users != 1 || !active {
		t.Fatalf("GetPipelined released borrowed lease: users=%d active=%v", users, active)
	}
}

func TestExecutionLeaseReplacementTimeoutInterruptsActiveRead(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.creationTime = time.Now()
	tx.timeoutNs.Store(int64(50 * time.Millisecond))
	tx.reset(false)
	ctx, cleanup := tx.opContext(context.Background())
	defer cleanup()
	op := tx.readOperation(ctx)
	if op == nil || op.lease == nil {
		t.Fatal("replacement read was not admitted before timeout")
	}
	select {
	case <-op.inc.ctx.Done():
		if code := fdbCodeOf(context.Cause(op.inc.ctx)); code != ErrTransactionTimedOut {
			t.Fatalf("replacement interruption code=%d, want %d", code, ErrTransactionTimedOut)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement timebomb did not interrupt active read")
	}
}

func TestExecutionLeaseEarlyReplacementCaptureArmsTimeout(t *testing.T) {
	t.Parallel()
	for _, userReset := range []bool{false, true} {
		name := "retry"
		if userReset {
			name = "user-reset"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := txWithDB()
			tx.db.mutateDefaults(func(defaults *TransactionDefaults) { defaults.Timeout = 60000 })
			tx.SetTimeout(60000)
			defer tx.Cancel()
			finish := tx.startTurnover()

			// Even a canceled read captures the replacement before waiting for
			// publication. It must not consume the replacement's timeout setup.
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			waiting, cleanup := tx.opContext(parent)
			cleanup()
			captured := tx.readOperation(waiting)
			tx.resetFields(userReset)
			finish()
			if captured == nil || captured.lease != nil {
				t.Fatalf("canceled turnover waiter = %v, want a captured incarnation without a lease", captured)
			}

			ctx, release := tx.opContext(context.Background())
			defer release()
			op := tx.readOperation(ctx)
			tx.readErrMu.Lock()
			timer, generation := op.inc.timer, op.inc.timerGen
			tx.readErrMu.Unlock()
			if op.inc != captured.inc || op.lease == nil {
				t.Fatal("read did not enter the early-captured replacement")
			}
			if timer == nil {
				t.Fatal("published early-captured replacement has no timeout timer")
			}
			// Deliver the armed callback deterministically, without racing an
			// operating-system timer against test scheduling.
			timer.Stop()
			tx.fireReadTimeout(op.inc, generation)
			receiveLifetimeTest(t, ctx.Done(), "replacement timeout delivery")
			if code := fdbCodeOf(context.Cause(ctx)); code != 1031 {
				t.Fatalf("replacement interruption code=%d, want 1031", code)
			}
		})
	}
}

func TestExecutionLeaseOptionsAfterCancellationDoNotReviveRead(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	finish := tx.startTurnover()
	tx.readErrMu.Lock()
	replacement := tx.readIncarnationLocked()
	tx.readErrMu.Unlock()
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, cleanup := tx.opContext(parent)
	defer cleanup()
	if op := tx.readOperation(ctx); op == nil || op.inc != replacement || op.lease != nil {
		t.Fatalf("canceled waiter state: op=%v replacement=%p", op, replacement)
	}
	finish()

	tx.SetRetryLimit(7)
	if !tx.hasRetryLimit || tx.retryLimit != 7 {
		t.Fatalf("option after cancellation was not applied: has=%v limit=%d", tx.hasRetryLimit, tx.retryLimit)
	}
	if err := tx.readEntryError(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("option revived canceled read: got %v, want context.Canceled", err)
	}
	tx.readErrMu.Lock()
	users := replacement.users
	tx.readErrMu.Unlock()
	if users != 0 {
		t.Fatalf("option or cleanup revived canceled admission: users=%d", users)
	}
}
