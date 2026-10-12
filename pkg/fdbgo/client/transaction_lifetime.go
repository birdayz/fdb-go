package client

import (
	"context"

	"fdb.dev/pkg/fdbgo/wire"
)

// unobservedReadReady marks turnover without waiters. It stays open forever;
// only admission waiters replace it with a private publication channel.
var unobservedReadReady = make(chan struct{})

// executionLease protects reset-owned state, not wire I/O ownership. tx and inc
// are immutable; readErrMu guards active. Nested operations borrow the same token.
type executionLease struct {
	tx     *Transaction
	inc    *readIncarnation
	active bool
	issued bool // counted in inc.issuedReads, which Commit waits on
}

func (l *executionLease) release() {
	if l == nil {
		return
	}
	l.tx.readErrMu.Lock()
	var issuedDone chan struct{}
	if l.active && l.issued {
		l.inc.issuedReads--
		if l.inc.issuedReads == 0 {
			issuedDone, l.inc.issuedReadsDone = l.inc.issuedReadsDone, nil
		}
	}
	if l.active {
		l.active = false
		l.inc.users--
		if l.inc.users == 0 && l.inc.drained != nil {
			close(l.inc.drained)
			l.inc.drained = nil
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
	return &executionLease{tx: tx, inc: tx.admitState(), active: true}
}

// admitState waits out a turnover and counts one user of the current incarnation.
func (tx *Transaction) admitState() *readIncarnation {
	for {
		tx.readErrMu.Lock()
		ready := tx.readReady
		if ready == nil {
			inc := tx.readIncarnationLocked()
			inc.users++
			tx.readErrMu.Unlock()
			return inc
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
			inc.users++
			if issued {
				inc.issuedReads++
			}
			if lease == nil {
				lease = &executionLease{}
			}
			*lease = executionLease{tx: tx, inc: inc, active: true, issued: issued}
			tx.readErrMu.Unlock()
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

func (tx *Transaction) detachPendingLocked(inc *readIncarnation) []*PendingGet {
	if tx.readLife != inc {
		return nil
	}
	pending := make([]*PendingGet, 0, len(tx.pendingReads))
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
	selfWait := released != nil && released.active
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
	recordReadCauseLocked(old, &wire.FDBError{Code: ErrTransactionCancelled})
	cause, timer := old.cause, old.timer
	old.timer = nil
	native := old.nativeCommits
	old.nativeCommits = nil
	var drained <-chan struct{}
	if old.users != 0 {
		old.drained = make(chan struct{})
		drained = old.drained
	}
	tx.readErrMu.Unlock()
	if timer != nil {
		timer.Stop()
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
	// Captured successors and positive final timeouts must exist at publication.
	// Construct behind readReady so the timebomb is armed only once below.
	inc := tx.readLife
	if inc != nil || tx.timeoutNs.Load() > 0 {
		inc = tx.readIncarnationLocked()
	}
	ready := tx.readReady
	tx.readReady = nil
	var cause error
	if inc != nil {
		tx.armReadTimeoutLocked(inc)
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

func (tx *Transaction) watchTenant(ctx context.Context) int64 {
	if parameters, ok := ctx.Value(watchParametersKey{}).(watchParameters); ok {
		return parameters.tenantID
	}
	// Direct WatchPoll/sendWatch callers did not capture setup parameters.
	lease := tx.enterState()
	defer lease.release()
	return tx.tenantId
}
