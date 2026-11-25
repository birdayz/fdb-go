package client

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	"fdb.dev/internal/fdbwire"
)

// Backoff constants — match C++ CLIENT_KNOBS.
const (
	defaultBackoff                = 10 * time.Millisecond // C++: DEFAULT_BACKOFF
	backoffGrowthRate             = 2.0                   // C++: BACKOFF_GROWTH_RATE
	maxBackoff                    = 1 * time.Second       // C++: DEFAULT_MAX_BACKOFF
	resourceConstrainedMaxBackoff = 30 * time.Second      // C++: RESOURCE_CONSTRAINED_MAX_BACKOFF
	futureVersionDelay            = 10 * time.Millisecond // C++: FUTURE_VERSION_RETRY_DELAY
)

// backoffSleep waits for `d` or until ctx fires, whichever comes first.
// Returns ctx.Err() on cancellation so callers can propagate it; nil on
// natural elapse. Used by OnError and commitDummyTransaction so a cancelled
// context doesn't keep clients pinned through a 1–30s backoff window.
func backoffSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffSleepBounded sleeps for `delay`, but a timeout delivered during the sleep cuts it short
// and surfaces transaction_timed_out (1031), matching C++ RYWImpl::onError which races the backoff
// against resetPromise (ReadYourWrites.actor.cpp:1517). With no timeout set it is exactly
// backoffSleep. A genuine parent-ctx cancellation still surfaces ctx.Err().
func (tx *Transaction) backoffSleepBounded(parent context.Context, delay time.Duration) error {
	ctx, cancel := tx.opContext(parent)
	defer cancel()
	return tx.mapReadError(ctx, backoffSleep(ctx, delay))
}

// borrowedOnErrorVerdict preserves OnError's verdict ordering for an internal
// borrower, but cannot publish timeout/abort failure into its owner's state.
func (tx *Transaction) borrowedOnErrorVerdict(caller context.Context, op *readOperation, err error) error {
	cause := tx.readIncarnationCause(op.inc)
	if cause != nil {
		var terminal *wire.FDBError
		if !errors.As(cause, &terminal) || terminal.Code != ErrTransactionTimedOut {
			return cause
		}
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		return err
	}
	if cause != nil {
		if callerErr := caller.Err(); callerErr != nil {
			return callerErr
		}
		return cause
	}
	if fdbErr.Code == ErrTransactionTimedOut {
		return err
	}
	if tx.hasRetryLimit && tx.retryCount >= tx.retryLimit {
		if fdbErr.Code == 0 {
			return &wire.FDBError{Code: ErrInternalError}
		}
		return err
	}
	if fdbErr.Code == 0 {
		return &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	if !onErrorRetryable(fdbErr.Code) {
		return err
	}
	if callerErr := caller.Err(); callerErr != nil {
		return callerErr
	}
	return &wire.FDBError{Code: ErrClientInvalidOperation}
}

// OnError handles a transaction error. Returns nil if the error is retryable
// (the transaction has been reset for retry). Returns the error if non-retryable
// or ctx.Err() if ctx fires during the backoff sleep. Retry log handlers receive
// the original caller context, not this operation's cancellation context.
func (tx *Transaction) OnError(ctx context.Context, err error) (rerr error) {
	callerCtx := ctx
	ctx, release, owned := tx.opContextWithLease(ctx, false)
	defer release.release()
	op := tx.readOperation(ctx)
	if owned == nil && op.lease.isActive() {
		// Public callers never receive an operation token. An internal
		// borrower cannot invoke observers or retire its owner's state.
		return tx.borrowedOnErrorVerdict(callerCtx, op, err)
	}
	defer func() { owned.release() }()
	// Terminal OnError cancels uncommitted watches so their slots do not leak; retry does so in reset.
	// Committed watches have already left this registry and retain independent future/parent ownership.
	defer func() {
		if rerr != nil {
			tx.cleanupOnErrorWatches(ctx, op.inc, owned)
		}
	}()
	fdbErr, termErr := tx.classifyOnError(callerCtx, ctx, op, err)
	if termErr != nil || fdbErr == nil {
		return termErr
	}
	var selfConflicts []KeyRange
	if fdbErr.Code == ErrCommitUnknownResult || fdbErr.Code == ErrClusterVersionChanged {
		selfConflicts = tx.copyWriteConflictsForRetry()
	}
	tx.retryCount++
	var cerr error
	ctx, op, owned, cerr = tx.logRetryAndReenter(callerCtx, ctx, op, owned, fdbErr.Code)
	if cerr != nil {
		return cerr
	}
	var delay time.Duration
	if fdbErr.Code == ErrTransactionTooOld || fdbErr.Code == ErrFutureVersion {
		// Version-related: fixed delay, no backoff growth.
		// C++ NativeAPI.actor.cpp: min(FUTURE_VERSION_RETRY_DELAY, maxBackoff).
		delay = futureVersionDelay
		if tx.maxRetryDelay > 0 && tx.maxRetryDelay < delay {
			delay = tx.maxRetryDelay
		}
	} else {
		delay = tx.nextBackoff(fdbErr.Code)
	}
	if cerr := tx.backoffSleepBounded(ctx, delay); cerr != nil {
		tx.state.Store(int32(txStateErrored))
		return cerr
	}
	owned.release()
	return tx.resetExpected(false, op.inc, owned, selfConflicts)
}

func (tx *Transaction) cleanupOnErrorWatches(ctx context.Context, inc *readIncarnation, owned *executionLease) {
	tx.failCapturedIncarnation(inc, &wire.FDBError{Code: ErrTransactionCancelled})
	if tx.hooks != nil && tx.hooks.beforeOnErrorWatchCleanup != nil {
		tx.hooks.beforeOnErrorWatchCleanup()
	}
	if !owned.isActive() {
		// Conditional turnover can reject a timeout after releasing
		// the operation's token. Reclaim cleanup authority only on the
		// same incarnation; a successor's watches are never ours.
		cleanupLease := tx.enterReadState(ctx, inc, false)
		if cleanupLease == nil {
			return
		}
		defer cleanupLease.release()
	}
	tx.cancelWatches()
}

func (tx *Transaction) classifyOnError(callerCtx, ctx context.Context, op *readOperation, err error) (*wire.FDBError, error) {
	// A cancelled txn can never be retried — C++ OnError races resetPromise and returns
	// transaction_cancelled (1025) (ReadYourWrites.actor.cpp). Without this, OnError on a
	// cancelled txn would reset-and-retry a retryable input error (return nil), reusing a
	// cancelled handle — a real divergence (RFC-068).
	if op.lease == nil {
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) {
			return nil, err
		}
		if e := callerCtx.Err(); e != nil {
			return nil, e
		}
		return nil, tx.readLifetimeError(ctx)
	}
	tx.readErrMu.Lock()
	terminalCause := op.inc.cause
	tx.readErrMu.Unlock()
	if cerr := terminalCause; cerr != nil {
		var terminal *wire.FDBError
		if !errors.As(cerr, &terminal) || terminal.Code != ErrTransactionTimedOut {
			return nil, cerr
		}
		// The timeout belongs to the FDB retry boundary below. A Go
		// application error or caller cancellation still escapes unchanged.
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		// A non-FDB application error (e.g. a Database.Transact callback returning errors.New(...))
		// is the caller's own, NOT an FDB retry concern — it escapes unchanged, even past the
		// deadline. The timeout gate below applies ONLY to FDB errors (the retry path), so it must
		// sit AFTER this branch (otherwise an expired deadline would replace the application
		// error with 1031). Pre-bug-hunt behavior, restored.
		tx.state.Store(int32(txStateErrored))
		return nil, err
	}
	// A delivered timeout out-ranks the retry for an FDB error: C++ RYWImpl::onError throws
	// transaction_timed_out at entry if the timebomb already fired (ReadYourWrites.actor.cpp:1506) —
	// before classifying the FDB error and before any backoff. An elapsed deadline whose timeout
	// is not yet delivered is no cause; the backoff below races its delivery. Non-retryable.
	// BUT a done CALLER ctx out-ranks the txn timeout:
	// if both the transaction timeout and the caller's ctx have fired, a TransactCtx caller must get
	// their own context.Canceled/DeadlineExceeded, not 1031.
	if cerr := tx.readIncarnationCause(op.inc); cerr != nil {
		tx.state.Store(int32(txStateErrored))
		if ctxErr := callerCtx.Err(); ctxErr != nil {
			return nil, ctxErr // the caller's own cancellation/deadline out-ranks the txn timeout
		}
		return nil, cerr // the recorded cause: the timeout above, or one recorded since
	}

	// Transaction timeout is NEVER retryable — matches C++ behavior where
	// OnError(1031) returns 1031 and the error escapes the retry loop.
	if fdbErr.Code == ErrTransactionTimedOut {
		tx.state.Store(int32(txStateErrored))
		return nil, err
	}

	// Check retry limit before allowing any retry.
	if tx.hasRetryLimit && tx.retryCount >= tx.retryLimit {
		tx.state.Store(int32(txStateErrored))
		if fdbErr.Code == 0 {
			// RYW throws success before NativeAPI can classify it; delivering
			// that invalid error trips Flow's assertion and surfaces internal_error.
			return nil, &wire.FDBError{Code: ErrInternalError}
		}
		return nil, err
	}

	// RYW checks the retry limit before NativeAPI rejects success as client_invalid_operation.
	if fdbErr.Code == 0 {
		tx.state.Store(int32(txStateErrored))
		return nil, &wire.FDBError{Code: ErrClientInvalidOperation}
	}

	// Single source of WHETHER to retry (RFC-105 — derive, don't mirror):
	// onErrorRetryable is the one onError-retryable predicate, also used by
	// commitDummyTransaction, so the two can never drift. A code it rejects errors
	// out here; the switch below only refines the BACKOFF CLASS for retryable
	// codes (its default arm covers the RETRYABLE_NOT_COMMITTED majority), so a
	// retryable code can never be silently dropped by a missing case.
	if !onErrorRetryable(fdbErr.Code) {
		tx.state.Store(int32(txStateErrored))
		return nil, err
	}
	return fdbErr, nil
}

func (tx *Transaction) copyWriteConflictsForRetry() []KeyRange {
	// MAYBE_COMMITTED: self-conflicting — deep-copy write conflicts to read
	// conflicts so the retry detects if our prior commit actually landed.
	// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_MAYBE_COMMITTED, code).
	//
	// Deep copy: KeyRange.Begin/End are sub-slices of conflictBuf, which
	// reset() reuses. Without a deep copy, retry writes overwrite the same
	// buffer positions, corrupting the selfConflicts data.
	tx.conflictMu.Lock()
	_, writes := tx.conflictRangesNowLocked() // with RYW, the write map's
	selfConflicts := make([]KeyRange, len(writes))
	for i, kr := range writes {
		buf := make([]byte, len(kr.Begin)+len(kr.End))
		copy(buf, kr.Begin)
		copy(buf[len(kr.Begin):], kr.End)
		selfConflicts[i] = KeyRange{Begin: buf[:len(kr.Begin)], End: buf[len(kr.Begin):]}
	}
	tx.conflictMu.Unlock()
	return selfConflicts
}

func (tx *Transaction) logRetryAndReenter(callerCtx, ctx context.Context, op *readOperation, owned *executionLease, code int) (context.Context, *readOperation, *executionLease, error) {
	db, count := tx.db, tx.retryCount
	if db == nil {
		return ctx, op, owned, nil
	}
	// Enabled and Handle are synchronous user code. Suspend execution,
	// not the operation, so same-transaction reentry can drain the owner.
	owned.release()
	db.countRetryAndLog(callerCtx, code, count)
	next := &readOperation{Context: ctx, readAdmission: op.readAdmission}
	owned = tx.enterReadStateInto(ctx, op.inc, false, false, &next.leaseStorage)
	next.lease = owned
	if owned != nil {
		ctx, op = next, next
	}
	if callerErr := callerCtx.Err(); callerErr != nil {
		return ctx, op, owned, callerErr
	}
	if owned == nil || tx.readIncarnationCause(op.inc) != nil {
		// Turnover publishes the old cause before refusing reacquisition.
		return ctx, op, owned, tx.mapReadError(ctx, context.Canceled)
	}
	return ctx, op, owned, nil
}

// nextBackoff returns the current backoff duration with jitter, then grows
// the backoff for the next call. Matches C++ getBackoff in NativeAPI.actor.cpp.
// errCode determines the backoff cap: the resource-constrained bucket —
// commit_proxy_memory_limit_exceeded (1042) and grv_proxy_memory_limit_exceeded
// (1078) — uses
// RESOURCE_CONSTRAINED_MAX_BACKOFF (30s) and IGNORES the user's maxRetryDelay;
// all other errors use the user's maxRetryDelay or DEFAULT_MAX_BACKOFF (1s). The
// two branches are mutually exclusive (see the cap selection below).
func (tx *Transaction) nextBackoff(errCode int) time.Duration {
	if tx.backoff == 0 {
		tx.backoff = defaultBackoff
	}
	// C++ pattern: return current * jitter, then grow for next time.
	jitter := rand.Float64()
	if tx.hooks != nil && tx.hooks.backoffJitter != nil {
		jitter = tx.hooks.backoffJitter() // test-only deterministic override
	}
	delay := time.Duration(float64(tx.backoff) * jitter)

	// Tag throttle: use server-supplied duration for tag_throttled errors.
	// C++ getBackoff(): max(backoff, min(TAG_THROTTLE_RECHECK_INTERVAL, tagThrottleDuration))
	if errCode == ErrTagThrottled && len(tx.tags) > 0 && tx.db != nil {
		tagDur := tx.db.tagThrottles.maxDuration(tx.priority, tx.tags)
		if tagDur > 0 {
			capped := tagDur
			if capped > tagThrottleRecheckInterval {
				capped = tagThrottleRecheckInterval
			}
			if capped > delay {
				delay = capped
			}
		}
	}

	// For proxy_tag_throttled, accumulate duration.
	// C++ NativeAPI.actor.cpp: trState->cx->throttledTags check in getBackoff.
	if errCode == ErrProxyTagThrottled {
		tx.proxyTagThrottledDuration += proxyMaxTagThrottleDuration.Seconds()
	}

	// C++ getBackoff(): the resource-constrained bucket (the two codes below) uses
	// RESOURCE_CONSTRAINED_MAX_BACKOFF exclusively (user's maxRetryDelay is IGNORED);
	// all other errors use the user's maxRetryDelay (or DEFAULT_MAX_BACKOFF). The two
	// branches are mutually exclusive — no min/max combining. (Code list + values are
	// in the function doc-comment, the single authoritative description.)
	var cap time.Duration
	if errCode == ErrProxyMemoryLimitExceeded || errCode == ErrGrvProxyMemoryLimit {
		cap = resourceConstrainedMaxBackoff
	} else {
		cap = maxBackoff
		if tx.maxRetryDelay > 0 {
			cap = tx.maxRetryDelay
		}
	}
	tx.backoff = time.Duration(math.Min(float64(tx.backoff)*backoffGrowthRate, float64(cap)))
	return delay
}
