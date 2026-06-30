package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
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
	// maxReadTimeoutRetries bounds the re-send of a read whose RPC reply
	// timed out (errReplyTimeout). libfdb_c re-sends indefinitely (bounded by
	// the transaction's read-version validity, ~5s MVCC window); a re-send of
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

// pipelineReplyTimeout is the deferred (pipelined) read's reply-wait, capped by
// the SetTimeout deadline so a hung pipelined reply does not run a full
// readRPCTimeout PAST the transaction timeout (RFC-112). When the timer fires the
// read re-drives through getValue, which is opContext-bounded and maps a blown
// deadline to transaction_timed_out (1031). With no timeout set it is the normal
// readRPCTimeout.
func (tx *Transaction) pipelineReplyTimeout() time.Duration {
	d := tx.readRPCTimeout()
	if tx.timeout > 0 {
		rem := time.Until(tx.deadline)
		if rem < 0 {
			rem = 0
		}
		if rem < d {
			d = rem
		}
	}
	return d
}

// opContext bounds a read's RPC waits by the transaction's SetTimeout deadline, so
// an in-flight (slow-but-alive) read is cancelled when the timeout elapses rather
// than re-sent for ~maxReadTimeoutRetries×readRPCTimeout. This is the Go analog of
// C++ timebomb (ReadYourWrites.actor.cpp:1567/1576): the deadline races every read
// the way resetPromise does (`resetPromise.getFuture() || op`). With no timeout set
// it returns ctx unchanged. The caller MUST call the returned cancel. (RFC-112)
func (tx *Transaction) opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if tx.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, tx.deadline)
}

// mapTimeout converts a deadline/cancel error caused by THIS transaction's
// SetTimeout into transaction_timed_out (1031) — matching C++ timebomb, which
// raises transaction_timed_out, not a generic cancel. If the caller's own context
// is done it is the caller's cancellation, so the original error is preserved; we
// synthesize 1031 only when parentCtx is still live and our deadline has passed.
func (tx *Transaction) mapTimeout(parentCtx context.Context, err error) error {
	if err == nil || tx.timeout <= 0 || parentCtx.Err() != nil {
		return err
	}
	if (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) &&
		!time.Now().Before(tx.deadline) {
		return &wire.FDBError{Code: ErrTransactionTimedOut}
	}
	return err
}

// sleepCtx sleeps for the given duration but returns early if ctx is cancelled.
// Returns ctx.Err() if the context was cancelled, nil otherwise.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	}
}

// allKeysEnd is \xFF\xFF — the absolute end of the key space.
var allKeysEnd = []byte{0xFF, 0xFF}

// getKey resolves a key selector via the storage server.
// Matches C++ NativeAPI.actor.cpp getKey(): loops until the selector is fully
// resolved (offset==0 && orEqual==true). A storage server resolves the selector
// within its shard; if the result crosses a shard boundary, it returns a partial
// resolution (non-zero offset). The client must then locate the new shard and
// re-issue the request with the updated selector.
func (tx *Transaction) getKey(parentCtx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if sp := tx.startOpSpan("fdbgo.getKey"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getKey)
		defer sp.End()
	}
	k, err := tx.getKeyImpl(ctx, selectorKey, orEqual, offset)
	return k, tx.mapTimeout(parentCtx, err)
}

func (tx *Transaction) getKeyImpl(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	tx.hadRead.Store(true) // a read was issued — the rywDisabled GetKey choke (RFC-059)
	// Three independent budgets, because getKey's loop iterates for two
	// unrelated reasons (codex): successful partial selector resolutions
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
	for {
		// C++ short-circuits: if key == allKeysEnd → offset > 0 → return allKeysEnd
		// if key == "" && offset <= 0 → return "" (empty key)
		// These checks are INSIDE the loop (matching C++) because the selector
		// key may be updated by a partial resolution from the previous iteration.
		if bytes.Equal(selectorKey, allKeysEnd) {
			if offset > 0 {
				return allKeysEnd, nil
			}
			orEqual = false // C++: k.orEqual = false
		} else if len(selectorKey) == 0 && offset <= 0 {
			return []byte{}, nil
		}

		loc, err := tx.db.locCache.locate(tx.db, ctx, selectorKey, tx.tenantId, tx.spanContext)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		replyKey, replyOrEqual, replyOffset, err := tx.sendGetKey(ctx, selectorKey, orEqual, offset, loc.Servers)
		if err != nil {
			// Reply timeout (slow-but-alive server): re-send, bounded, then a
			// RETRYABLE transaction_too_old — same contract as getValue/
			// getRange. errReplyTimeout must never escape.
			if isReplyTimeout(err) {
				timeoutRetries++
				if timeoutRetries > maxReadTimeoutRetries {
					return nil, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nil, err
				}
				continue
			}
			if isWrongShardServer(err) || isAllAlternativesFailed(err) {
				shardRetries++
				if shardRetries > MaxWrongShardRetries {
					// Transient routing error exhausted: retry the whole txn
					// with a fresh read version (consistent with getValue/
					// getRange; the read path never surfaces 1006 to the app).
					return nil, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				tx.db.locCache.invalidate(selectorKey, tx.tenantId)
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}

		// C++ NativeAPI.actor.cpp:1823-1826: k = reply.sel; if (!k.offset && k.orEqual) return k.getKey();
		// If offset==0 && orEqual==true, the selector is fully resolved.
		// Otherwise, the storage server returned a partial resolution — the
		// selector crossed a shard boundary. Update and loop.
		if replyOffset == 0 && replyOrEqual {
			return replyKey, nil
		}
		selectorKey = replyKey
		orEqual = replyOrEqual
		offset = replyOffset
		progressSteps++
		if progressSteps > maxSelectorResolutionSteps {
			// The selector keeps resolving without converging — a pathological
			// selector or a misbehaving server, NOT a transient condition.
			// Non-retryable: a fresh read version would re-hit the same cap.
			return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
		}
	}
}

// sendGetKey sends a GetKeyRequest and returns the full KeySelector from the reply.
// Returns (key, orEqual, offset, error). The caller must check offset==0 && orEqual
// to determine if the selector is fully resolved. Matches C++ getKey() which uses
// the reply's KeySelector to drive the resolution loop.
func (tx *Transaction) sendGetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo) ([]byte, bool, int32, error) {
	bestIdx, secondIdx := tx.db.queueModel.chooseTopTwo(servers)
	if !tx.db.hedgeEnabled.Load() {
		secondIdx = -1
	}

	// Capture tx fields before closures (see sendGetValue comment).
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.spanContext) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			replyToken, replyCh, replyHandle := conn.PrepareReply()
			req := types.GetKeyRequest{
				Sel: types.KeySelectorRef{
					Key:     selectorKey,
					OrEqual: orEqual,
					Offset:  offset,
				},
				Version:                readVersion,
				Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
				TenantInfo:             types.TenantInfo{TenantId: tenantId},
				SpanContext:            span,
				SsLatestCommitVersions: emptyVersionVector,
			}
			if lockAware {
				req.HasOptions = true
				req.Options = lockAwareReadOptions()
			}
			bufp := getKeyBufPool.Get().(*[]byte)
			reqData := req.MarshalFDBPooled(*bufp)
			*bufp = reqData
			gkToken := getAdjustedEndpoint(server.Token, EndpointGetKey)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := conn.SendFrame(gkToken, reqData); err != nil {
				getKeyBufPool.Put(bufp)
				tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
				replyHandle.Cancel()
				replyHandle.Release()
				tx.db.handleConnError(server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			getKeyBufPool.Put(bufp)
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
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
		tx.db.queueModel.endRequest(o.addr, o.delta, time.Since(o.start), false)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleConnError(result.addr)
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		}
	}
	if result.err != nil {
		return nil, false, 0, result.err
	}

	key, replyOrEqual, replyOffset, penalty, err := parseGetKeyReply(result.body)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return key, replyOrEqual, replyOffset, err
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
// re-sent without invalidating the location, matching libfdb_c's loadBalance
// (no per-read client timeout; re-send until the server replies or the read
// version ages to transaction_too_old). Other FDB errors (transaction_too_old,
// etc.) are returned to the caller for handling by the Transact retry loop.
func (tx *Transaction) getValue(parentCtx context.Context, key []byte) ([]byte, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if sp := tx.startOpSpan("fdbgo.getValue"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getValue)
		defer sp.End()
	}
	start := time.Now()
	v, err := tx.getValueImpl(ctx, key)
	if err == nil && tx.db != nil {
		// RFC-114: GetValue read latency (C++ readLatencies, NativeAPI.actor.cpp:3698),
		// sampled on the successful reply only. Divergence (documented in RFC-114):
		// `start` is taken before getValueImpl, so on the cold path this span includes
		// the locate + any wrong-shard retry loop, whereas C++ resets startTimeD per
		// attempt (:3660) and measures only the final physical-read RPC. Identical on
		// the common single-RPC happy path; Go over-measures under a wrong-shard storm.
		tx.db.metrics.observeReadLatency(time.Since(start))
	}
	return v, tx.mapTimeout(parentCtx, err)
}

func (tx *Transaction) getValueImpl(ctx context.Context, key []byte) ([]byte, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	timeoutRetries := 0
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.spanContext)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		val, err := tx.sendGetValue(ctx, key, loc.Servers)
		if err == nil {
			return val, nil
		}
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
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(key, tx.tenantId)
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

func (tx *Transaction) sendGetValue(ctx context.Context, key []byte, servers []ServerInfo) ([]byte, error) {
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
	span := childSpanContext(tx.spanContext) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	// Build a sender closure for a given server.
	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, span, replyToken, server.Token)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := conn.SendFrame(server.Token, body); err != nil {
				getValueBufPool.Put(poolBuf)
				tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
				replyHandle.Cancel()
				replyHandle.Release()
				tx.db.handleConnError(server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			getValueBufPool.Put(poolBuf)
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
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
		tx.db.queueModel.endRequest(o.addr, o.delta, time.Since(o.start), false)
	}

	// Process result.
	if result.addr != "" {
		if result.connErr {
			tx.db.handleConnError(result.addr)
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		}
	}
	if result.err != nil {
		// Hedge failed — fall back to remaining servers sequentially.
		// A reply timeout from any arm (hedge or a fallback) is REMEMBERED but
		// does NOT stop the scan: a later replica may be healthy, and one slow
		// server must not pre-empt an available one (codex). Only a definitive
		// wrong_shard/all_alternatives reply ends the scan (all alternatives
		// share the shard assignment, so re-locating is the right response).
		sawTimeout := isReplyTimeout(result.err)
		for i, server := range servers {
			if i == bestIdx || i == secondIdx {
				continue // already tried
			}
			val, err := tx.sendGetValueToServer(ctx, key, server, readVersion, lockAware, tenantId)
			if err == nil {
				return val, nil
			}
			if isReplyTimeout(err) {
				sawTimeout = true
				continue // remember it, keep trying healthy replicas
			}
			if isWrongShardServer(err) || isAllAlternativesFailed(err) {
				return nil, err
			}
		}
		// No replica succeeded. Prefer surfacing the timeout (so getValue
		// re-sends without a pointless cache invalidation — the location is
		// fine, the servers were slow) over flattening to 1006; only a genuine
		// no-reachable-server outcome flattens.
		if sawTimeout {
			return nil, errReplyTimeout
		}
		return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
	}

	val, penalty, err := parseGetValueReply(result.body)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return val, err
}

// sendGetValueToServer sends a getValue RPC to a single specific server.
// Used as fallback after hedge fails.
func (tx *Transaction) sendGetValueToServer(ctx context.Context, key []byte, server ServerInfo, readVersion int64, lockAware bool, tenantId int64) ([]byte, error) {
	conn, err := tx.db.getOrDial(ctx, server.Address)
	if err != nil {
		tx.db.handleDialError(ctx, server.Address)
		return nil, err
	}
	replyToken, replyCh, replyHandle := conn.PrepareReply()
	defer replyHandle.Release()
	body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, childSpanContext(tx.spanContext), replyToken, server.Token)

	delta := tx.db.queueModel.startRequest(server.Address)
	start := time.Now()

	if err := conn.SendFrame(server.Token, body); err != nil {
		getValueBufPool.Put(poolBuf)
		tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
		replyHandle.Cancel()
		tx.db.handleConnError(server.Address)
		return nil, err
	}
	getValueBufPool.Put(poolBuf)
	resp, err := waitReply(replyCh, ctx, tx.readRPCTimeout())
	if err != nil {
		tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
		replyHandle.Cancel()
		return nil, err
	}
	if resp.Err != nil {
		tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
		tx.db.handleConnError(server.Address)
		return nil, resp.Err
	}
	val, penalty, parseErr := parseGetValueReply(resp.Body)
	tx.db.queueModel.endRequestFull(server.Address, delta, time.Since(start), parseErr == nil, isFutureVersionOrProcessBehind(parseErr), penalty)
	return val, parseErr
}

// getRange reads a key range [begin, end), fetching all overlapping shard locations
// at once and iterating them in scan order. Matches C++ getExactRange in
// NativeAPI.actor.cpp: re-queries same shard on more=true (no re-locate),
// invalidates entire remaining range on wrong_shard_server, and passes reverse
// to getKeyRangeLocations so the proxy returns shards in the right order.
func (tx *Transaction) getRange(parentCtx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if sp := tx.startOpSpan("fdbgo.getRange"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getRange)
		defer sp.End()
	}
	kvs, more, err := tx.getRangeImpl(ctx, begin, end, limit, reverse)
	return kvs, more, tx.mapTimeout(parentCtx, err)
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

func (tx *Transaction) getRangeImpl(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	// getRangeShardLimit is locations fetched per locateRange RPC. NOT C++ GET_RANGE_SHARD_LIMIT
	// (=2, ClientKnobs.cpp:101) — a deliberate Go perf deviation: pre-fetch up to 100 shard locations
	// per RPC (fewer, larger locate calls) vs C++ getExactRange's batch-of-2. Correctness-neutral: both
	// loop until the range is exhausted and return identical KVs; only the locate-RPC count differs.
	const getRangeShardLimit = 100
	const maxRelocateRetries = 5 // Bound retry loop; C++ relies on transaction timeout (default 5s)

	var allKVs []KeyValue
	var materializedBytes int64 // RFC-115 §2: bound total materialized bytes vs WithRangeByteCeiling
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end
	relocateRetries := 0
	timeoutRetries := 0

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Get all shard locations for current range. C++ getKeyRangeLocations
		// receives the reverse flag so the proxy returns shards in scan order.
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, curBegin, curEnd, getRangeShardLimit, reverse, tx.tenantId, tx.spanContext)
		if err != nil {
			return nil, false, fmt.Errorf("locate range: %w", err)
		}
		if len(locations) == 0 {
			return allKVs, false, nil
		}

		// C++ getExactRange iterates shard=0,1,2,... linearly. With reverse=true
		// on the request, locations[0] is already the shard nearest end.
		relocated := false
		for shard := 0; shard < len(locations) && remaining > 0; shard++ {
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

			// Inner loop: re-query same shard while more=true (C++ stays on same
			// shard index, mutates locations[shard].range in-place).
			for remaining > 0 {
				kvs, more, err := tx.sendGetRange(ctx, shardBegin, shardEnd, remaining, reverse, loc.Servers)
				if err != nil {
					// Reply timeout (a slow-but-alive server): the location is
					// fine — re-send the SAME shard (no relocate), matching
					// libfdb_c (no per-read client timeout; re-send until the
					// server replies or the read version ages to
					// transaction_too_old). Bounded; on exhaustion surface a
					// RETRYABLE transaction_too_old so the Transact loop retries
					// with a fresh read version. errReplyTimeout never escapes.
					if isReplyTimeout(err) {
						timeoutRetries++
						if timeoutRetries > maxReadTimeoutRetries {
							return nil, false, &wire.FDBError{Code: ErrTransactionTooOld}
						}
						if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
							return nil, false, err
						}
						continue // re-send same shard
					}
					if isWrongShardServer(err) || isAllAlternativesFailed(err) {
						relocateRetries++
						if relocateRetries > maxRelocateRetries {
							// Surface a RETRYABLE transaction_too_old (not the
							// terminal all_alternatives_failed): the read path
							// absorbs all_alternatives_failed and lets the
							// transaction retry with a fresh locate, matching
							// libfdb_c's internal retry of the read.
							return nil, false, &wire.FDBError{Code: ErrTransactionTooOld}
						}
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
							return nil, false, err
						}
						relocated = true
						break // break to outer loop for re-locate
					}
					return nil, false, err
				}

				allKVs = append(allKVs, kvs...)
				remaining -= len(kvs)

				// RFC-115 §2: opt-in OOM ceiling. Checked AFTER each batch append so a
				// runaway unbounded scan errors instead of OOM-ing; the overshoot is at
				// most one reply (~80 KB). 0 = unlimited (default, oracle-matching).
				if ceiling := tx.db.rangeByteCeiling; ceiling > 0 {
					for _, kv := range kvs {
						materializedBytes += int64(len(kv.Key) + len(kv.Value))
					}
					if materializedBytes > ceiling {
						return nil, false, &RangeMaterializationLimitError{LimitBytes: ceiling, ReachedBytes: materializedBytes}
					}
				}

				if remaining <= 0 {
					// Limit reached. C++ getExactRange sets
					// output.more = (data.size() == limit) — always true when
					// the limit is met, regardless of the current shard's more
					// flag. There may be more data in subsequent shards.
					return allKVs, true, nil
				}

				// C++ "fix more" heuristic (NativeAPI.actor.cpp:2331-2333):
				// If reverse scan's last returned key equals shard begin, the
				// shard is exhausted regardless of what more says.
				if more && reverse && len(kvs) > 0 &&
					bytes.Equal(kvs[len(kvs)-1].Key, shardBegin) {
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

				// Narrow range and re-query same shard (C++ mutates
				// locations[shard].range in-place, lines 2349-2354).
				if reverse {
					shardEnd = kvs[len(kvs)-1].Key // [shardBegin, smallestReturnedKey)
				} else {
					shardBegin = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0) // keyAfter(lastKey)
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

	return allKVs, remaining <= 0, nil
}

func (tx *Transaction) sendGetRange(ctx context.Context, begin, end []byte, limit int, reverse bool, servers []ServerInfo) ([]KeyValue, bool, error) {
	wl := limit
	if wl > math.MaxInt32 {
		wl = math.MaxInt32
	}
	wireLimit := int32(wl)
	if reverse {
		wireLimit = -wireLimit
	}

	bestIdx, secondIdx := tx.db.queueModel.chooseTopTwo(servers)
	if !tx.db.hedgeEnabled.Load() {
		secondIdx = -1
	}

	// Capture tx fields before closures (see sendGetValue comment).
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.spanContext) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body, poolBuf := buildGetKeyValuesRequest(begin, end, readVersion, wireLimit, lockAware, tenantId, span, replyToken, server.Token)
			gkvToken := getAdjustedEndpoint(server.Token, EndpointGetKeyValues)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := conn.SendFrame(gkvToken, body); err != nil {
				getKeyValuesBufPool.Put(poolBuf)
				tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
				replyHandle.Cancel()
				replyHandle.Release()
				tx.db.handleConnError(server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			getKeyValuesBufPool.Put(poolBuf)
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
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
		tx.db.queueModel.endRequest(o.addr, o.delta, time.Since(o.start), false)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleConnError(result.addr)
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		} else if result.err != nil {
			tx.db.queueModel.endRequest(result.addr, result.delta, time.Since(result.start), false)
		}
	}
	if result.err != nil {
		return nil, false, result.err
	}

	kvs, more, penalty, err := parseGetKeyValuesReply(result.body)
	tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
	return kvs, more, err
}

// isWrongShardServer returns true if the error is FDB error 1001 (wrong_shard_server).
func isWrongShardServer(err error) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == ErrWrongShardServer
}

// isAllAlternativesFailed returns true if the error is FDB error 1006.
func isAllAlternativesFailed(err error) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == ErrAllAlternativesFailed
}

// isFutureVersionOrProcessBehind returns true for errors that should trigger
// future_version backoff in the QueueModel. Matches C++ ModelHolder::release()
// which passes futureVersion=true for future_version (1009) and process_behind (1037).
func isFutureVersionOrProcessBehind(err error) bool {
	if err == nil {
		return false
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		return false
	}
	return fdbErr.Code == ErrFutureVersion || fdbErr.Code == ErrProcessBehind
}

func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
		End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
		Version:                version,
		Limit:                  limit,
		LimitBytes:             replyByteLimit,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span, // RFC-115 §4
		SsLatestCommitVersions: emptyVersionVector,
	}
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

func buildGetValueRequest(key []byte, version int64, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span, // RFC-115 §4
		SsLatestCommitVersions: emptyVersionVector,
	}
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
func (tx *Transaction) Watch(ctx context.Context, key []byte) error {
	value, readVersion, span, watchCtx, err := tx.WatchSetup(ctx, key)
	if err != nil {
		return err
	}
	return tx.WatchPoll(watchCtx, key, value, readVersion, span)
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
func (tx *Transaction) WatchSetup(ctx context.Context, key []byte) ([]byte, int64, types.SpanContext, context.Context, error) {
	// C++ NativeAPI.actor.cpp: watches are disabled when RYW is disabled.
	// Returns watches_disabled (1034) immediately.
	if tx.rywDisabled {
		return nil, 0, types.SpanContext{}, nil, &wire.FDBError{Code: 1034} // watches_disabled
	}
	// Eager legal-range + key-size validation, BEFORE the read — C++ RYW watch
	// (ReadYourWrites.actor.cpp:2450-2456): a key >= getMaxReadKey() (and != metadataVersionKey) is
	// key_outside_legal_range (2004), an oversized key is key_too_large (2102). Without this a normal
	// (non-system) transaction could register a watch on a \xff system key or an oversized key that
	// libfdb_c rejects. Mirrors the Get/Set legal-range + commit key-size gates.
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, 0, types.SpanContext{}, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
	if len(key) > getMaxWriteKeySize(key, rawAccess) {
		return nil, 0, types.SpanContext{}, nil, &wire.FDBError{Code: 2102} // key_too_large
	}

	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, 0, types.SpanContext{}, nil, err
	}
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	// Capture the trace span here, SYNCHRONOUSLY — same reason as readVersion: the async
	// WatchPoll must NOT read tx.spanContext later, because a `w := tr.Watch(k)` inside
	// Database.Transact commits and postCommitReset()s the tx (regenerateSpan rewrites
	// spanContext) before the future goroutine runs — reading it there is a data race
	// (RFC-115 §4). Threaded through to sendWatch.
	span := tx.spanContext

	// C++ NativeAPI.actor.cpp watchValueMap: adds read conflict on watched key.
	tx.AddReadConflictKey(key)

	// Read current value so we can send it with the watch request.
	// C++ getValueOrStandby in watchValue actor reads the value at the watch version.
	// NOT tracked in readErr: the C++ watch actor reading.add's its `done`
	// future (ReadYourWrites.actor.cpp:1290), but every error path sends
	// done.send(Void()) BEFORE rethrowing (:1299-1302, :1325-1329) — done
	// completes successfully, so a failed watch read never poisons commit;
	// reading only barriers on watch-setup completion (codex P2 on RFC-098,
	// resolved the opposite way the finding suggested: the C++ source shows
	// watch errors are deliberately excluded).
	value, err := tx.ryw.get(ctx, key, tx.getValue)
	if err != nil {
		return value, readVersion, span, nil, err
	}
	// Capture the watch context SYNCHRONOUSLY — same reason as readVersion/span above. The async
	// WatchPoll must USE this context, not lazily fetch it in the future goroutine: a lazy
	// getWatchCtx there races Cancel()/reset() (which nil watchCtx/watchCancel), and if cancelWatches
	// runs first the poll mints a fresh, never-cancelled context → the watch goroutine + its storage
	// reply handle leak. Binding it here (before the future goroutine spawns) guarantees a later
	// Cancel/reset cancels the very context the poll holds, so it drains.
	watchCtx := tx.getWatchCtx(ctx)
	return value, readVersion, span, watchCtx, nil
}

// WatchPoll performs the ASYNCHRONOUS long-poll part of a watch: locate the
// storage server and wait for the WatchValueReply that fires when key's value
// differs from `value` (captured by WatchSetup), registered at `readVersion`
// (also captured by WatchSetup — NOT re-read from the possibly-reset transaction).
// Retries on wrong_shard_server with cache invalidation. Intended to run in the
// watch future's goroutine.
func (tx *Transaction) WatchPoll(watchCtx context.Context, key, value []byte, readVersion int64, span types.SpanContext) error {
	// watchCtx is captured SYNCHRONOUSLY by WatchSetup and passed in (NOT fetched here via
	// getWatchCtx): this runs in the async watch future, so a lazy fetch would race
	// Cancel()/reset()'s cancelWatches on watchCtx/watchCancel AND, if cancelWatches won, leak a
	// never-cancelled poll. Binding at setup makes Reset()/Cancel() cancel the very context this
	// loop holds — matching C++ resetRyow() → resetPromise.sendError(transaction_cancelled). span
	// is likewise captured synchronously by WatchSetup (re-reading tx.spanContext here would race a
	// concurrent commit/reset's regenerateSpan, RFC-115 §4).

	// Outstanding-watch cap (C++ increaseWatchCounter, NativeAPI.actor.cpp:5694/2175): reserve a
	// slot before polling; over the cap surfaces too_many_watches (1032) via this watch's future,
	// released on every exit path (fire / error / cancel). tx.db is always set on a WatchPoll (the
	// locate/sendWatch below dereference it unconditionally), so no nil guard.
	if err := tx.db.tryAcquireWatch(); err != nil {
		return err // too_many_watches (1032); not acquired, so no release
	}
	defer tx.db.releaseWatch()

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
		loc, locErr := tx.db.locCache.locate(tx.db, watchCtx, key, tx.tenantId, span)
		if locErr != nil {
			return fmt.Errorf("locate key: %w", locErr)
		}
		if len(loc.Servers) == 0 {
			return fmt.Errorf("no storage servers for key")
		}

		watchErr := tx.sendWatch(watchCtx, key, value, readVersion, watchSpan, loc.Servers)
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
			tx.db.locCache.invalidate(key, tx.tenantId)
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
func buildWatchValueRequest(key, value []byte, readVersion int64, tenantId int64, span types.SpanContext, replyToken transport.UID) []byte {
	req := types.WatchValueRequest{
		Key:         key,
		Version:     readVersion,
		Reply:       types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:  types.TenantInfo{TenantId: tenantId},
		SpanContext: span,
	}
	if value != nil {
		req.HasValue = true
		req.Value = value
	}
	return req.MarshalFDB()
}

func (tx *Transaction) sendWatch(ctx context.Context, key, value []byte, readVersion int64, span types.SpanContext, servers []ServerInfo) error {
	_, chosenIdx := tx.db.queueModel.chooseServer(servers)
	order := loadBalanceOrder(servers, chosenIdx)

	// readVersion is captured synchronously by WatchSetup and passed in — it must
	// NOT be re-read from tx here, because the transaction may have been
	// postCommitReset() (readVersion → 0) by the time this async poll runs.
	tenantId := tx.tenantId

	for _, server := range order {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		reqData := buildWatchValueRequest(key, value, readVersion, tenantId, span, replyToken)
		watchToken := getAdjustedEndpoint(server.Token, EndpointWatchValue)

		delta := tx.db.queueModel.startRequest(server.Address)
		start := time.Now()

		if err := conn.SendFrame(watchToken, reqData); err != nil {
			tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
			replyHandle.Cancel()
			replyHandle.Release()
			tx.db.handleConnError(server.Address)
			continue
		}
		// Long-poll: no short timeout. Use the caller's context deadline.
		select {
		case resp := <-replyCh:
			replyHandle.Release()
			if resp.Err != nil {
				tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
				tx.db.handleConnError(server.Address)
				continue
			}
			tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), true)
			return parseWatchValueReply(resp.Body)
		case <-ctx.Done():
			tx.db.queueModel.endRequest(server.Address, delta, time.Since(start), false)
			replyHandle.Cancel()
			replyHandle.Release()
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
