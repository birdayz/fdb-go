package client

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// Borrowed contexts are internal metadata, not public observer inputs. A retry
// root without ownership must preserve the existing verdict ordering but cannot
// retire or poison the operation from which it borrowed.
func TestOnErrorBorrowedAdmissionIsReadOnly(t *testing.T) {
	t.Parallel()
	appErr := errors.New("application error")
	for _, tc := range []struct {
		name     string
		input    error
		setup    func(*Transaction, *readOperation)
		cancel   bool
		want     error
		wantCode int
	}{
		{name: "retry", input: &wire.FDBError{Code: 1020}, wantCode: 2000},
		{name: "app", input: appErr, want: appErr},
		{name: "deferred-is-not-onerror-gate", input: &wire.FDBError{Code: 1020}, setup: func(tx *Transaction, _ *readOperation) { tx.deferredErr.Store(&wire.FDBError{Code: 2006}) }, wantCode: 2000},
		{name: "cancelled-inc-before-app", input: appErr, setup: func(tx *Transaction, op *readOperation) {
			tx.failCapturedIncarnation(op.inc, &wire.FDBError{Code: 1025})
		}, wantCode: 1025},
		{name: "timeout-canceled-incoming-context-live-original-parent", input: &wire.FDBError{Code: 1020}, setup: func(tx *Transaction, op *readOperation) {
			tx.failCapturedIncarnation(op.inc, &wire.FDBError{Code: 1031})
		}, want: context.Canceled},
		{name: "app-before-timeout", input: appErr, setup: func(tx *Transaction, op *readOperation) {
			tx.failCapturedIncarnation(op.inc, &wire.FDBError{Code: 1031})
		}, want: appErr},
		{name: "actual-incoming-caller-at-timeout", input: &wire.FDBError{Code: 1020}, setup: func(tx *Transaction, op *readOperation) {
			tx.failCapturedIncarnation(op.inc, &wire.FDBError{Code: 1031})
		}, cancel: true, want: context.Canceled},
		{name: "elapsed-undelivered-timeout-is-no-cause", input: &wire.FDBError{Code: 1020}, setup: func(tx *Transaction, _ *readOperation) {
			// Deliberately no SetTimeout: an elapsed deadline without a
			// delivered timeout is no cause (C++ C4), so this is a retry.
			tx.timeoutNs.Store(int64(time.Second))
			tx.deadlineNs.Store(time.Now().Add(-time.Hour).UnixNano())
		}, wantCode: 2000},
		{name: "input-timeout-before-caller", input: &wire.FDBError{Code: 1031}, cancel: true, wantCode: 1031},
		{name: "retry-limit-before-caller", input: &wire.FDBError{Code: 1020}, setup: func(tx *Transaction, _ *readOperation) { tx.hasRetryLimit = true; tx.retryLimit = 0 }, cancel: true, wantCode: 1020},
		{name: "nonretryable-before-caller", input: &wire.FDBError{Code: 2004}, cancel: true, wantCode: 2004},
		{name: "eligible-incoming-caller", input: &wire.FDBError{Code: 1020}, cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logCalls := 0
			tx := &Transaction{db: &database{logger: slog.New(&scopeObserver{enabled: func(context.Context) bool { logCalls++; return false }})}}
			tx.backoffJitter = func() float64 { return 0 }
			defer tx.Cancel()
			outer, release := tx.opContext(context.Background())
			defer release()
			op := tx.readOperation(outer)
			watch, stopWatch := tx.newWatchCtx(context.Background())
			defer stopWatch()
			if tc.setup != nil {
				tc.setup(tx, op)
			}
			if tx.readIncarnationCause(op.inc) != nil {
				<-outer.Done() // incoming legacy cancellation is distinct from op.parent
				if op.parent.Err() != nil {
					t.Fatal("test's original public parent must stay live")
				}
			}
			caller, cancel := context.WithCancel(outer)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			state, retries, backoff := tx.state.Load(), tx.retryCount, tx.backoff
			cause := tx.readIncarnationCause(op.inc)
			tx.readErrMu.Lock()
			armed := tx.timeoutArmed
			tx.readErrMu.Unlock()
			if armed {
				t.Fatal("borrowed test unexpectedly armed an ambient timebomb")
			}
			err := tx.OnError(caller, tc.input)
			if logCalls != 0 || tx.db.metrics.transactionRetries.Load() != 0 {
				t.Errorf("borrowed misuse emitted retry effects: log=%d metrics=%d", logCalls, tx.db.metrics.transactionRetries.Load())
			}
			if tc.wantCode != 0 {
				requireScopeCode(t, err, tc.wantCode)
			} else if err != tc.want {
				t.Errorf("OnError=%v, want original %v", err, tc.want)
			}
			tx.readErrMu.Lock()
			active, users, current, completion := op.lease.active.Load(), op.inc.users, tx.readLife, op.inc.versionstamp
			afterCause := op.inc.cause
			tx.readErrMu.Unlock()
			if !active || users != 1 || current != op.inc || completion != nil || afterCause != cause || tx.state.Load() != state || tx.retryCount != retries || tx.backoff != backoff || watch.Err() != nil {
				t.Errorf("BORROWED_ONERROR_MUTATION: active=%v users=%d current=%v completion=%v cause=%v->%v state=%d->%d retries=%d->%d backoff=%v->%v watch=%v", active, users, current == op.inc, completion, cause, afterCause, state, tx.state.Load(), retries, tx.retryCount, backoff, tx.backoff, watch.Err())
			}
		})
	}
}

func TestCommitBorrowedAdmissionKeepsOwnerAndProducer(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"healthy", "deferred", "cancelled", "caller"} {
		t.Run(mode, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			if mode == "deferred" {
				tx.deferredErr.Store(&wire.FDBError{Code: 2006})
			}
			outer, release := tx.opContext(context.Background())
			defer release()
			op := tx.readOperation(outer)
			caller, cancel := context.WithCancel(outer)
			defer cancel()
			want := 2000
			switch mode {
			case "deferred":
				want = 2006
			case "cancelled":
				tx.failCapturedIncarnation(op.inc, &wire.FDBError{Code: 1025})
				want = 1025
			case "caller":
				cancel()
			}
			err := tx.Commit(caller)
			// Existing active Commit entry does not generally check incoming
			// caller Err: even a canceled borrow reaches the authority guard.
			requireScopeCode(t, err, want)
			tx.readErrMu.Lock()
			active, users, current, completion := op.lease.active.Load(), op.inc.users, tx.readLife, op.inc.versionstamp
			tx.readErrMu.Unlock()
			if !active || users != 1 || current != op.inc || completion != nil {
				t.Errorf("BORROWED_COMMIT_MUTATION: active=%v users=%d current=%v completion=%v", active, users, current == op.inc, completion)
			}
		})
	}
}
