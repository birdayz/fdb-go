package client

import (
	"context"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/wire"
)

// unobservedReadReady marks turnover without waiters. It stays open forever;
// only admission waiters replace it with a private publication channel.
var unobservedReadReady = make(chan struct{})

// executionLease protects reset-owned state, not wire I/O ownership. Its identity
// is immutable; readErrMu serializes accounting, while borrowers sample active.
// A published token is never copied or reactivated.
type executionLease struct {
	tx     *Transaction
	inc    *readIncarnation
	active atomic.Bool
	issued bool // counted in inc.issuedReads, which Commit waits on
}

func (l *executionLease) isActive() bool {
	return l != nil && l.active.Load()
}

func (l *executionLease) release() {
	if l == nil {
		return
	}
	l.tx.readErrMu.Lock()
	var issuedDone chan struct{}
	if l.active.Load() {
		if l.issued {
			l.inc.issuedReads--
			if l.inc.issuedReads == 0 {
				issuedDone, l.inc.issuedReadsDone = l.inc.issuedReadsDone, nil
			}
		}
		l.inc.users--
		var drained chan struct{}
		if l.inc.users == 0 {
			drained, l.inc.drained = l.inc.drained, nil
		}
		// Publish retirement before a drain notification can wake turnover.
		l.active.Store(false)
		if drained != nil {
			close(drained)
		}
	}
	l.tx.readErrMu.Unlock()
	if issuedDone != nil {
		close(issuedDone)
	}
}

// enterState admits synchronous, context-free state access; the lease protects the
// whole mutable-state phase. Keep it inlinable (TestStateLeaseDoesNotAllocate).
func (tx *Transaction) enterState() *executionLease {
	lease := &executionLease{tx: tx}
	tx.admitState(lease)
	return lease
}

// admitState initializes a fresh, unpublished lease after waiting out turnover.
func (tx *Transaction) admitState(lease *executionLease) {
	for {
		tx.readErrMu.Lock()
		ready := tx.readReady
		if ready == nil {
			inc := tx.readIncarnationLocked()
			var cause error
			if tx.timeoutNeedsArm {
				tx.armTimeoutLocked()
				cause = inc.cause
			}
			inc.users++
			lease.inc = inc
			lease.active.Store(true)
			tx.readErrMu.Unlock()
			if cause != nil {
				inc.cancel(cause)
			}
			return
		}
		ready = tx.observeReadReadyLocked()
		tx.readErrMu.Unlock()
		<-ready
	}
}

// observeReadReadyLocked registers a waiter while readErrMu holds publication.
func (tx *Transaction) observeReadReadyLocked() chan struct{} {
	if tx.readReady == unobservedReadReady {
		tx.readReady = make(chan struct{})
	}
	return tx.readReady
}

// enterReadState never rebinds a captured operation to a replacement. A pending
// resolver passes wait=false: it may only claim its own active incarnation.
func (tx *Transaction) enterReadState(ctx context.Context, inc *readIncarnation, wait bool) *executionLease {
	return tx.enterReadStateFor(ctx, inc, wait, false)
}

// enterReadStateFor with issued also counts the lease as an issued read, under the same lock.
func (tx *Transaction) enterReadStateFor(ctx context.Context, inc *readIncarnation, wait, issued bool) *executionLease {
	return tx.enterReadStateInto(ctx, inc, wait, issued, nil)
}

func (tx *Transaction) enterReadStateInto(ctx context.Context, inc *readIncarnation, wait, issued bool, lease *executionLease) *executionLease {
	for {
		tx.readErrMu.Lock()
		if tx.readLife != inc {
			tx.readErrMu.Unlock()
			return nil
		}
		ready := tx.readReady
		if ready == nil {
			var cause error
			if tx.timeoutNeedsArm {
				tx.armTimeoutLocked()
				cause = inc.cause
			}
			inc.users++
			if issued {
				inc.issuedReads++
			}
			if lease == nil {
				lease = &executionLease{}
			}
			lease.tx, lease.inc, lease.issued = tx, inc, issued
			lease.active.Store(true)
			tx.readErrMu.Unlock()
			if cause != nil {
				inc.cancel(cause)
			}
			return lease
		}
		if wait {
			ready = tx.observeReadReadyLocked()
		}
		tx.readErrMu.Unlock()
		if !wait {
			return nil
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil
		}
	}
}

func (tx *Transaction) hasPendingReadsLocked() bool {
	return tx.pendingReadFirst != nil || len(tx.pendingReads) > 0
}

func (tx *Transaction) pendingReadsCountLocked() int {
	n := len(tx.pendingReads)
	if tx.pendingReadFirst != nil {
		n++
	}
	return n
}

func (tx *Transaction) addPendingReadLocked(p *PendingGet) {
	if tx.pendingReadFirst == nil && len(tx.pendingReads) == 0 {
		tx.pendingReadFirst = p
		return
	}
	if tx.pendingReads == nil {
		tx.pendingReads = make(map[*PendingGet]struct{}, 4)
	}
	if tx.pendingReadFirst != nil {
		tx.pendingReads[tx.pendingReadFirst] = struct{}{}
		tx.pendingReadFirst = nil
	}
	tx.pendingReads[p] = struct{}{}
}

func (tx *Transaction) removePendingReadLocked(p *PendingGet) {
	if tx.pendingReadFirst == p {
		tx.pendingReadFirst = nil
		return
	}
	delete(tx.pendingReads, p)
}

func (tx *Transaction) detachPendingLocked(inc *readIncarnation) []*PendingGet {
	if tx.readLife != inc {
		return nil
	}
	n := tx.pendingReadsCountLocked()
	if n == 0 {
		return nil
	}
	pending := make([]*PendingGet, 0, n)
	if tx.pendingReadFirst != nil {
		pending = append(pending, tx.pendingReadFirst)
		tx.pendingReadFirst = nil
	}
	for p := range tx.pendingReads {
		pending = append(pending, p)
	}
	tx.pendingReads = nil
	return pending
}

func retirePending(pending []*PendingGet, join bool) {
	for _, p := range pending {
		if join {
			p.mu.Lock()
		} else if !p.mu.TryLock() {
			continue // the resolver/finalizer holding mu owns all cleanup
		}
		p.retireLocked()
		p.mu.Unlock()
	}
}

// turnoverToken owns publication and resetMu until finish is called exactly once.
type turnoverToken struct {
	tx *Transaction
}

// beginTurnover returns with resetMu held and no old state users. Its token
// publishes the fully rebuilt replacement and releases resetMu.
// Internal callers must release their lease BEFORE requesting turnover.
func (tx *Transaction) beginTurnover(expected *readIncarnation, released *executionLease) (turnoverToken, error) {
	if released != nil && released.tx != tx {
		return turnoverToken{}, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	tx.readErrMu.Lock()
	selfWait := released != nil && released.active.Load()
	tx.readErrMu.Unlock()
	if selfWait {
		return turnoverToken{}, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	tx.resetMu.Lock()
	if tx.beforeTurnoverRetirement != nil {
		tx.beforeTurnoverRetirement()
	}
	tx.readErrMu.Lock()
	old := tx.readIncarnationLocked()
	if expected != nil && (old != expected || expected.cause != nil) {
		cause := expected.cause
		tx.readErrMu.Unlock()
		tx.resetMu.Unlock()
		if cause == nil {
			cause = &wire.FDBError{Code: ErrTransactionCancelled}
		}
		return turnoverToken{}, cause
	}
	// Expected-owner validation and retirement are one leaf-locked claim.
	// Releasing it here lets a completed Cancel/timeout be erased by reset.
	return tx.turnoverLocked(old), nil
}

func (tx *Transaction) startTurnover() turnoverToken {
	tx.resetMu.Lock()
	tx.readErrMu.Lock()
	return tx.turnoverLocked(tx.readIncarnationLocked())
}

// turnoverLocked enters with resetMu and readErrMu held. It releases readErrMu
// before delivering cancellation or waiting, and owns resetMu through the
// returned publication token.
func (tx *Transaction) turnoverLocked(old *readIncarnation) turnoverToken {
	pending := tx.detachPendingLocked(old)
	tx.readReady = unobservedReadReady
	tx.readLife = nil
	tx.readGen++
	tx.readErr = nil
	var cancelErr error
	if tx.transactFinalCommit.Load() && !tx.turnoverErrUsed {
		tx.turnoverErrUsed = true
		tx.turnoverErr = wire.FDBError{Code: ErrTransactionCancelled}
		cancelErr = &tx.turnoverErr
	} else {
		cancelErr = &wire.FDBError{Code: ErrTransactionCancelled}
	}
	tx.recordCauseLocked(old, cancelErr)
	cause := old.cause
	tx.disarmTimeoutLocked()
	first, native := old.nativeCommit, old.nativeCommits
	old.nativeCommit, old.nativeCommits = nil, nil
	var drained <-chan struct{}
	if old.users != 0 {
		old.drained = make(chan struct{})
		drained = old.drained
	}
	tx.readErrMu.Unlock()
	if first != nil {
		first.stop()
	}
	for commit := range native {
		commit.stop()
	}
	old.cancel(cause)
	retirePending(pending, false)
	if drained != nil {
		<-drained
	}
	retirePending(pending, true)
	return turnoverToken{tx: tx}
}

func (token turnoverToken) finish() {
	tx := token.tx
	tx.readErrMu.Lock()
	// Captured successors and non-final positive timeouts are created/armed here;
	// final Transact commits defer uncaptured successor creation to readIncarnationLocked.
	finalCommit := tx.transactFinalCommit.Load()
	if tx.readLife != nil || (tx.timeoutNs.Load() > 0 && !finalCommit) {
		tx.readIncarnationLocked()
	}
	ready := tx.readReady
	tx.readReady = nil
	if !finalCommit || tx.readLife != nil {
		tx.armTimeoutLocked()
	} else if tx.timeoutNs.Load() > 0 {
		tx.timeoutNeedsArm = true
	}
	inc := tx.readLife
	var cause error
	if inc != nil {
		cause = inc.cause
	}
	if ready != unobservedReadReady {
		close(ready)
	}
	tx.readErrMu.Unlock()
	tx.resetMu.Unlock()
	if cause != nil {
		inc.cancel(cause)
	}
}

type (
	watchParametersKey struct{}
	watchParameters    struct {
		tenantID   int64
		readTags   []byte // the encoded read tags at setup (NativeAPI.actor.cpp:5700)
		activation *watchActivation
	}
)

// WatchActivationFor returns the activation captured by WatchSetup. Keeping
// activation and read version in one setup incarnation prevents Reset between
// setup and future construction from binding the watch to its successor.
func (tx *Transaction) WatchActivationFor(ctx context.Context) *watchActivation {
	if ctx == nil {
		return nil
	}
	if parameters, ok := ctx.Value(watchParametersKey{}).(watchParameters); ok && parameters.activation != nil {
		return parameters.activation
	}
	return tx.WatchActivation()
}

// watchSetupTags returns the read tags captured when the watch was set up.
func (tx *Transaction) watchSetupTags(ctx context.Context) []byte {
	if parameters, ok := ctx.Value(watchParametersKey{}).(watchParameters); ok {
		return parameters.readTags
	}
	// Direct WatchPoll/sendWatch callers did not capture setup parameters.
	lease := tx.enterState()
	defer lease.release()
	return encodeTagSet(tx.readTags)
}

// watchCommitTags returns the read tags as of the commit that activated the
// watch, else those captured at setup.
func (tx *Transaction) watchCommitTags(ctx context.Context) []byte {
	if parameters, ok := ctx.Value(watchParametersKey{}).(watchParameters); ok && parameters.activation != nil {
		select {
		case <-parameters.activation.done:
			if !parameters.activation.abandoned {
				return parameters.activation.readTags
			}
		default:
		}
	}
	return tx.watchSetupTags(ctx)
}

func (tx *Transaction) watchTenant(ctx context.Context) int64 {
	if parameters, ok := ctx.Value(watchParametersKey{}).(watchParameters); ok {
		return parameters.tenantID
	}
	// Direct WatchPoll/sendWatch callers did not capture setup parameters.
	lease := tx.enterState()
	defer lease.release()
	return tx.tenantId
}
