package client

import (
	"context"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
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

// readVersionRequest contains no public transaction or incarnation references.
// Native commits retain this snapshot and the shared GRV future after admission.
type readVersionRequest struct {
	db           *database
	span         types.SpanContext
	tags         []string
	flags        uint32
	useGrvCache  bool
	skipGrvCache bool
	lockAware    bool
}

func (tx *Transaction) readVersionRequest() readVersionRequest {
	return readVersionRequest{
		db: tx.db, span: tx.currentSpan(), tags: tx.tags, flags: tx.grvFlags(),
		useGrvCache: tx.useGrvCache, skipGrvCache: tx.skipGrvCache,
		lockAware: tx.lockAware || tx.readLockAware,
	}
}

type nativeReadState struct {
	*readVersionState
	request readVersionRequest
}

func (state *nativeReadState) readVersionForCommit(ctx context.Context) (int64, error) {
	rv, userSet, err := state.cachedOrFetchReadVersion(ctx, state.request, grvFlagCausalReadRisky, nil)
	if err == nil && userSet {
		err = state.request.validateExplicitVersion(ctx, rv)
	}
	return rv, err
}

func (tx *Transaction) cachedOrFetchReadVersion(ctx context.Context, extraFlags uint32) (int64, bool, error) {
	return tx.readVersionState.cachedOrFetchReadVersion(ctx, tx.readVersionRequest(), extraFlags, tx)
}

// owner supplies only public admission-error mapping and span initialization.
// Detached native work passes nil and observes its own context instead.
func (state *readVersionState) cachedOrFetchReadVersion(ctx context.Context, request readVersionRequest, extraFlags uint32, owner *Transaction) (int64, bool, error) {
	for {
		state.readVersionMu.Lock()
		if err := readVersionEntryError(ctx, owner); err != nil {
			state.readVersionMu.Unlock()
			return 0, false, err
		}
		if state.metricStart.IsZero() {
			state.metricStart = time.Now()
			if owner != nil {
				owner.ensureTxSpan()
			}
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
				return 0, false, mapReadVersionError(ctx, owner, ctx.Err())
			case <-pending.done:
				if err := readVersionEntryError(ctx, owner); err != nil {
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
		rv, err := request.fetchReadVersion(ctx, extraFlags, state, pending, owner)
		if err != nil {
			return 0, false, mapReadVersionError(ctx, owner, err)
		}
		return rv, false, nil
	}
}

func readVersionEntryError(ctx context.Context, owner *Transaction) error {
	if owner != nil {
		return owner.readEntryError(ctx)
	}
	return ctx.Err()
}

func mapReadVersionError(ctx context.Context, owner *Transaction, err error) error {
	if owner != nil {
		return owner.mapReadError(ctx, err)
	}
	return err
}

// owner, when set, supplies lock awareness at reply time: C++ extractReadVersion
// checks the transaction's options after the reply arrives
// (NativeAPI.actor.cpp:7405, 7425-7426). Detached native work uses its snapshot.
func (request readVersionRequest) fetchReadVersion(ctx context.Context, extraFlags uint32, state *readVersionState, pending *pendingReadVersion, owner *Transaction) (rv int64, err error) {
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
	flags := request.flags | extraFlags
	var locked bool
	rv, locked, rvAt, err = request.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(request.db, ctx, flags, request.span, request.tags, request.useGrvCache, request.skipGrvCache)
	lockAware := request.lockAware
	if owner != nil {
		lockAware = owner.lockAware || owner.readLockAware
	}
	if err == nil && locked && !lockAware {
		// C++ extractReadVersion rejects a locked database before adopting its version.
		err = &wire.FDBError{Code: ErrDatabaseLocked}
	}
	return rv, err
}

func (request readVersionRequest) validateExplicitVersion(ctx context.Context, rv int64) error {
	if request.db == nil {
		return nil
	}
	// A previous cluster's floor reads as unset and must be bootstrapped again.
	if request.db.minAcceptable() == 0 {
		flags := request.flags
		_, _, _, _ = request.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(request.db, ctx, flags, request.span, request.tags, request.useGrvCache, request.skipGrvCache)
	}
	return request.db.validateVersion(rv)
}

// Turnover has drained public state users and cancelled retained native work.
// Non-retry state can stay absent until readIncarnationLocked admits reuse.
func (tx *Transaction) resetReadVersionState(preserveMetricStart bool) {
	var next *readVersionState
	if preserveMetricStart {
		next = &readVersionState{}
		tx.readVersionMu.Lock()
		next.metricStart = tx.metricStart
		tx.readVersionMu.Unlock()
	}
	tx.readErrMu.Lock()
	tx.readVersionState = next
	tx.readErrMu.Unlock()
}
