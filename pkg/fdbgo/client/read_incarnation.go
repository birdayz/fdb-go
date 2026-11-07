package client

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/wire"
)

// readIncarnation is the lifetime of one C++ resetPromise. Its mutable fields
// are protected by Transaction.readErrMu: cancellation delivery and all other
// transaction locks stay outside it (RFC-256); only the leaf locks
// nativeCommit.mu, timeoutQueue.mu, and designTimerEntry.mu (released before
// callbacks take readErrMu) nest inside.
type readIncarnation struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	gen     uint64
	cause   error
	users   int
	drained chan struct{}
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
	// deferred marks an operation whose parent can be cancelled: it ends with
	// the parent or the incarnation, whichever ends first. The context merging
	// the two is derived only when something waits on the operation or asks
	// the context package for its cause: an operation the RYW cache answers
	// only checks Err, as C++ answers it without an actor or a cancellation
	// callback (ReadYourWrites.actor.cpp:378-406). Context is the parent.
	deferred bool
	released atomic.Bool              // a deferred operation's release ran
	merge    atomic.Pointer[opMerged] // a deferred operation's merged context, once derived
}

// opMerged is the context merging a deferred operation's parent and incarnation.
type opMerged struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	stop   func() bool
}

func (m *opMerged) end() {
	m.stop()
	m.cancel(nil)
}

// merged derives the context a cancellable parent got eagerly before: a child
// of the parent that the incarnation's end cancels with its cause. Derived after
// release, it is already cancelled, as the eager one was by then.
func (op *readOperation) merged() context.Context {
	if m := op.merge.Load(); m != nil {
		return m.ctx
	}
	ctx, cancel := context.WithCancelCause(op.Context)
	inc := op.inc
	m := &opMerged{ctx: ctx, cancel: cancel}
	m.stop = context.AfterFunc(inc.ctx, func() { cancel(context.Cause(inc.ctx)) })
	if inc.ctx.Err() != nil {
		cancel(context.Cause(inc.ctx))
	}
	if !op.merge.CompareAndSwap(nil, m) {
		m.end()
		return op.merge.Load().ctx
	}
	// Release sets released before it loads merge: one of the two ends it.
	if op.released.Load() {
		m.end()
	}
	return ctx
}

// endDeferred ends a deferred operation: its merged context, if derived, and its lease.
func (op *readOperation) endDeferred() {
	op.released.Store(true)
	if m := op.merge.Load(); m != nil {
		m.end()
	}
	op.lease.release()
}

// derivedMerge is the merged context if derived, else nil.
func (op *readOperation) derivedMerge() context.Context {
	if m := op.merge.Load(); m != nil {
		return m.ctx
	}
	return nil
}

func (op *readOperation) Done() <-chan struct{} {
	if op.joined {
		return op.inc.ctx.Done()
	}
	if op.deferred {
		return op.merged().Done()
	}
	return op.Context.Done()
}

func (op *readOperation) Err() error {
	if op.joined {
		return op.inc.ctx.Err()
	}
	if op.deferred {
		if ctx := op.derivedMerge(); ctx != nil {
			return ctx.Err()
		}
		// What the merged context reports once either end has reached it.
		if err := op.Context.Err(); err != nil {
			return err
		}
		return op.inc.ctx.Err()
	}
	return op.Context.Err()
}

// cancelCtxKey is the key context.Cause looks its cancellation up by.
var cancelCtxKey = func() any {
	var p causeKeyProbe
	_ = context.Cause(&p)
	return p.key
}()

type causeKeyProbe struct {
	context.Context
	key any
}

func (p *causeKeyProbe) Value(key any) any { p.key = key; return nil }
func (p *causeKeyProbe) Err() error        { return nil }

func (op *readOperation) Value(key any) any {
	if k, ok := key.(readContextKey); ok && k.tx == op.tx {
		return op
	}
	if op.deferred {
		// Caller values are the parent's either way; the cancellation the
		// context package looks up (Cause, children) is the merged one's.
		if ctx := op.derivedMerge(); ctx != nil || key == cancelCtxKey {
			return op.merged().Value(key)
		}
		return op.Context.Value(key)
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
	}
	return tx.readLife
}

// recordCauseLocked linearizes failure before delivering it. A cause recorded
// into the current incarnation also retires its generation's pending timeout;
// a retired, captured incarnation leaves its successor's timeout alone.
func (tx *Transaction) recordCauseLocked(inc *readIncarnation, cause error) {
	if inc.cause != nil {
		return
	}
	inc.cause = cause
	if inc == tx.readLife {
		tx.disarmTimeoutLocked()
	}
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

func (tx *Transaction) failCapturedIncarnation(inc *readIncarnation, cause error) {
	tx.readErrMu.Lock()
	tx.recordCauseLocked(inc, cause)
	pending := tx.detachPendingLocked(inc)
	cause = inc.cause
	tx.readErrMu.Unlock()
	if tx.beforeReadFailureDelivery != nil {
		tx.beforeReadFailureDelivery(cause)
	}
	inc.cancel(cause)
	retirePending(pending, false)
}

// configureReadTimeout replaces the generation's pending timeout, not the
// deadlines of individual operations, so clearing or changing TIMEOUT reaches
// reads already waiting. A pending timeout is replaced without a cause, as C++
// replaces its timebomb actor (RYW:2570-2573); only delivery, or arming an
// already-expired deadline, records 1031, and only reset clears it.
func (tx *Transaction) configureReadTimeout() {
	tx.readErrMu.Lock()
	// Turnover arms the replacement only after reset-owned options and deadlines
	// are final (turnoverToken.finish).
	if tx.readReady != nil {
		tx.readErrMu.Unlock()
		return
	}
	tx.armTimeoutLocked()
	inc := tx.readLife
	var cause error
	var pending []*PendingGet
	if inc != nil && inc.cause != nil {
		cause = inc.cause
		pending = tx.detachPendingLocked(inc)
	}
	tx.readErrMu.Unlock()
	if cause != nil {
		inc.cancel(cause)
		retirePending(pending, false)
	}
}

// armTimeoutLocked arms TIMEOUT for the current read generation, whether or
// not its incarnation exists yet (C++ resetTimeout, RYW:1576-1579). An already
// expired deadline records 1031 synchronously, as the timebomb actor does
// before its first wait (RYW:1567-1573). The caller delivers a recorded cause.
func (tx *Transaction) armTimeoutLocked() {
	tx.timeoutNeedsArm = false
	if tx.readReady != nil {
		return
	}
	if tx.timeoutNs.Load() <= 0 {
		tx.disarmTimeoutLocked()
		return
	}
	if inc := tx.readLife; inc != nil && inc.cause != nil {
		return
	}
	q := tx.timeoutQueue()
	// Convert before acquiring the queue lock: contention must not extend the budget.
	delay := time.Until(tx.deadlineTime())
	deadline := q.now() + int64(delay)
	if delay <= 0 {
		inc := tx.readLife
		if inc == nil {
			inc = tx.readIncarnationLocked()
		}
		tx.recordCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})
		return
	}
	tx.timeoutArm++
	tx.timeoutArmed = true
	if tx.designArmLocked(q, delay) {
		return
	}
	n := tx.timeoutNode
	if n == nil {
		n = q.newNode(tx)
		tx.timeoutNode = n
	} else if weak.Make(tx) != n.tx {
		// The caller is strong: a different weak identity means resurrection.
		oldQueue := n.queue
		oldQueue.retire(n)
		n = oldQueue.newNodeForShard(tx)
		tx.timeoutNode = n
	}
	n.queue.schedule(n, deadline)
}

// disarmTimeoutLocked cancels the generation's pending timeout. It leaves
// timeoutArm alone: a delivery popped earlier is rejected by !timeoutArmed, or
// by the new arm's number once something re-arms.
func (tx *Transaction) disarmTimeoutLocked() {
	tx.timeoutArmed = false
	tx.timeoutNeedsArm = false
	tx.stopDesignTimerLocked()
}

func (tx *Transaction) timeoutQueue() *timeoutQueue {
	if tx.db != nil && tx.db.timeouts != nil {
		return tx.db.timeouts
	}
	return detachedTimeouts
}

// fireTimeout delivers the timeout armed as arm. Claim and record are one
// critical section, so a turnover or re-arm after the queue popped the node
// makes this a no-op. A generation without an incarnation gets one.
func (tx *Transaction) fireTimeout(arm uint64) {
	tx.readErrMu.Lock()
	if !tx.timeoutArmed || tx.timeoutArm != arm {
		tx.readErrMu.Unlock()
		return
	}
	tx.timeoutArmed = false
	inc := tx.readLife
	if inc == nil {
		inc = tx.readIncarnationLocked()
	}
	tx.recordCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})
	pending := tx.detachPendingLocked(inc)
	cause := inc.cause
	tx.readErrMu.Unlock()
	if tx.afterTimeoutRecord != nil {
		tx.afterTimeoutRecord()
	}
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
	return ctx, release.cancelFunc(), owned != nil && issued
}

// opRelease ends an admitted operation: op, when set, is a deferred operation
// whose merged context and lease end together; otherwise lease alone (nil when
// borrowed) is released. A value, so releasing allocates no closure.
type opRelease struct {
	op    *readOperation
	lease *executionLease
}

func (r opRelease) release() {
	if r.op != nil {
		r.op.endDeferred()
		return
	}
	r.lease.release()
}

func (r opRelease) cancelFunc() context.CancelFunc {
	switch {
	case r.op != nil:
		return r.op.endDeferred
	case r.lease != nil:
		return r.lease.release
	default:
		return func() {}
	}
}

// afterLease is the cleanup left for a holder that outlives the already
// released lease: only a derived context remains to end.
func (r opRelease) afterLease() context.CancelFunc {
	if r.op != nil {
		return r.op.endDeferred
	}
	return func() {}
}

// opContextWithLease returns release authority only to the admitting frame.
// Borrowers retain identity and effective cancellation, but own no token.
func (tx *Transaction) opContextWithLease(parent context.Context, issued bool) (context.Context, opRelease, *executionLease) {
	return tx.opContextInto(parent, issued, nil)
}

// opContextInto is opContextWithLease admitting a fresh operation into into, when
// set, instead of a new one: a pooled read answered at issue allocates none.
func (tx *Transaction) opContextInto(parent context.Context, issued bool, into *readOperation) (context.Context, opRelease, *executionLease) {
	if op := tx.readOperation(parent); op != nil {
		if op.lease.isActive() {
			return parent, opRelease{}, nil
		}
		nested := &readOperation{Context: parent, readAdmission: op.readAdmission}
		nested.lease = tx.enterReadStateInto(parent, op.inc, false, issued, &nested.leaseStorage)
		if nested.lease == nil {
			return parent, opRelease{}, nil
		}
		return nested, opRelease{lease: nested.lease}, nested.lease
	}
	tx.readErrMu.Lock()
	inc := tx.readIncarnationLocked()
	if tx.timeoutNeedsArm && tx.readReady == nil {
		tx.armTimeoutLocked()
	}
	cause := inc.cause
	tx.readErrMu.Unlock()
	if parent.Done() == nil {
		// Nothing but the incarnation can end this operation, so it uses the
		// incarnation's cancellation directly instead of deriving a context
		// that merges the two.
		if cause != nil {
			inc.cancel(cause)
		}
		op := freshOperation(into)
		*op = readOperation{Context: parent, readAdmission: readAdmission{tx: tx, inc: inc, parent: parent}, joined: true}
		op.lease = tx.enterReadStateInto(op, inc, true, issued, &op.leaseStorage)
		if op.lease != nil {
			op.entryErr = tx.deferredErr.Load()
		}
		return op, opRelease{lease: op.lease}, op.lease
	}
	if cause != nil {
		inc.cancel(cause)
	}
	// The merged context is derived when first needed (see deferred).
	op := freshOperation(into)
	*op = readOperation{Context: parent, readAdmission: readAdmission{tx: tx, inc: inc, parent: parent}, deferred: true}
	op.lease = tx.enterReadStateInto(op, inc, true, issued, &op.leaseStorage)
	if op.lease != nil {
		// The lease pins this incarnation's slot across reset. Preserve even a
		// clean entry through nested work; later poison gates later operations.
		op.entryErr = tx.deferredErr.Load()
	}
	return op, opRelease{op: op}, op.lease
}

func freshOperation(into *readOperation) *readOperation {
	if into != nil {
		return into
	}
	return new(readOperation)
}

func (tx *Transaction) readOperation(ctx context.Context) *readOperation {
	if op, ok := ctx.(*readOperation); ok && op.tx == tx {
		return op
	}
	if ctx.Done() == nil {
		return nil
	}
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

// addReadConflictsFor publishes only into the issuing incarnation. conflictMu
// spans the lifetime check and append: a later retirement must clear these
// conflicts before publishing the replacement incarnation. readErrMu stays a leaf.
func (tx *Transaction) addReadConflictsFor(ctx context.Context, ranges [][2][]byte) error {
	if hook := tx.beforeReadConflict; hook != nil {
		hook()
	}
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if err := tx.admitReadConflictLocked(ctx); err != nil {
		return err
	}
	tx.addReadConflictsLocked(ranges)
	return nil
}

// addReadConflictSpansFor is addReadConflictsFor for a read's filtered conflict spans.
func (tx *Transaction) addReadConflictSpansFor(ctx context.Context, spans []conflictSpan) error {
	if hook := tx.beforeReadConflict; hook != nil {
		hook()
	}
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if err := tx.admitReadConflictLocked(ctx); err != nil {
		return err
	}
	tx.addReadConflictSpansLocked(spans)
	return nil
}

// admitReadConflictLocked reports why a read's conflict may not join the
// transaction: its incarnation ended. Caller holds conflictMu.
func (tx *Transaction) admitReadConflictLocked(ctx context.Context) error {
	if ctx != nil {
		if op := tx.readOperation(ctx); op != nil {
			tx.readErrMu.Lock()
			cause := op.inc.cause
			if cause == nil && tx.readLife != op.inc {
				cause = &wire.FDBError{Code: ErrTransactionCancelled}
			}
			tx.readErrMu.Unlock()
			if cause != nil {
				return cause
			}
		}
	}
	return nil
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
	if err == nil {
		return nil
	}
	err = tx.mapReadError(ctx, err)
	if op := tx.readOperation(ctx); op != nil {
		return tx.trackReadErrorGen(err, op.inc.gen)
	}
	return tx.trackReadError(err)
}
