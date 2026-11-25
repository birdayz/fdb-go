package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// getKeyBufPool pools marshal buffers for GetKey requests.
var getKeyBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

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
	if tx.hooks != nil && tx.hooks.resolveKeyFn != nil {
		key, err := tx.hooks.resolveKeyFn(ctx, KeySelector{Key: selectorKey, OrEqual: orEqual, Offset: offset}, readVersion)
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
	var lb lbRetry
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
		lb.observe = tx.db.readBackoffObserver()
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
		replyKey, replyOrEqual, replyOffset, err := tx.sendGetKeyPaced(ctx, readVersion, selectorKey, orEqual, offset, loc.Servers, tags, replyWait, &lb)
		redraw = err == nil || readTagRetry(err)
		if err != nil {
			var retry bool
			timeoutRetries, shardRetries, needsRangeFallback, retry, err = tx.handleGetKeyRetry(
				ctx, selectorKey, isBackward, progressSteps, timeoutRetries, shardRetries, needsRangeFallback, err,
			)
			if retry {
				continue
			}
			return nil, false, err
		}
		lb = lbRetry{observe: tx.db.readBackoffObserver()} // the next request is a new loadBalance call

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

// handleGetKeyRetry classifies a getKey RPC failure and performs any retry sleep or cache invalidation.
func (tx *Transaction) handleGetKeyRetry(
	ctx context.Context,
	selectorKey []byte,
	isBackward bool,
	progressSteps int,
	timeoutRetries, shardRetries int,
	needsRangeFallback bool,
	err error,
) (int, int, bool, bool, error) {
	// Reply timeout (slow-but-alive server): re-send, bounded, then a
	// RETRYABLE transaction_too_old — same contract as getValue/
	// getRange. errReplyTimeout must never escape.
	if isReplyTimeout(err) {
		timeoutRetries++
		if timeoutRetries > maxReadTimeoutRetries {
			return timeoutRetries, shardRetries, needsRangeFallback, false, &wire.FDBError{Code: ErrTransactionTooOld}
		}
		if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
			return timeoutRetries, shardRetries, needsRangeFallback, false, err
		}
		return timeoutRetries, shardRetries, needsRangeFallback, true, nil
	}
	// maybeDelivered (1030/1100: a failed connect or a teardown on every
	// replica tried) is loadBalance's own unbounded retry (lbRetry); it is
	// re-located as getValueImpl explains, without charging shardRetries.
	if isMaybeDelivered(err) {
		tx.db.locCache.invalidate(selectorKey, tx.tenantId, isBackward)
		return timeoutRetries, shardRetries, needsRangeFallback, true, nil
	}
	if isWrongShardServer(err) || isAllAlternativesFailed(err) {
		// Native range reads switch to exact fallback on an initial wrong-shard reply.
		if progressSteps == 0 && isWrongShardServer(err) {
			needsRangeFallback = true
		}
		shardRetries++
		if shardRetries > MaxWrongShardRetries {
			// Transient routing error exhausted: retry the whole txn
			// with a fresh read version (consistent with getValue/
			// getRange; the read path never surfaces 1006 to the app).
			return timeoutRetries, shardRetries, needsRangeFallback, false, &wire.FDBError{Code: ErrTransactionTooOld}
		}
		tx.db.locCache.invalidate(selectorKey, tx.tenantId, isBackward)
		if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
			return timeoutRetries, shardRetries, needsRangeFallback, false, err
		}
		return timeoutRetries, shardRetries, needsRangeFallback, true, nil
	}
	return timeoutRetries, shardRetries, needsRangeFallback, false, err
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
	return tx.sendGetKeyPaced(ctx, readVersion, selectorKey, orEqual, offset, servers, tags, replyWait, nil)
}

func (tx *Transaction) sendGetKeyPaced(ctx context.Context, readVersion int64, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo, tags readTagSample, replyWait func(context.Context), lb *lbRetry) ([]byte, bool, int32, error) {
	alternatives := len(servers)
	servers, ok := tx.db.failMon.liveServers(servers, EndpointGetKey)
	if !ok {
		return nil, false, 0, tx.db.allAlternativesFailed(ctx)
	}
	alts := newReadAlternatives(tx.db, servers, alternatives, lb)

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

	result := alts.start(ctx, makeSender, tx.readRPCTimeout())
	for {
		tx.db.finishReadRPC(result)
		err := result.err
		var key []byte
		var replyOrEqual bool
		var replyOffset int32
		if err == nil {
			var penalty float64
			key, replyOrEqual, replyOffset, penalty, err = parseGetKeyReply(result.body)
			tx.db.noteReadBrokenPromise(err, servers, result.addr, EndpointGetKey)
			tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
			alts.fail(ctx, err)
		}
		if ctx.Err() != nil && err != nil {
			return nil, false, 0, ctx.Err()
		}
		if !isMaybeDelivered(err) {
			return key, replyOrEqual, replyOffset, err
		}
		next, ok := alts.advance(ctx, makeSender, tx.readRPCTimeout())
		if !ok {
			if ctx.Err() != nil {
				return nil, false, 0, ctx.Err()
			}
			return nil, false, 0, err
		}
		result = next
	}
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
	// The generated GetKeyReply does not decode Sel; decoding the rest checks it.
	var reply types.GetKeyReply
	if err := reply.UnmarshalFromReader(&r); err != nil {
		return nil, false, 0, -1.0, fmt.Errorf("GetKey: %w", err)
	}
	penalty = -1.0
	if r.FieldPresent(types.GetKeyReplySlotPenalty) {
		penalty = reply.Penalty
	}
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. RFC-010 #1.
	if reply.HasError {
		return nil, false, 0, penalty, &wire.FDBError{Code: int(reply.Error.ErrorCode)}
	}
	// Navigate into the KeySelector nested struct to extract all three fields.
	selR, selErr := r.ReadNestedReader(types.GetKeyReplySlotSel)
	if selErr != nil {
		return nil, false, 0, penalty, fmt.Errorf("read KeySelector: %w", selErr)
	}
	var sel types.KeySelectorRef
	if err := sel.UnmarshalFromReader(selR); err != nil {
		return nil, false, 0, penalty, fmt.Errorf("GetKey: %w", err)
	}
	return sel.Key, sel.OrEqual, sel.Offset, penalty, nil
}
