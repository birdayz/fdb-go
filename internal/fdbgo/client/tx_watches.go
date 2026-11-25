package client

import (
	"context"

	"fdb.dev/internal/fdbwire"
)

// cancelWatches cancels pending watches, not those already handed off by a successful commit.
func (tx *Transaction) cancelWatches() {
	tx.watchMu.Lock()
	cancels := tx.watchCancels
	tx.watchCancels = nil
	// Rotate and abandon under the registration lock so a successor's watches
	// cannot join the cancellation set belonging to this activation.
	tx.resolveWatchActivationLocked(0, true)
	tx.watchMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// watchActivation is one incarnation's commit-completion signal (RFC-170 / #8). `done` closes when the
// incarnation's commit completes, the read-only fast path returns, or the txn is reset/cancelled; the
// waiter then reads version/abandoned (the close is the happens-before). version is committedVersion (>0)
// on a real commit, 0 for the read-only fallback; abandoned=true on reset/Cancel → the watch drains and
// never registers.
type watchActivation struct {
	done      chan struct{}
	version   int64
	abandoned bool
	readTags  []byte // the encoded read tags at the commit, which setupWatches sends (NativeAPI.actor.cpp:6429)
}

func (tx *Transaction) stateWatchActivation() *watchActivation {
	if act := tx.watchAct.Load(); act != nil {
		return act
	}
	fresh := &watchActivation{done: make(chan struct{})}
	if tx.watchAct.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return tx.watchAct.Load()
}

// fireWatchActivation publishes commit handoff and rotates activation for the next incarnation.
// Teardown instead abandons it through resolveWatchActivationLocked while holding watchMu.
func (tx *Transaction) fireWatchActivation(version int64, abandoned bool) {
	tx.watchMu.Lock()
	defer tx.watchMu.Unlock()
	if !abandoned {
		// Native setupWatches clears the transaction's watch list after handoff.
		// The future still owns its scoped cancel, independently of this registry.
		tx.watchCancels = nil
	}
	tx.resolveWatchActivationLocked(version, abandoned)
}

func (tx *Transaction) resolveWatchActivationLocked(version int64, abandoned bool) {
	if tx.watchAct.Load() == nil {
		return
	}
	prev := tx.watchAct.Swap(&watchActivation{done: make(chan struct{})})
	if prev == nil {
		return
	}
	prev.version = version
	prev.abandoned = abandoned
	if !abandoned {
		prev.readTags = encodeTagSet(tx.readTags)
	}
	close(prev.done)
}

// AwaitWatchCommit blocks until the watch's captured incarnation resolves, returning the version to
// register the watch at — committedVersion, or readVersion when the txn didn't commit (C++ setupWatches'
// `committedVersion > 0 ? committedVersion : readVersion`, NativeAPI.actor.cpp:6420). Returns an error when
// the watch is abandoned (reset/Cancel → 1025) or its scoped context is cancelled, so the facade drains
// without registering. RFC-170 (#8): the async facade calls this BEFORE WatchPoll.
func (tx *Transaction) AwaitWatchCommit(watchCtx context.Context, act *watchActivation, readVersion int64) (int64, error) {
	select {
	case <-act.done:
		if act.abandoned {
			return 0, &wire.FDBError{Code: 1025} // transaction_cancelled — reset/Cancel drained this watch
		}
		if act.version > 0 {
			return act.version, nil
		}
		return readVersion, nil
	case <-watchCtx.Done():
		return 0, watchCtx.Err()
	}
}

// ReleaseWatch releases the outstanding-watch slot WatchSetup reserved (C++ decreaseWatchCounter). It is
// normally released by WatchPoll on completion, but when AwaitWatchCommit aborts a watch BEFORE it reaches
// WatchPoll (never-committed / reset / Cancel / ctx-expiry), WatchPoll never runs — so the async facade
// calls this on that abort path, else the slot leaks and later watches fail with too_many_watches under
// MAX_WATCHES. Idempotent-safe by construction: WatchPoll runs only on the success path, this
// only on the abort path — never both for one watch.
func (tx *Transaction) ReleaseWatch() {
	if tx.db != nil {
		tx.db.releaseWatch()
	}
}

// newWatchCtx mints a fresh cancellable context for ONE watch (a child of the caller's parent, so the
// caller's own cancellation still propagates), registers it under a unique id, and returns the
// context plus a SCOPED cancel that cancels+deregisters only THIS watch (idempotent). The future's
// Cancel(), a failed WatchSetup, and WatchPoll's completion all call the scoped cancel — so one watch
// can be freed without touching sibling watches, the map self-cleans, and
// Cancel()/reset() (cancelWatches) still cancel every uncommitted watch. Captured synchronously in
// WatchSetup (the async WatchPoll only USES the context) — preserves the race-free bind.
func (tx *Transaction) newWatchCtx(parent context.Context) (context.Context, context.CancelFunc) {
	tx.watchMu.Lock()
	parent = context.WithValue(parent, watchParametersKey{}, watchParameters{
		tenantID:   tx.tenantId,
		readTags:   encodeTagSet(tx.readTags),
		activation: tx.stateWatchActivation(),
	})
	ctx, cancel := context.WithCancel(parent)
	if tx.watchCancels == nil {
		tx.watchCancels = make(map[uint64]context.CancelFunc)
	}
	id := tx.nextWatchID
	tx.nextWatchID++
	tx.watchCancels[id] = cancel
	tx.watchMu.Unlock()
	scoped := func() {
		tx.watchMu.Lock()
		delete(tx.watchCancels, id)
		tx.watchMu.Unlock()
		// Commit handoff removes the registry entry, not the future's ownership.
		cancel()
	}
	return ctx, scoped
}
