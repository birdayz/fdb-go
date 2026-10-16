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
	// issuedReads counts issued reads not yet complete; Commit waits for zero
	// (issuedReadsDone closes then), as C++ commit waits on `reading`.
	issuedReads     int
	issuedReadsDone chan struct{}

	// Keep latest inline; overflow strongly retains displaced, unready producers.
	versionstamp  *versionstampCompletion
	versionstamps map[*versionstampCompletion]struct{}
	// Keep the usual single native commit inline; further ones overflow.
	nativeCommit  *nativeCommit
	nativeCommits map[*nativeCommit]struct{}
}

func (inc *readIncarnation) addNativeLocked(n *nativeCommit) {
	if inc.nativeCommit == nil {
		inc.nativeCommit = n
		return
	}
	if inc.nativeCommits == nil {
		inc.nativeCommits = make(map[*nativeCommit]struct{})
	}
	inc.nativeCommits[n] = struct{}{}
}

func (inc *readIncarnation) removeNativeLocked(n *nativeCommit) {
	if inc.nativeCommit == n {
		inc.nativeCommit = nil
		return
	}
	delete(inc.nativeCommits, n)
}

type readContextKey struct{ tx *Transaction }

// readAdmission is immutable after admission and may be copied by nested work.
// The incarnation's generation is immutable too; identity never grants release
// authority over the separate execution token.
type readAdmission struct {
	tx       *Transaction
	inc      *readIncarnation
	parent   context.Context
	entryErr *wire.FDBError
}

type readOperation struct {
	context.Context // retain the effective legacy cancellation, not just parent
	readAdmission
	lease        *executionLease
	leaseStorage executionLease
	// joined marks an operation whose parent can never be cancelled
	// (Done() == nil, as with context.Background): the incarnation's
	// cancellation is the operation's, so it needs no derived context.
	// Context is then the parent itself. Releasing such an operation cancels
	// nothing: a context derived from it ends with its own cancel or with the
	// incarnation, not with the operation.
	joined bool
}

func (op *readOperation) Done() <-chan struct{} {
	if op.joined {
		return op.inc.ctx.Done()
	}
	return op.Context.Done()
}

func (op *readOperation) Err() error {
	if op.joined {
		return op.inc.ctx.Err()
	}
	return op.Context.Err()
}

func (op *readOperation) Value(key any) any {
	if k, ok := key.(readContextKey); ok && k.tx == op.tx {
		return op
	}
	v := op.Context.Value(key)
	if v == nil && op.joined {
		// The incarnation holds no caller values; it answers only the context
		// package's own lookup, so context.Cause and contexts derived from the
		// operation follow the incarnation.
		return op.inc.ctx.Value(key)
	}
	return v
}

func (tx *Transaction) readIncarnationLocked() *readIncarnation {
	if tx.readVersionState == nil {
		tx.readVersionState = &readVersionState{}
	}
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
		if inc.versionstamp != nil {
			inc.versionstamp.selectLocked(commitOutcome{}, cause, true)
		}
		for completion := range inc.versionstamps {
			completion.selectLocked(commitOutcome{}, cause, true)
		}
		if inc.nativeCommit != nil {
			inc.nativeCommit.selectResult(cause, false, nil)
		}
		for native := range inc.nativeCommits {
			native.selectResult(cause, false, nil)
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
	ctx, release, _ := tx.opContextFor(parent, false)
	return ctx, release
}

// opContextFor with issued counts the new lease as an issued read Commit waits on;
// counted reports whether it did (a borrowed or refused lease counts nothing).
func (tx *Transaction) opContextFor(parent context.Context, issued bool) (context.Context, context.CancelFunc, bool) {
	ctx, release, owned := tx.opContextWithLease(parent, issued)
	return ctx, release, owned != nil && issued
}

// opContextWithLease returns release authority only to the admitting frame.
// Borrowers retain identity and effective cancellation, but own no token.
func (tx *Transaction) opContextWithLease(parent context.Context, issued bool) (context.Context, context.CancelFunc, *executionLease) {
	if op := tx.readOperation(parent); op != nil {
		if op.lease.isActive() {
			return parent, func() {}, nil
		}
		nested := &readOperation{Context: parent, readAdmission: op.readAdmission}
		nested.lease = tx.enterReadStateInto(parent, op.inc, false, issued, &nested.leaseStorage)
		if nested.lease == nil {
			return parent, func() {}, nil
		}
		return nested, nested.lease.release, nested.lease
	}
	tx.readErrMu.Lock()
	inc := tx.readIncarnationLocked()
	cause := inc.cause
	tx.readErrMu.Unlock()
	if parent.Done() == nil {
		// Nothing but the incarnation can end this operation, so it uses the
		// incarnation's cancellation directly instead of deriving a context
		// that merges the two.
		if cause != nil {
			inc.cancel(cause)
		}
		op := &readOperation{Context: parent, readAdmission: readAdmission{tx: tx, inc: inc, parent: parent}, joined: true}
		op.lease = tx.enterReadStateInto(op, inc, true, issued, &op.leaseStorage)
		if op.lease != nil {
			op.entryErr = tx.deferredErr.Load()
		}
		return op, op.lease.release, op.lease
	}
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(inc.ctx, func() { cancel(context.Cause(inc.ctx)) })
	tx.readErrMu.Lock()
	cause = inc.cause
	tx.readErrMu.Unlock()
	if cause != nil {
		inc.cancel(cause)
		cancel(cause)
	}
	op := &readOperation{Context: ctx, readAdmission: readAdmission{tx: tx, inc: inc, parent: parent}}
	op.lease = tx.enterReadStateInto(ctx, inc, true, issued, &op.leaseStorage)
	if op.lease != nil {
		// The lease pins this incarnation's slot across reset. Preserve even a
		// clean entry through nested work; later poison gates later operations.
		op.entryErr = tx.deferredErr.Load()
	}
	owned := op.lease
	return op, func() {
		stop()
		cancel(nil)
		owned.release()
	}, owned
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
