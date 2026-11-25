package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// getValueBufPool pools marshal buffers for GetValue requests.
var getValueBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

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
	lb := lbRetry{observe: tx.db.readBackoffObserver()}
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
		val, err := tx.sendGetValuePaced(ctx, key, loc.Servers, tags, &lb)
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
		// maybeDelivered (1030/1100 on every replica tried) is loadBalance's own unbounded retry, paced per
		// request by sendGetValuePaced; Go re-locates as it has no later all_alternatives_failed to do so.
		if isMaybeDelivered(err) {
			attempts--
			tx.db.locCache.invalidate(key, tx.tenantId, false)
			continue
		}
		// wrong_shard_server or all_alternatives_failed → invalidate cache, retry.
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
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
	return tx.sendGetValuePaced(ctx, key, servers, tags, nil)
}

// sendGetValuePaced is sendGetValue counting each maybe-delivered request as one loadBalance
// attempt in lb, which delays the request after it (LoadBalance.actor.h:301-308, 682-686, 721-725).
func (tx *Transaction) sendGetValuePaced(ctx context.Context, key []byte, servers []ServerInfo, tags readTagSample, lb *lbRetry) ([]byte, error) {
	alternatives := len(servers)
	servers, ok := tx.db.failMon.liveServers(servers, EndpointGetValue)
	if !ok {
		return nil, tx.db.allAlternativesFailed(ctx)
	}
	alts := newReadAlternatives(tx.db, servers, alternatives, lb)

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

	result := alts.start(ctx, makeSender, tx.readRPCTimeout())
	tx.db.finishReadRPC(result)
	err := result.err
	var val []byte
	if err == nil {
		var penalty float64
		val, penalty, err = parseGetValueReply(result.body)
		tx.db.noteReadBrokenPromise(err, servers, result.addr, EndpointGetValue)
		tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
		alts.fail(ctx, err)
		if !isMaybeDelivered(err) {
			return val, err
		}
	}
	secondIdx := alts.second
	if !result.triedSecondary {
		secondIdx = -1
	}
	var pace func() error
	if lb != nil {
		pace = func() error { return lb.pace(ctx, alternatives, 1) }
	}
	return resolveFallbackPaced(ctx, err, servers, alts.best, secondIdx, func(server ServerInfo) ([]byte, error) {
		return tx.sendGetValueToServer(ctx, key, server, readVersion, lockAware, tenantId, tags)
	}, pace)
}

// sendGetValueToServer is a single-server fallback after hedge failure.
// Its lone reply accrues second-request budget.
func (tx *Transaction) sendGetValueToServer(ctx context.Context, key []byte, server ServerInfo, readVersion int64, lockAware bool, tenantId int64, tags readTagSample) ([]byte, error) {
	conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
	if err != nil {
		tx.db.handleDialError(ctx, server.Address)
		noteLoneSendFailure(ctx, tx.db.queueModel)
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
		noteLoneSendFailure(ctx, tx.db.queueModel)
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
	tx.db.queueModel.firstReplied()
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
	if err := reply.UnmarshalFromReader(&r); err != nil {
		return nil, -1.0, fmt.Errorf("GetValue: %w", err)
	}
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. RFC-010 #1.
	if reply.HasError {
		return nil, reply.Penalty, &wire.FDBError{Code: int(reply.Error.ErrorCode)}
	}
	if !reply.HasValue {
		return nil, reply.Penalty, nil // key not found
	}
	return reply.Value, reply.Penalty, nil
}
