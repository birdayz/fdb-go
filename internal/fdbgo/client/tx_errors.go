package client

import (
	"context"
	"errors"

	"fdb.dev/internal/fdbwire"
)

// ErrNeedFullRYW is returned by GetPipelined when the key has pending atomics
// that require a server read + merge through the full ryw.get() path.
var ErrNeedFullRYW = errors.New("need full RYW path")

// FDB error codes. Source of truth: flow/error_definitions.h + fdb_c.cpp fdb_error_predicate().
const (
	ErrNotCommitted              = 1020 // not_committed (conflict)
	ErrCommitUnknownResult       = 1021 // commit_unknown_result
	ErrTransactionTooOld         = 1007 // transaction_too_old
	ErrFutureVersion             = 1009 // future_version
	ErrWrongShardServer          = 1001 // wrong_shard_server (1062 is change_feed_cancelled — do not confuse)
	ErrTransactionCancelled      = 1025 // transaction_cancelled
	ErrTransactionTimedOut       = 1031 // transaction_timed_out (NEVER retryable)
	ErrAccessedUnreadable        = 1036 // accessed_unreadable — read of a pending versionstamped key (NOT retryable; RFC-098)
	ErrProcessBehind             = 1037 // process_behind
	ErrServerOverloaded          = 1211 // server_overloaded
	ErrWatchCancelled            = 1029 // watch_cancelled — SS watch limit exceeded; client polls instead
	ErrTooManyWatches            = 1032 // too_many_watches — outstanding-watch cap exceeded (client-side)
	ErrTimedOut                  = 1004 // timed_out — the SS occasionally times out a watch; re-arm
	ErrDatabaseLocked            = 1038 // database_locked
	ErrClusterVersionChanged     = 1039 // cluster_version_changed (MAYBE_COMMITTED)
	ErrProxyMemoryLimitExceeded  = 1042 // commit_proxy_memory_limit_exceeded
	ErrBatchTransactionThrottled = 1051 // batch_transaction_throttled
	ErrGrvProxyMemoryLimit       = 1078 // grv_proxy_memory_limit_exceeded
	ErrBlobGranuleRequestFailed  = 1079 // blob_granule_request_failed (retryable, C++ NativeAPI.actor.cpp)
	ErrTagThrottled              = 1213 // tag_throttled
	ErrProxyTagThrottled         = 1223 // proxy_tag_throttled
	ErrThrottledHotShard         = 1235 // transaction_throttled_hot_shard (FDB 7.4+; not retried by pinned 7.3.77)
	ErrRangeLocked               = 1242 // transaction_rejected_range_locked (FDB 7.4+; not retried by pinned 7.3.77)
	ErrOperationFailed           = 4    // operation_failed (endpoint not supported)
	ErrAllAlternativesFailed     = 1006 // all_alternatives_failed (Layer 2 only)
	ErrRequestMaybeDelivered     = 1030 // request_maybe_delivered — an in-flight RPC's connection died (transport teardown); reads retry another alternative, commit maps it to 1021
	ErrBrokenPromise             = 1100 // broken_promise — the peer dropped the reply promise; same maybeDelivered class as 1030 (LoadBalance.actor.h:344)
	ErrInvertedRange             = 2005 // inverted_range (begin > end)
	ErrInvalidOptionValue        = 2006 // invalid_option_value (e.g. SIZE_LIMIT outside [32, 1e7])
	ErrRangeLimitsInvalid        = 2012 // range_limits_invalid (e.g. a row limit < -1)
	ErrInternalError             = 4100 // internal_error
	ErrInvalidMutationType       = 2018 // invalid_mutation_type (a non-atomic op passed to Atomic())
	ErrTransactionReadOnly       = 2023 // transaction_read_only (a READ_LOCK_AWARE txn committed writes)
	ErrTenantNotFound            = 2131 // tenant_not_found
)

// checkCancelled returns transaction_cancelled (1025) if the transaction has been cancelled.
// C++ cancel() does resetPromise.sendError(transaction_cancelled) (ReadYourWrites.actor.cpp:2730)
// and EVERY RYW op races resetPromise, so a cancelled txn resolves every op with 1025. This is
// the Go analogue of that per-op resetPromise check: read admission calls this
// without acquiring a read version; other operations call it directly at entry.
// Apps branch on err.Code == 1025 (RFC-068), so the code must match.
func (tx *Transaction) checkCancelled() error {
	tx.readErrMu.Lock()
	var cause error
	if tx.readLife != nil {
		cause = tx.readLife.cause
	}
	tx.readErrMu.Unlock()
	if cause != nil {
		return cause
	}
	if txState(tx.state.Load()) == txStateCancelled {
		return &wire.FDBError{Code: 1025} // transaction_cancelled
	}
	return nil
}

// trackReadError records err as this transaction's first failed read (see the
// readErr field — the C++ ryw->reading analogue, which fails a later Commit
// with the same error). Returns err unchanged so read tails can
// `return v, tx.trackReadError(err)`. Operations that captured an incarnation
// must use trackReadOperation/trackReadErrorGen instead: even a synchronous
// call can finish on another goroutine after Reset.
func (tx *Transaction) trackReadError(err error) error {
	// Nil/ctx check BEFORE the lock: every successful read funnels through
	// here — taking readErrMu on the hot path would serialize concurrent
	// pipelined reads on the mutex that also guards pendingReads.
	if !isTrackableReadError(err) {
		return err
	}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.readErr == nil {
		tx.readErr = err
	}
	return err
}

// trackReadErrorGen is trackReadError for reads issued under generation gen:
// the recording is dropped if the transaction has been reset since (C++ swaps
// the reading AndFuture on resetRyow, detaching in-flight reads).
func (tx *Transaction) trackReadErrorGen(err error, gen uint64) error {
	if !isTrackableReadError(err) {
		return err
	}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if gen == tx.readGen && tx.readErr == nil {
		tx.readErr = err
	}
	return err
}

// isTrackableReadError: ctx cancellation has no C++ analogue (cancellation is
// whole-transaction via resetPromise) and must not poison commit.
func isTrackableReadError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// deferIllegalLocked defers an illegal write's error: RYW throws it in the call (RYW:2301-2348, 2420, 2228-2241)
// and libfdb_c keeps the first throw (ThreadSafeTransaction.cpp:589-617). Caller holds conflictMu.
func (tx *Transaction) deferIllegalLocked(m Mutation) {
	if tx.deferredErr.Load() != nil {
		return
	}
	if err := tx.validateMutation(m, tx.maxWriteKey()); err != nil {
		tx.deferErrorLocked(err)
	}
}

// deferErrorLocked stores err as the deferred error unless one is set; kept
// apart so that a legal write takes no address for errors.As.
func (tx *Transaction) deferErrorLocked(err error) {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		tx.deferredErr.CompareAndSwap(nil, fe)
	}
}

// recordDeferredError stores poison unless an error is deferred, as libfdb_c keeps the first throw
// (ThreadHelper.actor.h:44-61); under conflictMu so Commit's snapshot orders it against the op-log.
func (tx *Transaction) recordDeferredError(poison *wire.FDBError) {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	tx.deferredErr.CompareAndSwap(nil, poison)
}
