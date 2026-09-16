package client

import (
	"context"

	"fdb.dev/pkg/fdbgo/wire"
)

// executionLease protects reset-owned state, not wire I/O ownership. All fields
// are guarded by readErrMu. Nested operations borrow the same token.
type executionLease struct {
	tx     *Transaction
	inc    *readIncarnation
	active bool
}

func (l *executionLease) release() {
	if l == nil {
		return
	}
	l.tx.readErrMu.Lock()
	if l.active {
		l.active = false
		l.inc.users--
		if l.inc.users == 0 && l.inc.drained != nil {
			close(l.inc.drained)
			l.inc.drained = nil
		}
	}
	l.tx.readErrMu.Unlock()
}

// enterState admits synchronous, context-free state access. The lease, rather
// than a separate readiness check, protects the entire mutable-state phase.
func (tx *Transaction) enterState() *executionLease {
	for {
		tx.readErrMu.Lock()
		ready := tx.readReady
		if ready == nil {
			inc := tx.readIncarnationLocked()
			inc.users++
			lease := &executionLease{tx: tx, inc: inc, active: true}
			tx.readErrMu.Unlock()
			return lease
		}
		tx.readErrMu.Unlock()
		<-ready
	}
}

// enterReadState never rebinds a captured operation to a replacement. A pending
// resolver passes wait=false: it may only claim its own active incarnation.
func (tx *Transaction) enterReadState(ctx context.Context, inc *readIncarnation, wait bool) *executionLease {
	for {
		tx.readErrMu.Lock()
		if tx.readLife != inc {
			tx.readErrMu.Unlock()
			return nil
		}
		ready := tx.readReady
		if ready == nil {
			inc.users++
			lease := &executionLease{tx: tx, inc: inc, active: true}
			tx.readErrMu.Unlock()
			return lease
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

// beginTurnover returns with resetMu held and no old state users. Its finish
// closure publishes the fully rebuilt replacement and releases resetMu.
// Internal callers must release their token BEFORE requesting turnover.
func (tx *Transaction) beginTurnover(expected *readIncarnation, released *executionLease) (func(), error) {
	if released != nil && released.tx != tx {
		return nil, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	tx.readErrMu.Lock()
	selfWait := released != nil && released.active
	tx.readErrMu.Unlock()
	if selfWait {
		return nil, &wire.FDBError{Code: ErrClientInvalidOperation}
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
		return nil, cause
	}
	// Expected-owner validation and retirement are one leaf-locked claim.
	// Releasing it here lets a completed Cancel/timeout be erased by reset.
	return tx.turnoverLocked(old), nil
}

func (tx *Transaction) startTurnover() func() {
	tx.resetMu.Lock()
	tx.readErrMu.Lock()
	return tx.turnoverLocked(tx.readIncarnationLocked())
}

// turnoverLocked enters with resetMu and readErrMu held. It releases readErrMu
// before delivering cancellation or waiting, and owns resetMu through the
// returned publication closure.
func (tx *Transaction) turnoverLocked(old *readIncarnation) func() {
	pending := tx.detachPendingLocked(old)
	ready := make(chan struct{})
	tx.readReady = ready
	tx.readLife = nil
	tx.readGen++
	tx.readErr = nil
	recordReadCauseLocked(old, &wire.FDBError{Code: ErrTransactionCancelled})
	cause, timer := old.cause, old.timer
	old.timer = nil
	var drained <-chan struct{}
	if old.users != 0 {
		old.drained = make(chan struct{})
		drained = old.drained
	}
	tx.readErrMu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	old.cancel(cause)
	retirePending(pending, false)
	if drained != nil {
		<-drained
	}
	retirePending(pending, true)
	return func() {
		tx.readErrMu.Lock()
		// Reads can capture the replacement during turnover. Arm its timebomb
		// after reset-owned deadlines are final, whether it was captured early
		// or constructed here. close(ready) remains the publication edge.
		inc := tx.readIncarnationLocked()
		tx.readReady = nil
		tx.armReadTimeoutLocked(inc)
		cause := inc.cause
		close(ready)
		tx.readErrMu.Unlock()
		tx.resetMu.Unlock()
		if cause != nil {
			inc.cancel(cause)
		}
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
