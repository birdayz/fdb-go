package client

import (
	"context"
	"errors"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// readIncarnation is the lifetime of one C++ resetPromise. Its mutable fields
// are protected by Transaction.readErrMu, a leaf lock: cancellation delivery
// and all other transaction locks must stay outside it (RFC-256).
type readIncarnation struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	gen      uint64
	cause    error
	timer    *time.Timer
	timerGen uint64
	users    int
	drained  chan struct{}

	versionstamp  *versionstampCompletion
	versionstamps map[*versionstampCompletion]struct{}
}

type readContextKey struct{ tx *Transaction }

type readOperation struct {
	inc      *readIncarnation
	parent   context.Context
	lease    *executionLease
	entryErr *wire.FDBError
}

func (tx *Transaction) readIncarnationLocked() *readIncarnation {
	if tx.readLife == nil {
		ctx, cancel := context.WithCancelCause(context.Background())
		tx.readLife = &readIncarnation{ctx: ctx, cancel: cancel, gen: tx.readGen}
		tx.armReadTimeoutLocked(tx.readLife)
	}
	return tx.readLife
}

// recordReadCauseLocked linearizes failure before delivering it. In particular,
// SetTimeout cannot retire a callback after that callback has recorded failure.
func recordReadCauseLocked(inc *readIncarnation, cause error) {
	if inc.cause == nil {
		inc.cause = cause
		inc.timerGen++
		for completion := range inc.versionstamps {
			completion.selectLocked(commitOutcome{}, cause, true)
		}
	}
}

func (tx *Transaction) failCapturedIncarnation(inc *readIncarnation, cause error) {
	tx.readErrMu.Lock()
	recordReadCauseLocked(inc, cause)
	pending := tx.detachPendingLocked(inc)
	cause, timer := inc.cause, inc.timer
	inc.timer = nil
	tx.readErrMu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if tx.beforeReadFailureDelivery != nil {
		tx.beforeReadFailureDelivery(cause)
	}
	inc.cancel(cause)
	retirePending(pending, false)
}

// configureReadTimeout replaces the incarnation's timebomb, not the deadlines
// of individual operations. Clearing/changing TIMEOUT therefore reaches reads
// already waiting. Once the timebomb fires, only reset can clear its cause.
func (tx *Transaction) configureReadTimeout() {
	tx.readErrMu.Lock()
	// Turnover arms the replacement only after reset-owned options and deadlines
	// are final. Creating it here would leave an unarmed incarnation behind the
	// not-ready barrier.
	if tx.readReady != nil {
		tx.readErrMu.Unlock()
		return
	}
	if tx.readLife == nil && tx.timeoutNs.Load() <= 0 {
		tx.readErrMu.Unlock()
		return
	}
	inc := tx.readIncarnationLocked()
	old := inc.timer
	inc.timer = nil
	tx.armReadTimeoutLocked(inc)
	cause := inc.cause
	var pending []*PendingGet
	if cause != nil {
		pending = tx.detachPendingLocked(inc)
	}
	tx.readErrMu.Unlock()
	if old != nil {
		old.Stop()
	}
	if cause != nil {
		inc.cancel(cause)
		retirePending(pending, false)
	}
}

func (tx *Transaction) armReadTimeoutLocked(inc *readIncarnation) {
	if tx.readReady != nil {
		return
	}
	inc.timerGen++
	gen := inc.timerGen
	if inc.cause == nil && tx.timeoutNs.Load() > 0 {
		delay := time.Until(tx.deadlineTime())
		if delay <= 0 {
			recordReadCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})
		} else {
			inc.timer = time.AfterFunc(delay, func() { tx.fireReadTimeout(inc, gen) })
		}
	}
}

func (tx *Transaction) fireReadTimeout(inc *readIncarnation, gen uint64) {
	tx.readErrMu.Lock()
	if tx.readLife != inc || inc.timerGen != gen || inc.cause != nil {
		tx.readErrMu.Unlock()
		return
	}
	recordReadCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})
	cause := inc.cause
	inc.timer = nil
	pending := tx.detachPendingLocked(inc)
	tx.readErrMu.Unlock()
	inc.cancel(cause)
	retirePending(pending, false)
}

// opContext captures once, above GRV and all nested reads/retries. Internal
// calls carrying the same operation retain its OLD incarnation even after a
// reset. Cleanup belongs to the outer operation, or to PendingGet.Resolve.
func (tx *Transaction) opContext(parent context.Context) (context.Context, context.CancelFunc) {
	key := readContextKey{tx}
	if op := tx.readOperation(parent); op != nil {
		tx.readErrMu.Lock()
		borrow := op.lease != nil && op.lease.active
		tx.readErrMu.Unlock()
		if borrow {
			return parent, func() {}
		}
		lease := tx.enterReadState(parent, op.inc, false)
		if lease == nil {
			return parent, func() {}
		}
		ctx := context.WithValue(parent, key, &readOperation{inc: op.inc, parent: op.parent, lease: lease, entryErr: op.entryErr})
		return ctx, lease.release
	}
	tx.readErrMu.Lock()
	inc := tx.readIncarnationLocked()
	tx.readErrMu.Unlock()
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(inc.ctx, func() { cancel(context.Cause(inc.ctx)) })
	tx.readErrMu.Lock()
	cause := inc.cause
	tx.readErrMu.Unlock()
	if cause != nil {
		inc.cancel(cause)
		cancel(cause)
	}
	lease := tx.enterReadState(ctx, inc, true)
	var entryErr *wire.FDBError
	if lease != nil {
		// The lease pins this incarnation's slot across reset. Preserve even a
		// clean entry through nested work; later poison gates later operations.
		entryErr = tx.deferredErr.Load()
	}
	ctx = context.WithValue(ctx, key, &readOperation{inc: inc, parent: parent, lease: lease, entryErr: entryErr})
	return ctx, func() {
		stop()
		cancel(nil)
		lease.release()
	}
}

func (tx *Transaction) readOperation(ctx context.Context) *readOperation {
	op, _ := ctx.Value(readContextKey{tx}).(*readOperation)
	return op
}

func (tx *Transaction) readIncarnationCause(inc *readIncarnation) error {
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	return inc.cause
}

// readEntryError preserves the deferred verdict captured at admission. C++ checks
// deferredError before dispatching to RYW and its resetPromise; nested work does
// not re-enter that wrapper or sample a later void-operation error.
func (tx *Transaction) readEntryError(ctx context.Context) error {
	if op := tx.readOperation(ctx); op != nil && op.entryErr != nil {
		return op.entryErr
	}
	return tx.readLifetimeError(ctx)
}

// readLifetimeError excludes deferred admission failure. OnError has no C++
// deferred gate. Unlike completion mapping, an already-failed incarnation wins
// over an already-canceled caller at entry.
func (tx *Transaction) readLifetimeError(ctx context.Context) error {
	// Failure is published before cancellation is delivered. Observe the context
	// first so a delivered cancellation cannot follow a stale clean cause read.
	ctxErr := ctx.Err()
	if op := tx.readOperation(ctx); op != nil {
		tx.readErrMu.Lock()
		cause := op.inc.cause
		tx.readErrMu.Unlock()
		if cause != nil {
			return cause
		}
	}
	return ctxErr
}

// mapReadError changes only interruption errors. Completed protocol/transport
// errors and successful outcomes are immutable; current transaction state and
// timeout options cannot classify an old operation after reset.
func (tx *Transaction) mapReadError(ctx context.Context, err error) error {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if op := tx.readOperation(ctx); op != nil {
		if parentErr := op.parent.Err(); parentErr != nil {
			return parentErr
		}
		tx.readErrMu.Lock()
		cause := op.inc.cause
		tx.readErrMu.Unlock()
		if cause != nil {
			return cause
		}
	}
	return err
}

func (tx *Transaction) trackReadOperation(ctx context.Context, err error) error {
	err = tx.mapReadError(ctx, err)
	if op := tx.readOperation(ctx); op != nil {
		return tx.trackReadErrorGen(err, op.inc.gen)
	}
	return tx.trackReadError(err)
}
