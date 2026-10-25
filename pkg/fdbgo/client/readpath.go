package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// marshalBufPools for read-path request types.
var (
	getKeyBufPool       = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
	getValueBufPool     = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
	getKeyValuesBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
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
	// maxRelocateRetries bounds a range read's relocations after wrong_shard_server,
	// all_alternatives_failed or a maybe-delivered teardown; C++ relies on the
	// transaction timeout (default 5s).
	maxRelocateRetries = 5
)

// readTypeNormal is C++ ReadType::NORMAL (FDBTypes.h:1732, enum value 3) — the read-priority rank
// the storage server applies. NOT EAGER (0).
const readTypeNormal int32 = 3

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
	if tx.rpcTimeoutOverride > 0 {
		return tx.rpcTimeoutOverride
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

// allKeysEnd is \xFF\xFF — the absolute end of the key space.
var allKeysEnd = []byte{0xFF, 0xFF}

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

// getKey resolves a key selector via the storage server.
// Matches C++ NativeAPI.actor.cpp getKey(): loops until the selector is fully
// resolved (offset==0 && orEqual==true). A storage server resolves the selector
// within its shard; if the result crosses a shard boundary, it returns a partial
// resolution (non-zero offset). The client must then locate the new shard and
// re-issue the request with the updated selector.
func (tx *Transaction) getKey(parentCtx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	key, _, err := tx.getKeyForRange(parentCtx, selectorKey, orEqual, offset)
	return key, err
}

// getKeyForRange also reports whether the initial selector needs native range
// fallback, so client-side mapped resolution preserves that pagination behavior.
func (tx *Transaction) getKeyForRange(parentCtx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, bool, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	readVersion, err := tx.operationReadVersion(ctx)
	if err != nil {
		return nil, false, err
	}
	if sp := tx.startOpSpan("fdbgo.getKey"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getKey)
		defer sp.End()
	}
	k, fallback, err := tx.getKeyImpl(ctx, readVersion, selectorKey, orEqual, offset)
	return k, fallback, tx.mapReadError(ctx, err)
}

// getKeyImpl sends every request at readVersion, the version its operation captured.
func (tx *Transaction) getKeyImpl(ctx context.Context, readVersion int64, selectorKey []byte, orEqual bool, offset int32) ([]byte, bool, error) {
	return tx.getKeyImplWithReplyWait(ctx, readVersion, selectorKey, orEqual, offset, nil)
}

// A fallback resolver hands off at its first reply wait even if Go's concurrent
// read loop has already published the reply. Ordinary getKey leaves it nil.
func (tx *Transaction) getKeyImplWithReplyWait(ctx context.Context, readVersion int64, selectorKey []byte, orEqual bool, offset int32, replyWait func(context.Context)) ([]byte, bool, error) {
	tx.hadRead.Store(true) // a read was issued — the rywDisabled GetKey choke (RFC-059)
	if fn := tx.resolveKeyFn; fn != nil {
		key, err := fn(ctx, KeySelector{Key: selectorKey, OrEqual: orEqual, Offset: offset}, readVersion)
		return key, false, err
	}
	// Three independent budgets, because getKey's loop iterates for two
	// unrelated reasons: successful partial selector resolutions
	// (PROGRESS — a deep selector legitimately crosses many shards) and error
	// retries. Conflating them in one counter forced an impossible choice —
	// retryable exhaustion infinite-loops a deep selector, non-retryable
	// exhaustion aborts a transient wrong-shard storm. Separate them:
	//   - shardRetries (wrong_shard/all_alternatives) and timeoutRetries are
	//     transient error retries → exhaustion is RETRYABLE transaction_too_old
	//     (consistent with getValue/getRange);
	//   - progressSteps bounds total successful resolutions → exhaustion is a
	//     genuinely stuck/pathological selector (or a misbehaving server), a
	//     NON-retryable terminal error (retrying re-hits the same cap).
	timeoutRetries, shardRetries, progressSteps := 0, 0, 0
	needsRangeFallback := false
	var tags readTagSample
	redraw := true
	for {
		// C++ short-circuits: if key == allKeysEnd → offset > 0 → return allKeysEnd
		// if key == "" && offset <= 0 → return "" (empty key)
		// These checks are INSIDE the loop (matching C++) because the selector
		// key may be updated by a partial resolution from the previous iteration.
		if bytes.Equal(selectorKey, allKeysEnd) {
			if offset > 0 {
				return bytes.Clone(allKeysEnd), needsRangeFallback, nil
			}
			orEqual = false // C++: k.orEqual = false
		} else if len(selectorKey) == 0 && offset <= 0 {
			return []byte{}, needsRangeFallback, nil
		}

		// A BACKWARD key selector on a shard boundary must resolve to the shard ENDING at the key, not the
		// one beginning at it — else it routes to the wrong SS and re-locates forever (livelock). Thread it
		// through the location lookup + proxy request + invalidate, mirroring C++ getKey's
		// Reverse{k.isBackward()} (NativeAPI:3787-3788). See keySelectorIsBackward for the predicate.
		isBackward := keySelectorIsBackward(orEqual, offset)
		loc, err := tx.db.locCache.locateWithReplyWait(tx.db, ctx, selectorKey, tx.tenantId, tx.currentSpan(), isBackward, replyWait)
		if err != nil {
			return nil, false, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, false, fmt.Errorf("no storage servers for key")
		}

		if redraw {
			tags = tx.sampleReadTags() // GetKeyRequest per loop iteration (NativeAPI.actor.cpp:3806)
		}
		replyKey, replyOrEqual, replyOffset, err := tx.sendGetKey(ctx, readVersion, selectorKey, orEqual, offset, loc.Servers, tags, replyWait)
		redraw = err == nil || readTagRetry(err)
		if err != nil {
			// Reply timeout (slow-but-alive server): re-send, bounded, then a
			// RETRYABLE transaction_too_old — same contract as getValue/
			// getRange. errReplyTimeout must never escape.
			if isReplyTimeout(err) {
				timeoutRetries++
				if timeoutRetries > maxReadTimeoutRetries {
					return nil, false, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nil, false, err
				}
				continue
			}
			// A maybeDelivered teardown (1030/1100: the storage connection died
			// under the in-flight RPC) takes the same arm: C++ loadBalance
			// retries another alternative and exhausts to all_alternatives_failed,
			// which this arm already absorbs.
			if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
				// Native range reads switch to exact fallback on an initial wrong-shard reply.
				if progressSteps == 0 && isWrongShardServer(err) {
					needsRangeFallback = true
				}
				shardRetries++
				if shardRetries > MaxWrongShardRetries {
					// Transient routing error exhausted: retry the whole txn
					// with a fresh read version (consistent with getValue/
					// getRange; the read path never surfaces 1006 to the app).
					return nil, false, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				tx.db.locCache.invalidate(selectorKey, tx.tenantId, isBackward)
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nil, false, err
				}
				continue
			}
			return nil, false, err
		}

		if progressSteps == 0 {
			needsRangeFallback = needsRangeFallback || keyResolutionNeedsRangeFallback(replyKey, replyOrEqual, replyOffset, loc, isBackward)
		}
		// C++ NativeAPI.actor.cpp:1823-1826: k = reply.sel; if (!k.offset && k.orEqual) return k.getKey();
		// If offset==0 && orEqual==true, the selector is fully resolved.
		// Otherwise, the storage server returned a partial resolution — the
		// selector crossed a shard boundary. Update and loop.
		if replyOffset == 0 && replyOrEqual {
			return replyKey, needsRangeFallback, nil
		}
		selectorKey = replyKey
		orEqual = replyOrEqual
		offset = replyOffset
		progressSteps++
		if progressSteps > maxSelectorResolutionSteps {
			// The selector keeps resolving without converging — a pathological
			// selector or a misbehaving server, NOT a transient condition.
			// Non-retryable: a fresh read version would re-hit the same cap.
			return nil, false, &wire.FDBError{Code: ErrAllAlternativesFailed}
		}
	}
}

func keyResolutionNeedsRangeFallback(key []byte, orEqual bool, offset int32, loc LocationResult, reverse bool) bool {
	complete := orEqual && offset == 0
	if reverse {
		// getKey may resolve across adjacent readable shards in one response.
		return !complete || bytes.Compare(key, loc.ShardBegin) <= 0
	}
	// Mapped reads can use an interior partial FGE offset 1 directly, unlike getKey.
	return (!complete && offset != 1) || (loc.ShardEnd != nil && bytes.Compare(key, loc.ShardEnd) >= 0)
}

// sendGetKey sends a GetKeyRequest and returns the full KeySelector from the reply.
// Returns (key, orEqual, offset, error). The caller must check offset==0 && orEqual
// to determine if the selector is fully resolved. Matches C++ getKey() which uses
// the reply's KeySelector to drive the resolution loop.
func (tx *Transaction) sendGetKey(ctx context.Context, readVersion int64, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo, tags readTagSample, replyWait func(context.Context)) ([]byte, bool, int32, error) {
	servers, ok := tx.db.failMon.liveServers(servers, EndpointGetKey)
	if !ok {
		return nil, false, 0, tx.db.allAlternativesFailed(ctx)
	}
	bestIdx, secondIdx := tx.db.queueModel.chooseTopTwo(servers)
	if !tx.db.hedgeEnabled.Load() {
		secondIdx = -1
	}

	// Capture tx fields before closures (see sendGetValue comment).
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			gkToken := getAdjustedEndpoint(server.Token, EndpointGetKey)
			defer permit.Release()
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(gkToken)
			replyCleanup := replyGuard{handle: replyHandle}
			defer replyCleanup.close()
			reqData, bufp := buildGetKeyRequest(selectorKey, orEqual, offset, readVersion, lockAware, tenantId, span, tags, replyToken)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, gkToken, reqData, replyHandle, permit, replyWait); err != nil {
				getKeyBufPool.Put(bufp) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyCleanup.release()
				tx.db.handleReadConnError(server.Address, conn, err)
				return inFlightRPC{err: err, addr: server.Address}
			}
			getKeyBufPool.Put(bufp)
			replyCleanup.transfer()
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
				conn:        conn,
				delta:       delta,
				start:       start,
			}
		}
	}

	primary := makeSender(servers[bestIdx])
	var secondary sendFunc
	if secondIdx >= 0 {
		secondary = makeSender(servers[secondIdx])
	}

	hedgeDelay := tx.db.queueModel.secondDelay(servers[bestIdx].Address)
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, tx.readRPCTimeout())

	// End every started request that did not become the winner (hedge losers;
	// both arms on timeout/cancel) exactly once, else its QueueModel delta leaks
	// permanently and biases server selection. RFC-010 #5.
	for _, o := range result.others {
		tx.db.endOther(o)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleReadConnError(result.addr, result.conn, result.err)
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		}
	}
	if result.err != nil {
		return nil, false, 0, result.err
	}

	key, replyOrEqual, replyOffset, penalty, err := parseGetKeyReply(result.body)
	tx.db.noteReadBrokenPromise(err, servers, result.addr, EndpointGetKey)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return key, replyOrEqual, replyOffset, err
}

func buildGetKeyRequest(selectorKey []byte, orEqual bool, offset int32, version int64, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyRequest{
		Sel: types.KeySelectorRef{
			Key:     selectorKey,
			OrEqual: orEqual,
			Offset:  offset,
		},
		Version:                version,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span,
		SsLatestCommitVersions: emptyVersionVector,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if lockAware {
		req.HasOptions = true
		req.Options = lockAwareReadOptions()
	}
	bufp := getKeyBufPool.Get().(*[]byte)
	reqData := req.MarshalFDBPooled(*bufp)
	*bufp = reqData
	return reqData, bufp
}

// parseGetKeyReply parses the ErrorOr<GetKeyReply> response.
// Returns the full KeySelector fields (key, orEqual, offset) plus penalty.
// C++ GetKeyReply contains a KeySelector (key + orEqual + offset), not just a key.
func parseGetKeyReply(data []byte) (key []byte, orEqual bool, offset int32, penalty float64, err error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, false, 0, -1.0, fmt.Errorf("GetKey: %w", err)
	}
	penalty = -1.0
	if r.FieldPresent(types.GetKeyReplySlotPenalty) {
		penalty = r.ReadFloat64(types.GetKeyReplySlotPenalty)
	}
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. RFC-010 #1.
	if ferr := wire.ReadInlineReplyError(&r, types.GetKeyReplySlotError); ferr != nil {
		return nil, false, 0, penalty, ferr
	}
	// Navigate into the KeySelector nested struct to extract all three fields.
	selR, selErr := r.ReadNestedReader(types.GetKeyReplySlotSel)
	if selErr != nil {
		return nil, false, 0, penalty, fmt.Errorf("read KeySelector: %w", selErr)
	}
	key = selR.ReadBytes(types.KeySelectorRefSlotKey)
	if selR.FieldPresent(types.KeySelectorRefSlotOrEqual) {
		orEqual = selR.ReadBool(types.KeySelectorRefSlotOrEqual)
	}
	if selR.FieldPresent(types.KeySelectorRefSlotOffset) {
		offset = selR.ReadInt32(types.KeySelectorRefSlotOffset)
	}
	return key, orEqual, offset, penalty, nil
}

// getValue sends a GetValueRequest to the appropriate storage server.
// Returns the value (nil if key not found), or an error.
// wrong_shard_server is retried locally with cache invalidation.
// A reply timeout (errReplyTimeout — a slow-but-alive storage server) is
// re-sent without invalidating the location (libfdb_c has no per-read client
// timeout and waits for the reply; see maxReadTimeoutRetries). Other FDB errors (transaction_too_old,
// etc.) are returned to the caller for handling by the Transact retry loop.
func (tx *Transaction) getValue(parentCtx context.Context, key []byte) ([]byte, error) {
	return tx.getValueSampled(parentCtx, key, nil)
}

// getValueSampled is getValue whose first request carries an earlier draw:
// a pipelined read continued after a failure loadBalance retries itself.
func (tx *Transaction) getValueSampled(parentCtx context.Context, key []byte, carried *readTagSample) ([]byte, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	if sp := tx.startOpSpan("fdbgo.getValue"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getValue)
		defer sp.End()
	}
	start := time.Now()
	v, err := tx.getValueImpl(ctx, key, carried)
	if err == nil && tx.db != nil {
		// RFC-114: GetValue read latency (C++ readLatencies, NativeAPI.actor.cpp:3698),
		// sampled on the successful reply only. Divergence (documented in RFC-114):
		// `start` is taken before getValueImpl, so on the cold path this span includes
		// the locate + any wrong-shard retry loop, whereas C++ resets startTimeD per
		// attempt (:3660) and measures only the final physical-read RPC. Identical on
		// the common single-RPC happy path; Go over-measures under a wrong-shard storm.
		tx.db.metrics.observeReadLatency(time.Since(start))
	}
	return v, tx.mapReadError(ctx, err)
}

func (tx *Transaction) getValueImpl(ctx context.Context, key []byte, carried *readTagSample) ([]byte, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	timeoutRetries := 0
	var tags readTagSample
	redraw := carried == nil
	if carried != nil {
		tags = *carried
	}
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.currentSpan(), false)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		if redraw {
			tags = tx.sampleReadTags() // GetValueRequest per loop iteration (NativeAPI.actor.cpp:3681)
		}
		val, err := tx.sendGetValue(ctx, key, loc.Servers, tags)
		if err == nil {
			return val, nil
		}
		redraw = readTagRetry(err)
		// Reply timeout: the location is fine, the server was slow. Re-send
		// (no invalidate, no attempt-count charge) up to a bounded cap, then
		// surface a RETRYABLE transaction_too_old so the Transact loop retries
		// the whole transaction with a fresh read version — the observable
		// libfdb_c outcome. errReplyTimeout itself must never escape.
		if isReplyTimeout(err) {
			timeoutRetries++
			if timeoutRetries > maxReadTimeoutRetries {
				return nil, &wire.FDBError{Code: ErrTransactionTooOld}
			}
			attempts-- // a slow server is not a wrong-shard attempt
			if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
				return nil, err
			}
			continue
		}
		// wrong_shard_server or all_alternatives_failed → invalidate cache, retry.
		// maybeDelivered (1030/1100, a connection teardown under the in-flight
		// RPC) joins them: C++ loadBalance retries another alternative and
		// exhausts to all_alternatives_failed — same absorption, same bound.
		if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
			tx.db.locCache.invalidate(key, tx.tenantId, false)
			if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
				return nil, err
			}
			continue
		}
		// Other FDB error → bubble up for Transact retry.
		return nil, err
	}
	// Exhausted the wrong-shard retry budget (a wrong_shard_server storm or
	// repeated all_alternatives_failed): surface a RETRYABLE transaction_too_old
	// (libfdb_c never propagates all_alternatives_failed to the application — it
	// retries the read; a bounded client surfaces the transaction-level retry
	// instead).
	return nil, &wire.FDBError{Code: ErrTransactionTooOld}
}

func (tx *Transaction) sendGetValue(ctx context.Context, key []byte, servers []ServerInfo, tags readTagSample) ([]byte, error) {
	servers, ok := tx.db.failMon.liveServers(servers, EndpointGetValue)
	if !ok {
		return nil, tx.db.allAlternativesFailed(ctx)
	}
	// Pick best + second-best for speculative hedge.
	bestIdx, secondIdx := tx.db.queueModel.chooseTopTwo(servers)
	if !tx.db.hedgeEnabled.Load() {
		secondIdx = -1 // disable hedge
	}

	// Capture tx fields before building closures. The makeSender closures may
	// execute in goroutines (via hedge), and postCommitReset can clear these
	// fields concurrently if a Watch goroutine races with Commit.
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	// Build a sender closure for a given server.
	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			defer permit.Release()
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(server.Token)
			replyCleanup := replyGuard{handle: replyHandle}
			defer replyCleanup.close()
			body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, span, tags, replyToken, server.Token)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, server.Token, body, replyHandle, permit, nil); err != nil {
				getValueBufPool.Put(poolBuf) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyCleanup.release()
				tx.db.handleReadConnError(server.Address, conn, err)
				return inFlightRPC{err: err, addr: server.Address}
			}
			getValueBufPool.Put(poolBuf)
			replyCleanup.transfer()
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
				conn:        conn,
				delta:       delta,
				start:       start,
			}
		}
	}

	primary := makeSender(servers[bestIdx])
	var secondary sendFunc
	if secondIdx >= 0 {
		secondary = makeSender(servers[secondIdx])
	}

	hedgeDelay := tx.db.queueModel.secondDelay(servers[bestIdx].Address)
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, tx.readRPCTimeout())

	// End every started request that did not become the winner (hedge losers;
	// both arms on timeout/cancel) exactly once, else its QueueModel delta leaks
	// permanently and biases server selection. RFC-010 #5.
	for _, o := range result.others {
		tx.db.endOther(o)
	}

	// Process result.
	if result.addr != "" {
		if result.connErr {
			tx.db.handleReadConnError(result.addr, result.conn, result.err)
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		}
	}
	if result.err != nil {
		// Hedge failed — fall back to remaining servers sequentially (see resolveFallback).
		return resolveFallback(ctx, result.err, servers, bestIdx, secondIdx, func(server ServerInfo) ([]byte, error) {
			return tx.sendGetValueToServer(ctx, key, server, readVersion, lockAware, tenantId, tags)
		})
	}

	val, penalty, err := parseGetValueReply(result.body)
	tx.db.noteReadBrokenPromise(err, servers, result.addr, EndpointGetValue)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return val, err
}

// sendGetValueToServer sends a getValue RPC to a single specific server.
// Used as fallback after hedge fails.
func (tx *Transaction) sendGetValueToServer(ctx context.Context, key []byte, server ServerInfo, readVersion int64, lockAware bool, tenantId int64, tags readTagSample) ([]byte, error) {
	conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
	if err != nil {
		tx.db.handleDialError(ctx, server.Address)
		return nil, err
	}
	defer permit.Release()
	replyToken, replyCh, replyHandle := conn.PrepareReplyTo(server.Token)
	replyCleanup := replyGuard{handle: replyHandle}
	defer replyCleanup.close()
	body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, childSpanContext(tx.currentSpan()), tags, replyToken, server.Token)

	delta := tx.db.queueModel.startRequest(server.Address)
	start := time.Now()

	if err := sendReadFrame(ctx, conn, server.Token, body, replyHandle, permit, nil); err != nil {
		getValueBufPool.Put(poolBuf) // the write queue owns its copy
		tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
		replyHandle.Cancel()
		tx.db.handleReadConnError(server.Address, conn, err)
		return nil, err
	}
	getValueBufPool.Put(poolBuf)
	resp, err := waitReply(replyCh, ctx, tx.readRPCTimeout())
	if err != nil {
		tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
		replyHandle.Cancel()
		return nil, err
	}
	replyCleanup.release()
	if resp.Err != nil {
		tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(resp.Err, start), false)
		tx.db.handleReadConnError(server.Address, conn, resp.Err)
		return nil, resp.Err
	}
	val, penalty, parseErr := parseGetValueReply(resp.Body)
	tx.db.noteBrokenPromise(parseErr, server.Address, server.Token)
	tx.db.queueModel.endRequestFull(server.Address, delta, time.Since(start), parseErr == nil, isFutureVersionOrProcessBehind(parseErr), penalty)
	return val, parseErr
}

// getRange reads a key range [begin, end), fetching all overlapping shard locations
// at once and iterating them in scan order. Matches C++ getExactRange in
// NativeAPI.actor.cpp: re-queries same shard on more=true (no re-locate),
// invalidates entire remaining range on wrong_shard_server, and passes reverse
// to getKeyRangeLocations so the proxy returns shards in the right order.
func (tx *Transaction) getRange(parentCtx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	kvs, more, _, err := tx.getRangeReadThrough(parentCtx, begin, end, limit, byteTarget, reverse)
	return kvs, more, err
}

// getRangeReadThrough is getRange that also reports readThrough: where a read that
// stopped after its first shard's rows finished that shard (NativeAPI.actor.cpp:4795-4799).
func (tx *Transaction) getRangeReadThrough(parentCtx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
	// Once per call, before GRV: C++ Transaction::getRangeInternal (NativeAPI.actor.cpp:5808).
	tx.db.metrics.getRangeRequests.Add(1)
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	readVersion, err := tx.operationReadVersion(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	if sp := tx.startOpSpan("fdbgo.getRange"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getRange)
		defer sp.End()
	}
	kvs, more, readThrough, err := tx.getRangeImpl(ctx, begin, end, newRangeLimits(limit, byteTarget), readVersion, reverse)
	return kvs, more, readThrough, tx.mapReadError(ctx, err)
}

// RangeMaterializationLimitError is returned by a GetRange that would materialize
// more than the opt-in WithRangeByteCeiling cap (RFC-115 §2) — an OOM safety valve,
// off by default. libfdb_c has no such ceiling (its GetSliceWithError equivalent also
// materializes unbounded and never returns a "too big" error), so this NEVER fires
// unless the operator opts in via WithRangeByteCeiling; the default facade behavior
// stays oracle-matching. Match it with errors.As. For large scans, prefer the bounded,
// StreamingMode-honoring Iterator() instead of raising the ceiling.
type RangeMaterializationLimitError struct {
	LimitBytes   int64 // the configured WithRangeByteCeiling
	ReachedBytes int64 // total key+value bytes materialized when the cap was exceeded
}

func (e *RangeMaterializationLimitError) Error() string {
	return fmt.Sprintf("fdbgo: GetRange materialized %d bytes, exceeding the configured WithRangeByteCeiling of %d bytes; use Iterator() for large/unbounded scans",
		e.ReachedBytes, e.LimitBytes)
}

// rangeScanOps supplies the only four things the shard-walking scan loop needs
// to know about its element type. C++ reaches the same factoring through
// templates: getExactRange is one templated ACTOR over GetKeyValuesRequest /
// GetMappedKeyValuesRequest (NativeAPI.actor.cpp), and there is exactly one
// shared getRangeFallback rather than a mapped-specific copy. Keeping ONE loop
// here means the shard-boundary walk, the wrong_shard_server relocate, the
// reply-timeout re-send, the reverse "fix more" heuristic and the limit
// semantics cannot drift between the plain and mapped paths — which is the
// whole reason this is generic rather than duplicated.
type rangeScanOps[T any] struct {
	// send issues one shard-local request at the scan's limits and read version
	// and parses its reply.
	send func(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample) ([]T, bool, error)
	// key returns the PRIMARY key of an element. For a mapped row this is the
	// primary (index) row's key, never a secondary key: it drives shard
	// re-query bounds, which are bounds in the primary range only.
	key func(*T) []byte
	// sizeBytes is the element's contribution to the WithRangeByteCeiling
	// accounting. Called only when that opt-in ceiling is armed.
	sizeBytes func(*T) int64
	// limitBytes is C++ GetRangeLimits::decrement's charge against a byte target
	// (NativeAPI.actor.cpp:2811-2853): the primary row's key and value plus 8.
	limitBytes func(*T) int
}

var plainRangeScanOps = rangeScanOps[KeyValue]{
	key:        func(kv *KeyValue) []byte { return kv.Key },
	sizeBytes:  func(kv *KeyValue) int64 { return int64(len(kv.Key) + len(kv.Value)) },
	limitBytes: func(kv *KeyValue) int { return 8 + len(kv.Key) + len(kv.Value) },
}

func (tx *Transaction) getRangeImpl(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool) ([]KeyValue, bool, []byte, error) {
	ops := plainRangeScanOps
	ops.send = tx.sendGetRange
	return rangeScanImpl(tx, ctx, begin, end, limits, readVersion, reverse, ops)
}

// rangeScanImpl is NativeAPI getExactRange over [begin, end), consuming limits as
// replies arrive (NativeAPI.actor.cpp:4255-4444). Every request carries readVersion.
func rangeScanImpl[T any](tx *Transaction, ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, ops rangeScanOps[T]) ([]T, bool, []byte, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	// C++ getExactRange locates GET_RANGE_SHARD_LIMIT (=2, ClientKnobs.cpp:101) shards at a time
	// (NativeAPI.actor.cpp:4272-4278) and the soft byte limit below depends on those pairs. Go
	// fetches more per locate RPC, always a whole number of pairs so pair origins coincide.
	const cppGetRangeShardLimit = 2
	const getRangeShardLimit = 50 * cppGetRangeShardLimit

	var allKVs []T
	var materializedBytes int64 // RFC-115 §2: bound total materialized bytes vs WithRangeByteCeiling
	byteLimited := limits.hasByteLimit()
	curBegin := begin
	curEnd := end
	relocateRetries := 0
	timeoutRetries := 0
	// C++ getRange's own loop, before any getRangeFallback, reports readThrough when the
	// first reply's rows end a shard the range continues past (NativeAPI.actor.cpp:4762,
	// 4795-4799). A byte-targeted call then stops; wrong_shard_server falls back (:4864-4870).
	firstReply := true
	var readThrough []byte
	var tags readTagSample
	redraw := true

	// SOFT BYTE LIMIT — C++ getExactRange:4415:
	//
	//	// Soft byte limit - return results early if the user specified a byte limit and we got results
	//	if (limits.hasSatisfiedMinRows() && output.size() > 0) { output.more = true; return output; }
	//
	// hasSatisfiedMinRows() is hasByteLimit() && minRows == 0 (:2875). A caller's own limits
	// start with minRows 1 (FDBTypes.h GetRangeLimits ctor), so for them ANY non-empty reply
	// satisfies it; a RYW request can ask for more (setRequestLimits). C++ checks it before
	// re-querying a shard or moving to a location pair's second shard, but relocates after the
	// second without checking (:4392-4407). A call whose first rows come from a pair's second
	// shard therefore also reads the next pair's first, unless that shard's reply was cut short;
	// getRange reaches that through its fallback from an empty first reply (:4828-4836). Each
	// request carries the bytes left.
	//
	// This is the half that makes the request's byte target OBSERVABLE. Without it the loop
	// absorbs a truncated reply and re-queries until the ROW budget fills, which is why
	// lowering the request's byte limit ALONE changes no division — a real measurement that
	// must not be read as "the request side is not involved". Both halves are required and
	// neither reproduces the other: the server's reply-size accounting is not the client's, so
	// a client-side byte budget over an untruncated reply cuts at a different row (measured: 19
	// where C cuts 18 at a 4096-byte target — libfdbc:TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget).
	//
	// EXACT falls out of this rather than being special-cased: mode_bytes_array[EXACT] is
	// BYTE_LIMIT_UNLIMITED (fdb_c.cpp:1002), so the byte limit is unlimited, this can never fire,
	// and the loop keeps absorbing across replies exactly as it did before.
	//
	// It is checked where the loop is about to fetch MORE data, never where the scan is
	// finishing. C's early return sits AFTER the shard-exhaustion block that returns
	// output.more = false (:4391-4400), so a range drained before its bytes run out still
	// reports more=false; a check placed before that would label every completed byte-limited
	// drain as having more.
	softByteStop := func() bool { return limits.hasSatisfiedMinRows() && len(allKVs) > 0 }

	for !limits.isReached() && bytes.Compare(curBegin, curEnd) < 0 {
		// Get all shard locations for current range. C++ getKeyRangeLocations
		// receives the reverse flag so the proxy returns shards in scan order.
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, curBegin, curEnd, getRangeShardLimit, reverse, tx.tenantId, tx.currentSpan())
		if err != nil {
			return nil, false, nil, fmt.Errorf("locate range: %w", err)
		}
		if len(locations) == 0 {
			return allKVs, false, readThrough, nil
		}

		// C++ getExactRange iterates shard=0,1,2,... linearly. With reverse=true
		// on the request, locations[0] is already the shard nearest end.
		relocated := false
		for shard := 0; shard < len(locations) && !limits.isReached(); shard++ {
			loc := locations[shard]

			// Clamp shard boundaries to user's requested range.
			shardBegin := loc.ShardBegin
			shardEnd := loc.ShardEnd
			if bytes.Compare(shardBegin, curBegin) < 0 {
				shardBegin = curBegin
			}
			if shardEnd == nil || bytes.Compare(shardEnd, curEnd) > 0 {
				shardEnd = curEnd
			}
			if bytes.Compare(shardBegin, shardEnd) >= 0 {
				continue // empty range for this shard
			}

			// About to fetch from a FURTHER shard on a byte-limited read that already has
			// rows. C++ reaches its soft-byte-limit return after ++shard for exactly this
			// case, so the call ends at its location pair — which is the point of the soft
			// limit: "This can prevent problems where the desired range spans many shards
			// and would be too slow to fetch entirely." A pair's first shard is fetched
			// without the check.
			if shard%cppGetRangeShardLimit != 0 && softByteStop() {
				return allKVs, true, readThrough, nil
			}

			// Inner loop: re-query same shard while more=true (C++ stays on same
			// shard index, mutates locations[shard].range in-place).
			for !limits.isReached() {
				if redraw {
					tags = tx.sampleReadTags() // a request per send (NativeAPI.actor.cpp:4303, 4684)
				}
				kvs, more, err := ops.send(ctx, shardBegin, shardEnd, limits, readVersion, reverse, loc.Servers, tags)
				redraw = err == nil || readTagRetry(err)
				if err != nil {
					// Reply timeout (a slow-but-alive server): the location is
					// fine — re-send the SAME shard (no relocate). libfdb_c has no
					// per-read client timeout and waits until the server replies or
					// the read version ages to
					// transaction_too_old). Bounded; on exhaustion surface a
					// RETRYABLE transaction_too_old so the Transact loop retries
					// with a fresh read version. errReplyTimeout never escapes.
					if isReplyTimeout(err) {
						timeoutRetries++
						if timeoutRetries > maxReadTimeoutRetries {
							return nil, false, nil, &wire.FDBError{Code: ErrTransactionTooOld}
						}
						if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
							return nil, false, nil, err
						}
						continue // re-send same shard
					}
					// maybeDelivered (1030/1100: the storage connection died
					// under the in-flight RPC) takes the same arm — C++
					// loadBalance retries another alternative and exhausts to
					// all_alternatives_failed, which this arm already absorbs.
					if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
						relocateRetries++
						if relocateRetries > maxRelocateRetries {
							// Surface a RETRYABLE transaction_too_old (not the
							// terminal all_alternatives_failed): the read path
							// absorbs all_alternatives_failed and lets the
							// transaction retry with a fresh locate, matching
							// libfdb_c's internal retry of the read.
							return nil, false, nil, &wire.FDBError{Code: ErrTransactionTooOld}
						}
						firstReply = firstReply && !isWrongShardServer(err)
						// C++ invalidates just the stale shard's range:
						// cx->invalidateCache(locations[shard].range).
						// Narrower than our previous whole-remaining-range invalidation.
						tx.db.locCache.invalidateRange(shardBegin, shardEnd, tx.tenantId)
						if reverse {
							curEnd = shardEnd
						} else {
							curBegin = shardBegin
						}
						if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
							return nil, false, nil, err
						}
						relocated = true
						break // break to outer loop for re-locate
					}
					return nil, false, nil, err
				}
				if firstReply {
					firstReply = false
					if byteLimited && len(kvs) > 0 && !more {
						if !reverse && loc.ShardEnd != nil && bytes.Compare(loc.ShardEnd, curEnd) < 0 {
							readThrough = shardEnd
						} else if reverse && bytes.Compare(loc.ShardBegin, curBegin) > 0 {
							readThrough = shardBegin
						}
					}
				}

				if len(allKVs) == 0 && len(kvs) > 0 {
					// Parsers transfer fresh descriptors; fence spare capacity before later appends.
					allKVs = kvs[:len(kvs):len(kvs)]
				} else {
					allKVs = append(allKVs, kvs...)
				}

				// RFC-115 §2: opt-in OOM ceiling. Checked AFTER each batch append so a
				// runaway unbounded scan errors instead of OOM-ing; the overshoot is at
				// most one reply (~80 KB). 0 = unlimited (default, oracle-matching).
				if ceiling := tx.db.rangeByteCeiling; ceiling > 0 {
					for i := range kvs {
						materializedBytes += ops.sizeBytes(&kvs[i])
					}
					if materializedBytes > ceiling {
						return nil, false, nil, &RangeMaterializationLimitError{LimitBytes: ceiling, ReachedBytes: materializedBytes}
					}
				}

				size := 0
				if limits.hasByteLimit() {
					for i := range kvs {
						size += ops.limitBytes(&kvs[i])
					}
				}
				limits.decrement(len(kvs), size)
				// A row or byte limit met reports more regardless of the shard's own flag
				// (:4359-4362).
				if limits.isReached() {
					return allKVs, true, readThrough, nil
				}

				// C++ "fix more" heuristic (NativeAPI.actor.cpp:2331-2333):
				// If reverse scan's last returned key equals shard begin, the
				// shard is exhausted regardless of what more says.
				if more && reverse && len(kvs) > 0 &&
					bytes.Equal(ops.key(&kvs[len(kvs)-1]), shardBegin) {
					more = false
				}

				if !more {
					break // move to next shard
				}

				// C++ ASSERT: more=true with zero rows is impossible.
				// Guard against infinite loop on misbehaving storage server.
				if len(kvs) == 0 {
					break
				}

				// About to re-query the SAME shard because the reply was truncated. On a
				// byte-limited read that truncation IS the batch boundary, so the call ends
				// here instead of absorbing it.
				if softByteStop() {
					return allKVs, true, readThrough, nil
				}

				// Narrow range and re-query same shard (C++ mutates
				// locations[shard].range in-place, lines 2349-2354).
				lastKey := ops.key(&kvs[len(kvs)-1])
				if reverse {
					shardEnd = lastKey // [shardBegin, smallestReturnedKey)
				} else {
					shardBegin = append(append([]byte{}, lastKey...), 0) // keyAfter(lastKey)
				}
			}

			if relocated {
				break
			}
		}

		if relocated {
			continue // re-locate with adjusted curBegin/curEnd
		}

		// Exhausted all locations from this batch. Update range for next locateRange call.
		if reverse {
			firstShard := locations[len(locations)-1]
			if bytes.Compare(firstShard.ShardBegin, curBegin) <= 0 {
				break // first shard covers our begin, done
			}
			curEnd = firstShard.ShardBegin
		} else {
			lastShard := locations[len(locations)-1]
			if lastShard.ShardEnd == nil || bytes.Compare(lastShard.ShardEnd, curEnd) >= 0 {
				break
			}
			curBegin = lastShard.ShardEnd
		}
		if bytes.Compare(curBegin, curEnd) >= 0 {
			break
		}
	}

	return allKVs, limits.rows == 0, readThrough, nil
}

func (tx *Transaction) sendGetRange(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample) ([]KeyValue, bool, error) {
	return sendRangeRPC(tx, ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), limits, readVersion, reverse, servers, tags, plainRangeRPCOps)
}

// plainRangeRPCOps sends a GetKeyValuesRequest.
var plainRangeRPCOps = rangeRPCOps[KeyValue]{
	endpoint: EndpointGetKeyValues,
	build:    buildGetKeyValuesRequest,
	putBuf:   func(bufp *[]byte) { getKeyValuesBufPool.Put(bufp) },
	parse:    parseGetKeyValuesReply,
}

// rangeRPCOps is the per-request-kind half of the shard-local range RPC: which
// storage endpoint to hit, how to build the request, which pool its scratch
// buffer came from, and how to parse the reply. Everything else — hedging,
// QueueModel accounting, the connection-error and reply-timeout arms — is
// identical between getKeyValues and getMappedKeyValues in C++ (both go through
// the same loadBalance), so it is shared here rather than copied. A copy is how
// the QueueModel delta-leak fix (RFC-010 #5) would end up applied to one path
// and not the other.
type rangeRPCOps[T any] struct {
	endpoint int
	build    func(begin, end KeySelector, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID, serverToken transport.UID) ([]byte, *[]byte)
	putBuf   func(*[]byte)
	parse    func([]byte) ([]T, bool, float64, error)
}

// limits.bytes is the per-fetch BYTE target left for this read (unlimited when the streaming
// mode has none). It is not the same thing as replyByteLimit: that is CLIENT_KNOBS->REPLY_BYTE_LIMIT,
// a flat ceiling on any single reply, while the target is the mode's own, usually much smaller,
// one — and it is the value the STORAGE SERVER truncates against, which is what actually
// divides a scan into batches. The request is sent at readVersion, the version the read's
// operation captured (NativeAPI.actor.cpp:4617-4618).
func sendRangeRPC[T any](tx *Transaction, ctx context.Context, begin, end KeySelector, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample, ops rangeRPCOps[T]) ([]T, bool, error) {
	// C++ transformRangeLimits, called from BOTH range loops (getExactRange:4299, getRange:4681).
	wireLimit, wireLimitBytes := transformRangeLimits(limits, reverse)
	servers, ok := tx.db.failMon.liveServers(servers, ops.endpoint)
	if !ok {
		return nil, false, tx.db.allAlternativesFailed(ctx)
	}

	bestIdx, secondIdx := tx.db.queueModel.chooseTopTwo(servers)
	if !tx.db.hedgeEnabled.Load() {
		secondIdx = -1
	}

	// Capture tx fields before closures (see sendGetValue comment).
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			gkvToken := getAdjustedEndpoint(server.Token, ops.endpoint)
			defer permit.Release()
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(gkvToken)
			replyCleanup := replyGuard{handle: replyHandle}
			defer replyCleanup.close()
			body, poolBuf := ops.build(begin, end, readVersion, wireLimit, wireLimitBytes, lockAware, tenantId, span, tags, replyToken, server.Token)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, gkvToken, body, replyHandle, permit, nil); err != nil {
				ops.putBuf(poolBuf) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyCleanup.release()
				tx.db.handleReadConnError(server.Address, conn, err)
				return inFlightRPC{err: err, addr: server.Address}
			}
			ops.putBuf(poolBuf)
			replyCleanup.transfer()
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
				conn:        conn,
				delta:       delta,
				start:       start,
			}
		}
	}

	primary := makeSender(servers[bestIdx])
	var secondary sendFunc
	if secondIdx >= 0 {
		secondary = makeSender(servers[secondIdx])
	}

	hedgeDelay := tx.db.queueModel.secondDelay(servers[bestIdx].Address)
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, tx.readRPCTimeout())

	// End every started request that did not become the winner (hedge losers;
	// both arms on timeout/cancel) exactly once, else its QueueModel delta leaks
	// permanently and biases server selection. RFC-010 #5.
	for _, o := range result.others {
		tx.db.endOther(o)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleReadConnError(result.addr, result.conn, result.err)
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, readFailureLatency(result.err, result.start), false)
		}
	}
	if result.err != nil {
		return nil, false, result.err
	}

	kvs, more, penalty, err := ops.parse(result.body)
	tx.db.noteReadBrokenPromise(err, servers, result.addr, ops.endpoint)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return kvs, more, err
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
// coded 1030 for every connection teardown, so this is how a read loop
// recognizes "the connection under my in-flight RPC died". For reads
// (AtMostOnce::False) C++ retries another alternative and, on exhaustion,
// surfaces all_alternatives_failed to the read's own retry loop — never the raw
// teardown code to the application. The Go read loops mirror that by treating
// maybeDelivered exactly like all_alternatives_failed: invalidate the location,
// bounded retry, and on exhaustion a RETRYABLE transaction_too_old. The commit
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
// with the false return at :377) and must not be remembered as a timeout. When every replica is torn
// down the default arm flattens to all_alternatives_failed, which getValueImpl absorbs via
// invalidate+relocate — the same exhaustion outcome as C++ loadBalance.
func resolveFallback(ctx context.Context, hedgeErr error, servers []ServerInfo, bestIdx, secondIdx int, trySingle func(ServerInfo) ([]byte, error)) ([]byte, error) {
	// A cancelled/expired CALLER context propagates IMMEDIATELY — a cancelled read must return ctx.Err(),
	// never a remembered version error or a flattened timeout. Gate on ctx.Err() (the real READ context),
	// NOT errors.Is on a per-server error: a cold dial / RPC timeout to an unreachable replica surfaces as
	// a wrapped context.DeadlineExceeded under the db-scoped DefaultRPCTimeout while the read context is
	// still LIVE, and that must fall back to other replicas rather than abort the scan. Checked at
	// entry (covers the no-untried-fallback case) and after each replica in the loop below.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sawTimeout := isServerTimeout(hedgeErr)
	var versionErr error
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
		}
	}
	switch {
	case versionErr != nil:
		return nil, versionErr
	case sawTimeout:
		// Prefer the timeout (getValue re-sends without a pointless cache invalidation — the location
		// is fine, the servers were slow) over flattening to all_alternatives_failed.
		return nil, errReplyTimeout
	default:
		return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
	}
}

// isServerTimeout reports whether a per-replica error is a slow/unreachable-server TIMEOUT — either an RPC
// reply timeout (errReplyTimeout) or a wrapped context.DeadlineExceeded from a cold dial / RPC under the
// db-scoped DefaultRPCTimeout. resolveFallback calls this only AFTER confirming the caller's read context
// is still live (ctx.Err()==nil), so a DeadlineExceeded here is a per-server deadline, never caller
// cancellation. Such a timeout must be remembered as a timeout — surfaced as errReplyTimeout so getValue
// re-sends to the same location — NOT dropped and flattened to all_alternatives_failed (1006), which
// getValueImpl absorbs via the wrong-shard invalidate+relocate path, the wrong retry response to a merely
// slow server.
func isServerTimeout(err error) bool {
	return isReplyTimeout(err) || errors.Is(err, context.DeadlineExceeded)
}

func buildGetKeyValuesRequest(begin, end KeySelector, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyValuesRequest{
		Begin:                  begin.ref(),
		End:                    end.ref(),
		Version:                version,
		Limit:                  limit,
		LimitBytes:             limitBytes,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span, // RFC-115 §4
		SsLatestCommitVersions: emptyVersionVector,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if lockAware {
		req.HasOptions = true
		req.Options = lockAwareReadOptions()
	}
	bufp := getKeyValuesBufPool.Get().(*[]byte)
	result := req.MarshalFDBPooled(*bufp)
	*bufp = result
	return result, bufp
}

// parseGetKeyValuesReply parses the ErrorOr-wrapped GetKeyValuesReply.
// Returns (keyValues, more, penalty, error).
func parseGetKeyValuesReply(data []byte) ([]KeyValue, bool, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, false, -1.0, fmt.Errorf("GetKeyValues: %w", err)
	}
	var reply types.GetKeyValuesReply
	reply.UnmarshalFromReader(&r)
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. RFC-010 #1.
	if ferr := wire.ReadInlineReplyError(&r, types.GetKeyValuesReplySlotError); ferr != nil {
		return nil, false, reply.Penalty, ferr
	}

	kvs := types.ParseKeyValueRefStringVector(reply.Data)
	return kvs, reply.More, reply.Penalty, nil
}

// KeyValue is a key-value pair returned from reads.
type KeyValue = types.KeyValueRef

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

func buildGetValueRequest(key []byte, version int64, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span, // RFC-115 §4
		SsLatestCommitVersions: emptyVersionVector,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if lockAware {
		req.HasOptions = true
		req.Options = lockAwareReadOptions()
	}
	bufp := getValueBufPool.Get().(*[]byte)
	result := req.MarshalFDBPooled(*bufp)
	*bufp = result
	return result, bufp
}

// parseGetValueReply parses the ErrorOr-wrapped GetValueReply.
func parseGetValueReply(data []byte) ([]byte, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, -1.0, fmt.Errorf("GetValue: %w", err)
	}
	var reply types.GetValueReply
	reply.UnmarshalFromReader(&r)
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. Decoded from the nested
	// Error table (the generated reply.Error is mis-typed). RFC-010 #1.
	if ferr := wire.ReadInlineReplyError(&r, types.GetValueReplySlotError); ferr != nil {
		return nil, reply.Penalty, ferr
	}
	if !reply.HasValue {
		return nil, reply.Penalty, nil // key not found
	}
	return reply.Value, reply.Penalty, nil
}

// Watch watches a key for changes. The server holds the connection open until
// the watched key's value changes from the version observed by this transaction.
// Returns nil when the key has changed, or an error on failure.
//
// Matches C++ NativeAPI.actor.cpp watchValueMap/watchValue flow: locate storage
// server, send WatchValueRequest with current read version, long-poll for
// WatchValueReply. Retries on wrong_shard_server with cache invalidation.
//
// The watch is a long-poll: there is no short timeout. The context's deadline
// (if any) controls the maximum wait time.
// RFC-170 (#8) note: this SYNCHRONOUS client API registers the watch at the READ version — it blocks in
// WatchPoll until the watch fires, so it cannot wait for a commit that (by construction) can only happen
// AFTER it returns. The committed-version deferral (register post-commit) is done by the ASYNC fdb facade
// Watch (which returns a future and blocks that future's goroutine on the commit-completion signal). Callers
// of this sync API that need committed-version semantics must commit before/around the watch (C++
// setupWatches' committedVersion>0?committedVersion:readVersion applies at the facade, not here).
func (tx *Transaction) Watch(ctx context.Context, key []byte) error {
	value, readVersion, span, watchCtx, watchCancel, err := tx.WatchSetup(ctx, key)
	if err != nil {
		return err
	}
	// Nothing commits before this poll: it carries the tags captured at setup.
	return tx.watchPoll(watchCtx, watchCancel, key, value, readVersion, span, tx.watchSetupTags(watchCtx))
}

// WatchSetup performs the SYNCHRONOUS part of a watch: pin the read version, add
// the read conflict, and read the value to watch — all at the transaction's read
// version. It returns BOTH the value bytes and the read version the watch must be
// registered against.
//
// This MUST run within the watching transaction's active window (e.g. directly
// from Transaction.Watch, not from a detached goroutine that races later
// mutations). Two things are captured here, both for the same reason:
//   - the VALUE: if read late — after some other transaction already changed the
//     key — the storage server would be told to watch the *new* value, see the
//     current value already equals it, and never fire (a silent 10s-timeout flake);
//   - the READ VERSION: the async WatchPoll's sendWatch must NOT read tx.readVersion
//     later, because the common `w := tr.Watch(k)` inside Database.Transact pattern
//     commits and postCommitReset()s the transaction (readVersion → 0) before the
//     future goroutine runs — sending the watch at version 0, which can error or
//     register incorrectly. So the read version is captured synchronously here and
//     threaded through to sendWatch.
func (tx *Transaction) WatchSetup(ctx context.Context, key []byte) ([]byte, int64, types.SpanContext, context.Context, context.CancelFunc, error) {
	callerCtx := ctx
	ctx, release := tx.opContext(ctx)
	defer release()
	if err := tx.readEntryError(ctx); err != nil {
		return nil, 0, types.SpanContext{}, nil, nil, err
	}
	// Deferred failure was captured before RYW/resetPromise dispatch. Lifetime
	// failure still precedes watch options, key validation and cap acquisition.
	if cerr := tx.checkCancelled(); cerr != nil {
		return nil, 0, types.SpanContext{}, nil, nil, cerr // transaction_cancelled (1025)
	}
	if cerr := tx.readLifetimeError(ctx); cerr != nil {
		return nil, 0, types.SpanContext{}, nil, nil, cerr
	}
	// C++ RYW::watch: watches are disabled when RYW is disabled
	// (ReadYourWrites.actor.cpp:2448-2449). Returns watches_disabled (1034).
	if tx.rywDisabled {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 1034} // watches_disabled
	}
	// Eager legal-range + key-size validation, BEFORE the read — C++ RYW watch
	// (ReadYourWrites.actor.cpp:2450-2456): a key >= getMaxReadKey() (and != metadataVersionKey) is
	// key_outside_legal_range (2004), an oversized key is key_too_large (2102). Without this a normal
	// (non-system) transaction could register a watch on a \xff system key or an oversized key that
	// libfdb_c rejects. Mirrors the Get/Set legal-range + commit key-size gates.
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
	if len(key) > getMaxWriteKeySize(key, rawAccess) {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 2102} // key_too_large
	}

	// Reserve the outstanding-watch slot HERE — synchronously, at registration order — matching C++
	// Transaction::watch, which calls increaseWatchCounter at watch() time (NativeAPI.actor.cpp:5694),
	// NOT in the async poll. Charging it inside the async WatchPoll goroutine let two Watch() calls
	// under MAX_WATCHES=1 race for the slot, so the first-registered watch could lose to the
	// second. Released by WatchPoll on the success path, or on a setup error below (matching C++'s
	// catch → decreaseWatchCounter, NativeAPI:5679). Charged AFTER the malformed-key rejections above
	// so an illegal/oversized-key watch never holds a slot.
	if err := tx.db.tryAcquireWatch(); err != nil {
		return nil, 0, types.SpanContext{}, nil, nil, err // too_many_watches (1032)
	}
	// The watch is in `reading` from here until setup ends (ReadYourWrites.actor.cpp:1290),
	// and its read's conflict is classified against the write map as of this call (:382).
	defer tx.leaveIssuedRead(tx.enterIssuedRead(ctx))
	addConflict := tx.readConflictForKeyRYW(key)

	// Mint THIS watch's own cancellable context HERE — right after the acquire and BEFORE the blocking
	// GRV / value read below. Those reads RUN on watchCtx (below), so a Cancel()/reset() during
	// setup cancels THIS context and the setup-error path releases the slot at once; a Cancel during the
	// async poll is observed by WatchPoll, which drains and releases. watchCancel is the SCOPED cancel
	// (cancels+deregisters only this watch): the setup-error paths below call it, WatchPoll defers it on
	// completion, and the fdb facade wires the Watch future's Cancel() to it — so one watch is freed
	// without touching siblings. The async WatchPoll USES this context, never a lazy fetch
	// (which races Cancel/reset — RFC-115 §4).
	watchCtx, watchCancel := tx.newWatchCtx(callerCtx)
	setupCtx, setupCancel := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(watchCtx, setupCancel)
	defer func() { stopWatch(); setupCancel() }()

	// The blocking setup reads (GRV here, value read below) run on watchCtx — NOT the caller ctx — so a
	// Cancel()/reset() DURING setup (which cancels watchCtx via cancelWatches) unblocks the read and
	// releases the reserved slot PROMPTLY, matching C++'s watch actor: its setup waits are inside a
	// `catch (Error& e){ cx->decreaseWatchCounter(); throw; }`, so cancelling the watch actor on a txn
	// reset decrements the watch counter at once (NativeAPI.actor.cpp:5637-5682). watchCtx is a CHILD of
	// ctx, so the caller's own ctx cancellation still propagates. Using the caller ctx here instead
	// (the bug) left the slot charged until the caller ctx / RPC timeout when a stuck read raced a
	// Cancel — a starve under a low MAX_WATCHES. A Cancel that ran BEFORE the mint above (its
	// cancelWatches was a no-op) is caught by ensureReadVersion's leading checkCancelled.
	if err := tx.ensureReadVersion(setupCtx); err != nil {
		tx.db.releaseWatch()
		watchCancel() // setup failed — cancel+deregister THIS watch's context
		return nil, 0, types.SpanContext{}, nil, nil, tx.watchSetupErr(err)
	}
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	// Capture the trace span here, SYNCHRONOUSLY — same reason as readVersion: the async
	// WatchPoll must NOT read the tx span later, because a `w := tr.Watch(k)` inside
	// Database.Transact commits and postCommitReset()s the tx (regenerateSpan rotates
	// the span) before the future goroutine runs — a lazy read would stamp the NEXT
	// incarnation's span on this watch (RFC-115 §4). Threaded through to sendWatch.
	span := tx.currentSpan()

	// Read current value so we can send it with the watch request.
	// C++ getValueOrStandby in watchValue actor reads the value at the watch version.
	// NOT tracked in readErr: the C++ watch actor reading.add's its `done`
	// future (ReadYourWrites.actor.cpp:1290), but every error path sends
	// done.send(Void()) BEFORE rethrowing (:1299-1302, :1325-1329) — done
	// completes successfully, so a failed watch read never poisons commit;
	// reading only barriers on watch-setup completion (RFC-098): the C++ source
	// deliberately excludes watch errors.
	value, err := tx.ryw.get(setupCtx, key, tx.getValue)
	if err != nil {
		tx.db.releaseWatch() // setup failed after the slot was reserved (C++ catch → decreaseWatchCounter)
		watchCancel()        // cancel+deregister THIS watch's context
		return value, readVersion, span, nil, nil, tx.watchSetupErr(err)
	}
	// Only a successful read adds its conflict (ReadYourWrites.actor.cpp:384-388). A failed watch
	// read does not fail commit, so a conflict added before it would outlive it.
	if addConflict {
		if err := tx.addReadConflictsFor(ctx, [][2][]byte{{key, keyAfterBytes(key)}}); err != nil {
			tx.db.releaseWatch()
			watchCancel()
			return value, readVersion, span, nil, nil, err
		}
	}
	// watchCtx/watchCancel were minted synchronously above (before this blocking read), so a
	// Cancel/reset during setup cancels the very context the read (above) and WatchPoll hold; WatchPoll
	// defers watchCancel to deregister on completion, and the facade wires the future's Cancel() to it.
	return value, readVersion, span, watchCtx, watchCancel, nil
}

// watchSetupErr maps a setup-read failure to the faithful terminal error. The blocking GRV / value
// read run on watchCtx (a child of the caller ctx); a Cancel()/reset() during setup cancels watchCtx
// via cancelWatches, so the read surfaces a context cancellation. Only THAT is reinterpreted: when the
// txn itself was Cancel()ed, the cancellation out-ranks the raw ctx error as transaction_cancelled
// (1025) — matching what C++'s watch read observes after resetPromise.sendError(transaction_cancelled)
// fires (ReadYourWrites.actor.cpp:1284-1333). A caller-ctx cancellation (txn not cancelled) is
// returned as-is, and a GENUINE read error (an FDBError, a decode failure) is NEVER masked by 1025
// even if a Cancel raced in just after it — whichever surfaced first wins, as in C++.
func (tx *Transaction) watchSetupErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if cerr := tx.checkCancelled(); cerr != nil {
			return cerr // the watchCtx cancellation was a txn Cancel → transaction_cancelled (1025)
		}
	}
	return err
}

// WatchPoll performs the ASYNCHRONOUS long-poll part of a watch: locate the
// storage server and wait for the WatchValueReply that fires when key's value
// differs from `value` (captured by WatchSetup), registered at `readVersion`
// (also captured by WatchSetup — NOT re-read from the possibly-reset transaction).
// Retries on wrong_shard_server with cache invalidation. Intended to run in the
// watch future's goroutine.
//
// Its requests carry the read tags as of the commit that activated the watch
// (setupWatches, NativeAPI.actor.cpp:6418-6432), or those captured at setup
// when no commit activated it.
func (tx *Transaction) WatchPoll(watchCtx context.Context, watchCancel context.CancelFunc, key, value []byte, readVersion int64, span types.SpanContext) error {
	return tx.watchPoll(watchCtx, watchCancel, key, value, readVersion, span, tx.watchCommitTags(watchCtx))
}

// watchPoll sends tags, the watch's parameters->tags, never the live transaction's.
func (tx *Transaction) watchPoll(watchCtx context.Context, watchCancel context.CancelFunc, key, value []byte, readVersion int64, span types.SpanContext, tags []byte) error {
	tenantID := tx.watchTenant(watchCtx)
	// watchCtx is captured SYNCHRONOUSLY by WatchSetup and passed in (NOT fetched here via
	// newWatchCtx): this runs in the async watch future, so a lazy fetch would race
	// Cancel()/reset()'s cancelWatches on watchCtx/watchCancel AND, if cancelWatches won, leak a
	// never-cancelled poll. Binding at setup makes Reset()/Cancel() cancel the very context this
	// loop holds — matching C++ resetRyow() → resetPromise.sendError(transaction_cancelled). span
	// is likewise captured synchronously by WatchSetup (a lazy read here would stamp a later
	// incarnation's regenerated span on this watch, RFC-115 §4).

	// The outstanding-watch slot was reserved SYNCHRONOUSLY in WatchSetup (registration order, C++
	// increaseWatchCounter at watch() time). Release it on EVERY exit path of this poll — fire /
	// error / cancel — matching C++ decreaseWatchCounter in the async watch actor (NativeAPI:5679).
	// tx.db is always set on a WatchPoll (the locate/sendWatch below dereference it), so no nil guard.
	defer tx.db.releaseWatch()
	// Deregister THIS watch's scoped context on completion (fire / error / cancel), so the per-txn
	// watchCancels map self-cleans across a reused post-commit handle. Idempotent with the future's
	// Cancel() and the cancelWatches path. Production callers always pass a non-nil watchCancel (from
	// WatchSetup's success path), but guard defensively so a future caller on an error path can't panic
	// the async watch goroutine.
	if watchCancel != nil {
		defer watchCancel()
	}

	// The WatchValueRequest carries a CHILD of the tx span, derived ONCE here and
	// reused across the wrong-shard retry loop — matching C++ watchValue's single
	// `state Span span("NAPI:watchValue", parameters->spanContext)` (NativeAPI.actor.cpp:3933),
	// whose span.context is stamped on the request (:3965). locate below is passed the
	// raw tx span (C++ hands getKeyLocation parameters->spanContext, not the watch child).
	watchSpan := childSpanContext(span)

	// A watch is a long-lived poll: it loops until the value changes (sendWatch returns nil), the
	// watch context is cancelled (Reset/Cancel/timeout), or a non-retryable error surfaces. Mirrors
	// C++ watchValue's `loop { ... } catch` (NativeAPI.actor.cpp:3993-4012). Only wrong_shard /
	// all_alternatives is bounded (a relocate storm); the SS poll-signals below are unbounded — the
	// watch keeps polling, as C++ does.
	wrongShardRetries := 0
	for {
		if err := watchCtx.Err(); err != nil {
			return err
		}
		loc, locErr := tx.db.locCache.locate(tx.db, watchCtx, key, tenantID, span, false)
		if locErr != nil {
			return fmt.Errorf("locate key: %w", locErr)
		}
		if len(loc.Servers) == 0 {
			return fmt.Errorf("no storage servers for key")
		}

		// A WatchValueRequest per iteration (NativeAPI.actor.cpp:3970).
		watchErr := tx.sendWatch(watchCtx, key, value, readVersion, watchSpan, loc.Servers, tx.db.sampleTagSet(tags))
		if watchErr == nil {
			return nil
		}
		delay, retryable, invalidate := classifyWatchError(watchErr)
		if !retryable {
			return watchErr
		}
		if invalidate {
			// wrong_shard / all_alternatives: transient relocate; bounded (a wrong-shard storm
			// shouldn't spin forever — the unbounded poll signals below reset this budget).
			wrongShardRetries++
			if wrongShardRetries > MaxWrongShardRetries {
				return &wire.FDBError{Code: ErrAllAlternativesFailed}
			}
			tx.db.locCache.invalidate(key, tenantID, false)
		} else {
			wrongShardRetries = 0 // a completed round-trip clears the relocate budget
		}
		if err := sleepCtx(watchCtx, delay); err != nil {
			return err
		}
	}
}

// classifyWatchError decides how WatchPoll handles a watch RPC error, mirroring C++ watchValue's
// catch arms (NativeAPI.actor.cpp:3993-4012):
//   - wrong_shard_server / all_alternatives_failed → re-locate after WRONG_SHARD_SERVER_DELAY
//     (invalidate=true, the only BOUNDED retry).
//   - watch_cancelled (1029, SS watch limit exceeded) / process_behind (1037) → poll after
//     WATCH_POLLING_TIME. Unbounded — a watch is a long-lived poll.
//   - timed_out (1004, the SS occasionally times out a watch) / future_version (1009, the SS hasn't
//     caught up to the watch's read version) → re-arm after FUTURE_VERSION_RETRY_DELAY. Unbounded.
//     (C++ retries future_version in the SS-response actor, watchStorageServerResp; Go's flatter
//     WatchPoll handles it here.)
//   - anything else → terminal (retryable=false).
func classifyWatchError(err error) (delay time.Duration, retryable, invalidate bool) {
	switch {
	case isWrongShardServer(err) || isAllAlternativesFailed(err):
		return wrongShardRetryDelay, true, true
	case isWatchErrorCode(err, ErrWatchCancelled) || isWatchErrorCode(err, ErrProcessBehind):
		return watchPollingTime, true, false
	case isWatchErrorCode(err, ErrTimedOut) || isWatchErrorCode(err, ErrFutureVersion):
		return futureVersionDelay, true, false
	default:
		// Terminal: surface immediately. C++ watchValue delays FUTURE_VERSION_RETRY_DELAY before
		// re-throwing (NativeAPI.actor.cpp:4010) — a PREVENT_FAST_SPIN guard for its actor loop. Go's
		// WatchPoll returns the error to the caller (the watch is done; no spin), so the delay is
		// unnecessary and deliberately omitted.
		return 0, false, false
	}
}

// isWatchErrorCode reports whether err is an FDBError with the given code.
func isWatchErrorCode(err error, code int) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == code
}

// buildWatchValueRequest constructs a WatchValueRequest. `span` is the watchValue
// CHILD span (derived once in WatchPoll, reused across the wrong-shard retry loop) —
// stamped verbatim, matching C++ WatchValueRequest(span.context,…) (NativeAPI.actor.cpp:3965).
func buildWatchValueRequest(key, value []byte, readVersion int64, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID) []byte {
	req := types.WatchValueRequest{
		Key:         key,
		Version:     readVersion,
		Reply:       types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:  types.TenantInfo{TenantId: tenantId},
		SpanContext: span,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if value != nil {
		req.HasValue = true
		req.Value = value
	}
	return req.MarshalFDB()
}

func (tx *Transaction) sendWatch(ctx context.Context, key, value []byte, readVersion int64, span types.SpanContext, servers []ServerInfo, tags readTagSample) error {
	servers, ok := tx.db.failMon.liveServers(servers, EndpointWatchValue)
	if !ok {
		return tx.db.allAlternativesFailed(ctx)
	}
	_, chosenIdx := tx.db.queueModel.chooseServer(servers)
	order := loadBalanceOrder(servers, chosenIdx)

	// readVersion is captured synchronously by WatchSetup and passed in — it must
	// NOT be re-read from tx here, because the transaction may have been
	// postCommitReset() (readVersion → 0) by the time this async poll runs.
	tenantId := tx.watchTenant(ctx)
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()

	for _, server := range order {
		conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		sendCleanup.permit = permit
		watchToken := getAdjustedEndpoint(server.Token, EndpointWatchValue)
		replyToken, replyCh, replyHandle := conn.PrepareReplyTo(watchToken)
		replyCleanup.handle = replyHandle
		reqData := buildWatchValueRequest(key, value, readVersion, tenantId, span, tags, replyToken)

		delta := tx.db.queueModel.startRequest(server.Address)
		start := time.Now()

		if err := sendReadFrame(ctx, conn, watchToken, reqData, replyHandle, permit, nil); err != nil {
			tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
			replyHandle.Cancel()
			replyCleanup.release()
			tx.db.handleReadConnError(server.Address, conn, err)
			continue
		}
		// Long-poll: no short timeout. Use the caller's context deadline.
		select {
		case resp := <-replyCh:
			replyCleanup.release()
			if resp.Err != nil {
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(resp.Err, start), false)
				tx.db.handleReadConnError(server.Address, conn, resp.Err)
				continue
			}
			tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), true)
			return parseWatchValueReply(resp.Body)
		case <-ctx.Done():
			tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(ctx.Err(), start), false)
			replyHandle.Cancel()
			replyCleanup.release()
			return ctx.Err()
		}
	}
	return &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func parseWatchValueReply(data []byte) error {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return fmt.Errorf("WatchValue: %w", err)
	}
	// Reply parsed successfully — key has changed.
	return nil
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
