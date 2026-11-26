package client

import (
	"context"
	"errors"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

const (
	wrongShardRetryDelay = 10 * time.Millisecond // CLIENT_KNOBS->WRONG_SHARD_SERVER_DELAY
	watchPollingTime     = 1 * time.Second       // CLIENT_KNOBS->WATCH_POLLING_TIME
	replyByteLimit       = 80000                 // CLIENT_KNOBS->REPLY_BYTE_LIMIT
	// ByteLimitUnlimited mirrors GetRangeLimits::BYTE_LIMIT_UNLIMITED (FDBTypes.h). A range
	// read carrying it has no per-fetch byte TARGET — only the flat replyByteLimit ceiling
	// applies — so the soft byte limit in rangeScanImpl can never fire and the scan stays
	// bounded by rows alone. This is EXACT's value, per mode_bytes_array[EXACT] (fdb_c.cpp:1002).
	ByteLimitUnlimited = -1
	// maxReadTimeoutRetries bounds the re-send of a read whose RPC reply
	// timed out (errReplyTimeout). libfdb_c has no per-RPC reply timeout and
	// waits (bounded by the transaction's read-version validity, ~5s MVCC window); a re-send of
	// our read with the same read version converges to a server-side
	// transaction_too_old once the window passes, so a small cap is a safety
	// backstop against a wedged connection, not the primary bound. On
	// exhaustion the read path surfaces a RETRYABLE transaction_too_old.
	maxReadTimeoutRetries = 10
	// maxSelectorResolutionSteps bounds the number of SUCCESSFUL partial
	// selector resolutions in getKey (each crosses one shard boundary). A
	// legitimate deep selector crosses at most one shard per resolution and
	// converges; this generous cap (far above any realistic cluster shard
	// count touched by one selector) only trips on a non-converging selector
	// or a misbehaving server — a non-retryable terminal condition, not a
	// transient one. C++ has no such cap (it relies on the transaction
	// timeout); this is the bounded-client analog.
	maxSelectorResolutionSteps = 1000
	// maxRelocateRetries bounds a range read's relocations after wrong_shard_server
	// or all_alternatives_failed; C++ relies on the transaction timeout (default 5s).
	// A maybe-delivered failure is loadBalance's own unbounded retry (lbRetry).
	maxRelocateRetries = 5
)

// readTypeNormal is C++ ReadType::NORMAL (FDBTypes.h:1732, enum value 3) — the read-priority rank
// the storage server applies. NOT EAGER (0).
const readTypeNormal int32 = 3

// KeyValue is a key-value pair returned from reads.
type KeyValue = types.KeyValueRef

// allKeysEnd is \xFF\xFF — the absolute end of the key space.
var allKeysEnd = []byte{0xFF, 0xFF}

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() for empty = sizeof(size_t) + sizeof(Version) = 16.
// C++ encodes: [utlCount=0 (8 bytes LE)] [maxVersion=invalidVersion=-1 (8 bytes LE)]
var emptyVersionVector = func() []byte {
	b := make([]byte, 16)
	// utlCount = 0 (already zero)
	// maxVersion = invalidVersion = -1
	b[8] = 0xFF
	b[9] = 0xFF
	b[10] = 0xFF
	b[11] = 0xFF
	b[12] = 0xFF
	b[13] = 0xFF
	b[14] = 0xFF
	b[15] = 0xFF
	return b
}()

// lockAwareReadOptions is the ReadOptions a lock-aware read sends. C++ LOCK_AWARE/READ_LOCK_AWARE
// initialize a DEFAULT ReadOptions (NativeAPI.actor.cpp:7072-7090) whose ctor sets
// type=NORMAL(3), cacheResult=true (FDBTypes.h:1748), THEN set lockAware=true. Sending only
// {LockAware:true} (Type=0/EAGER, CacheResult=false) is wire-wrong: it lands the read in the EAGER
// SS read-priority rank and disables result caching, diverging from libfdb_c on every lock-aware read.
func lockAwareReadOptions() types.ReadOptions {
	return types.ReadOptions{Type: readTypeNormal, CacheResult: true, LockAware: true}
}

// readRPCTimeout is the per-RPC reply timeout for this transaction's reads:
// the transaction's override when set (test-only), else DefaultRPCTimeout.
func (tx *Transaction) readRPCTimeout() time.Duration {
	if tx.hooks != nil && tx.hooks.rpcTimeoutOverride > 0 {
		return tx.hooks.rpcTimeoutOverride
	}
	return DefaultRPCTimeout
}

// sleepCtx sleeps for the given duration but returns early if ctx is cancelled.
// Returns ctx.Err() if the context was cancelled, nil otherwise.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	notifyParked(ctx) // the delay is already running when the actor suspends
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// keySelectorIsBackward reports whether a key selector resolves BACKWARD (toward smaller keys) —
// the exact C++ KeySelectorRef::isBackward() predicate, `!orEqual && offset <= 0` (FDBTypes.h:659-661).
// It drives getKey's shard-location routing: a backward selector on a cross-server shard boundary must
// resolve to the shard ENDING at the key (rangeContainingKeyBefore), else it routes to the wrong SS and
// livelocks (RFC-169). Both terms are load-bearing: `lastLessThan(k)=(k,false,0)` is backward, but
// `lastLessOrEqual(k)=(k,true,0)` is NOT (orEqual=true, FDBTypes.h:683) — dropping the `!orEqual` term
// would mistag lastLessOrEqual as backward and route it to the shard the SS won't serve for it (whose
// real isBackward() is false → rangeContaining, storageserver.actor.cpp:4504), mirroring the bug.
func keySelectorIsBackward(orEqual bool, offset int32) bool {
	return !orEqual && offset <= 0
}

// getAdjustedEndpoint computes the endpoint token for interface method at given index.
// C++ Endpoint::getAdjustedEndpoint(n): first += (n << 32), second.lower32 += n.
func getAdjustedEndpoint(base transport.UID, index int) transport.UID {
	baseIndex := uint32(base.Second)
	return transport.UID{
		First:  base.First + (uint64(index) << 32),
		Second: (base.Second & 0xFFFFFFFF00000000) | uint64(baseIndex+uint32(index)),
	}
}

// fdbErrorCode returns err's FDB error code. errors.AsType, unlike errors.As,
// does not heap-allocate a target on every call.
func fdbErrorCode(err error) (int, bool) {
	fdbErr, ok := errors.AsType[*wire.FDBError](err)
	if !ok || fdbErr == nil {
		return 0, false
	}
	return fdbErr.Code, true
}

// isWrongShardServer returns true if the error is FDB error 1001 (wrong_shard_server).
func isWrongShardServer(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == ErrWrongShardServer
}

// isAllAlternativesFailed returns true if the error is FDB error 1006.
func isAllAlternativesFailed(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == ErrAllAlternativesFailed
}

// isMaybeDelivered reports whether err carries request_maybe_delivered (1030)
// or broken_promise (1100) — C++ loadBalance's maybeDelivered classification
// (LoadBalance.actor.h:344: `maybeDelivered = errCode == error_code_broken_promise
// || errCode == error_code_request_maybe_delivered`). The transport delivers a
// coded 1030 for every connection teardown and the client for every failed
// connect (transport.ConnectFailedError), so this is how a read loop recognizes
// "the connection to my replica failed". For reads (AtMostOnce::False) C++
// loadBalance retries alternatives with backoff, unbounded, and never surfaces
// the code to the application. The Go read loops mirror that with lbRetry,
// re-locating each time (see getValueImpl). The commit
// path consults this too, but converts instead of retrying: commit runs through
// basicLoadBalance at AtMostOnce::True, which forbids a re-send, so commitpath.go
// normalises maybeDelivered to commit_unknown_result. Its four TRANSPORT arms do
// the same for teardowns coded 1030; this predicate is what additionally catches
// an IN-BAND 1100, which no transport arm ever sees.
func isMaybeDelivered(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && (code == ErrRequestMaybeDelivered || code == ErrBrokenPromise)
}

// isFutureVersionOrProcessBehind returns true for errors that should trigger
// future_version backoff in the QueueModel. Matches C++ ModelHolder::release()
// which passes futureVersion=true for future_version (1009) and process_behind (1037).
func isFutureVersionOrProcessBehind(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && (code == ErrFutureVersion || code == ErrProcessBehind)
}

// resolveFallback runs the sequential fallback after the speculative hedge fails: it tries every replica
// not already attempted (bestIdx/secondIdx) via trySingle and returns the first real value. When none
// succeeds it surfaces the most-specific remembered error, with precedence:
//
//	future_version / process_behind  >  reply-timeout  >  all_alternatives_failed
//
// The version-error precedence is the fix for finding #22: a future_version (1009) / process_behind
// (1037) reply from a fallback replica is a DEFINITIVE, retryable condition that C++ getValue propagates
// unchanged — its catch re-throws any error that is not wrong_shard_server / all_alternatives_failed
// (NativeAPI.actor.cpp:3738, `throw e`). The old loop matched only timeout / wrong_shard / all_alternatives
// and let a version error fall through: silently dropped, then flattened to errReplyTimeout (1007) or
// all_alternatives_failed (1006), hiding a retryable condition behind a less-actionable code. Timeout
// still does NOT stop the scan (a slow replica must not pre-empt a healthy one); only
// wrong_shard/all_alternatives ends it early (all replicas share the shard assignment, so re-locating is
// the right response). hedgeErr seeds the remembered timeout from the hedge arm's own failure — a
// transport error, never a reply-carried version error (those surface directly via parseGetValueReply on
// the hedge success path, so they never reach here).
//
// A maybeDelivered teardown (1030/1100 — one replica's connection died under the RPC) deliberately
// matches NO case: it must not end the scan (unlike wrong_shard/all_alternatives it says nothing about
// the other replicas — C++ loadBalance simply moves to the next alternative, LoadBalance.actor.h:344
// with the false return at :377) and must not be remembered as a timeout. When every failure was
// maybeDelivered (a failed connect or a teardown on each replica) it is returned as is: C++ loadBalance
// keeps retrying with backoff and raises all_alternatives_failed only when the FailureMonitor marks
// every alternative failed (LoadBalance.actor.h:604-635, 682-686); the pace callback paces each one.
// Any other unclassified failure still flattens to all_alternatives_failed.
func resolveFallback(ctx context.Context, hedgeErr error, servers []ServerInfo, bestIdx, secondIdx int, trySingle func(ServerInfo) ([]byte, error)) ([]byte, error) {
	return resolveFallbackPaced(ctx, hedgeErr, servers, bestIdx, secondIdx, trySingle, nil)
}

// resolveFallbackPaced is resolveFallback calling pace, when set, after each maybe-delivered replica.
func resolveFallbackPaced(ctx context.Context, hedgeErr error, servers []ServerInfo, bestIdx, secondIdx int, trySingle func(ServerInfo) ([]byte, error), pace func() error) ([]byte, error) {
	// A cancelled/expired CALLER context propagates IMMEDIATELY — a cancelled read must return ctx.Err(),
	// never a remembered version error or a flattened timeout. Gate on ctx.Err() (the real READ context),
	// NOT errors.Is on a per-server error: a per-server deadline can surface as a wrapped
	// context.DeadlineExceeded while the read context is still LIVE, and that must fall back to other
	// replicas rather than abort the scan (a failed connect is coded 1030 instead). Checked at
	// entry (covers the no-untried-fallback case) and after each replica in the loop below.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sawTimeout := isServerTimeout(hedgeErr)
	var versionErr, maybeDelivered error
	unclassified := false
	switch {
	case isMaybeDelivered(hedgeErr):
		maybeDelivered = hedgeErr
	case hedgeErr != nil && !sawTimeout:
		unclassified = true
	}
	for i, server := range servers {
		if i == bestIdx || i == secondIdx {
			continue // already tried by the hedge
		}
		val, err := trySingle(server)
		if err == nil {
			return val, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // caller cancelled mid-scan — propagate, never mask with a remembered error
		}
		switch {
		case isFutureVersionOrProcessBehind(err):
			if versionErr == nil {
				versionErr = err // remember, but keep trying — a later replica may still have the value
			}
		case isServerTimeout(err):
			sawTimeout = true // remember, keep scanning healthy replicas
		case isWrongShardServer(err) || isAllAlternativesFailed(err):
			return nil, err // definitive: all replicas share the shard assignment → re-locate
		case isMaybeDelivered(err):
			if maybeDelivered == nil {
				maybeDelivered = err // this replica is unreachable; keep scanning
			}
			if pace != nil {
				if err := pace(); err != nil {
					return nil, err
				}
			}
		default:
			unclassified = true
		}
	}
	switch {
	case versionErr != nil:
		return nil, versionErr
	case sawTimeout:
		// Prefer the timeout (getValue re-sends without a pointless cache invalidation — the location
		// is fine, the servers were slow) over flattening to all_alternatives_failed.
		return nil, errReplyTimeout
	case maybeDelivered != nil && !unclassified:
		return nil, maybeDelivered
	default:
		return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
	}
}

// isServerTimeout reports whether a per-replica error is a slow/unreachable-server TIMEOUT — either an RPC
// reply timeout (errReplyTimeout) or a wrapped context.DeadlineExceeded from a per-server deadline (a
// connect timeout is coded request_maybe_delivered, not this). resolveFallback calls this only AFTER confirming the caller's read context
// is still live (ctx.Err()==nil), so a DeadlineExceeded here is a per-server deadline, never caller
// cancellation. Such a timeout must be remembered as a timeout — surfaced as errReplyTimeout so getValue
// re-sends to the same location — NOT dropped and flattened to all_alternatives_failed (1006), which
// getValueImpl absorbs via the wrong-shard invalidate+relocate path, the wrong retry response to a merely
// slow server.
func isServerTimeout(err error) bool {
	return isReplyTimeout(err) || errors.Is(err, context.DeadlineExceeded)
}
