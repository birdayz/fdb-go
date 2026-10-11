package client

import (
	"context"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// The retained native commit shares the GRV, not the public resetPromise or
// transaction. Reset replaces this state after dropping the old actor.
type readVersionState struct {
	// Lock order: readVersionMu → traceMu, never the reverse. Never held over I/O.
	readVersionMu      sync.Mutex
	readVersion        int64
	hasReadVersion     bool
	userSetReadVersion bool
	pendingReadVersion *pendingReadVersion
	// Total latency spans OnError retries; the MVCC instant belongs to one GRV.
	metricStart        time.Time
	readVersionInstant time.Time
}

type pendingReadVersion struct {
	done    chan struct{}
	version int64
	err     error
}

func (tx *Transaction) cachedOrFetchReadVersion(ctx context.Context, extraFlags uint32) (int64, bool, error) {
	state := tx.readVersionState
	for {
		state.readVersionMu.Lock()
		if err := tx.readEntryError(ctx); err != nil {
			state.readVersionMu.Unlock()
			return 0, false, err
		}
		if state.metricStart.IsZero() {
			state.metricStart = time.Now()
			tx.ensureTxSpan()
		}
		if state.hasReadVersion {
			rv, userSet := state.readVersion, state.userSetReadVersion
			state.readVersionMu.Unlock()
			return rv, userSet, nil
		}
		if pending := state.pendingReadVersion; pending != nil {
			state.readVersionMu.Unlock()
			select {
			case <-ctx.Done():
				return 0, false, tx.mapReadError(ctx, ctx.Err())
			case <-pending.done:
				if err := tx.readEntryError(ctx); err != nil {
					return 0, false, err
				}
				if pending.err == nil {
					return pending.version, false, nil
				}
				// A cancelled producer must not cancel a different live waiter.
				continue
			}
		}
		pending := &pendingReadVersion{done: make(chan struct{})}
		state.pendingReadVersion = pending
		state.readVersionMu.Unlock()
		rv, err := tx.fetchReadVersion(ctx, extraFlags, state, pending)
		if err != nil {
			return 0, false, tx.mapReadError(ctx, err)
		}
		return rv, false, nil
	}
}

func (tx *Transaction) fetchReadVersion(ctx context.Context, extraFlags uint32, state *readVersionState, pending *pendingReadVersion) (rv int64, err error) {
	var rvAt time.Time
	err = context.Canceled // a panic must also release waiters without publishing a version
	defer func() {
		state.readVersionMu.Lock()
		pending.version, pending.err = rv, err
		if state.pendingReadVersion == pending {
			state.pendingReadVersion = nil
			if err == nil {
				state.readVersion, state.hasReadVersion = rv, true
				// A cache hit carries the original GRV instant, not this adoption time.
				state.readVersionInstant = rvAt
			}
		}
		close(pending.done)
		state.readVersionMu.Unlock()
	}()
	flags := tx.grvFlags() | extraFlags
	var locked bool
	rv, locked, rvAt, err = tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags, tx.currentSpan(), tx.tags, tx.useGrvCache, tx.skipGrvCache)
	if err == nil && locked && !(tx.lockAware || tx.readLockAware) {
		// C++ extractReadVersion rejects a locked database before adopting its version.
		err = &wire.FDBError{Code: ErrDatabaseLocked}
	}
	return rv, err
}

// Turnover has drained public state users and cancelled retained native work.
func (tx *Transaction) resetReadVersionState(preserveMetricStart bool) {
	next := &readVersionState{}
	if preserveMetricStart {
		tx.readVersionMu.Lock()
		next.metricStart = tx.metricStart
		tx.readVersionMu.Unlock()
	}
	tx.readErrMu.Lock()
	tx.readVersionState = next
	tx.readErrMu.Unlock()
}
