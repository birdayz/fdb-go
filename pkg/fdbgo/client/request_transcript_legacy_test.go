package client

// The read path's storage plumbing as it was before RFC-262 phase 1 (commit
// 338e1644b), renamed with a legacy prefix. request_transcript_test.go runs
// the same reads through this copy and through the current code and requires
// the same requests, compared field by field without reply tokens, spans and
// debug IDs (body lengths included), and the same results. Delete it when
// RFC-262 phase 4 changes the RYW-disabled route on purpose.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func (tx *Transaction) legacyGetKeyForRange(parentCtx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, bool, error) {
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, err
	}
	if sp := tx.startOpSpan("fdbgo.getKey"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getKey)
		defer sp.End()
	}
	k, fallback, err := tx.legacyGetKeyImpl(ctx, selectorKey, orEqual, offset)
	return k, fallback, tx.mapReadError(ctx, err)
}

func (tx *Transaction) legacyGetKeyImpl(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, bool, error) {
	tx.hadRead.Store(true) // a read was issued — the rywDisabled GetKey choke (RFC-059)
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
		loc, err := tx.db.locCache.locate(tx.db, ctx, selectorKey, tx.tenantId, tx.currentSpan(), isBackward)
		if err != nil {
			return nil, false, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, false, fmt.Errorf("no storage servers for key")
		}

		replyKey, replyOrEqual, replyOffset, err := tx.legacySendGetKey(ctx, selectorKey, orEqual, offset, loc.Servers)
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

func (tx *Transaction) legacySendGetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo) ([]byte, bool, int32, error) {
	servers, ok := tx.db.failMon.liveServers(servers, EndpointGetKey)
	if !ok {
		return nil, false, 0, tx.db.allAlternativesFailed(ctx)
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
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			gkToken := getAdjustedEndpoint(server.Token, EndpointGetKey)
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(gkToken)
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

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, gkToken, reqData, replyHandle, nil); err != nil {
				getKeyBufPool.Put(bufp) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyHandle.Release()
				tx.db.handleReadConnError(server.Address, err)
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
		tx.db.endOther(o)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleReadConnError(result.addr, result.err)
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

func (tx *Transaction) legacyGetRangeReadThrough(parentCtx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
	// Once per call, before GRV: C++ Transaction::getRangeInternal (NativeAPI.actor.cpp:5808).
	tx.db.metrics.getRangeRequests.Add(1)
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, nil, err
	}
	if sp := tx.startOpSpan("fdbgo.getRange"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getRange)
		defer sp.End()
	}
	kvs, more, readThrough, err := tx.legacyGetRangeImpl(ctx, begin, end, limit, byteTarget, reverse)
	return kvs, more, readThrough, tx.mapReadError(ctx, err)
}

type legacyRangeScanOps[T any] struct {
	// send issues one shard-local request and parses its reply.
	send func(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, servers []ServerInfo) ([]T, bool, error)
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

var legacyPlainRangeScanOps = legacyRangeScanOps[KeyValue]{
	key:        func(kv *KeyValue) []byte { return kv.Key },
	sizeBytes:  func(kv *KeyValue) int64 { return int64(len(kv.Key) + len(kv.Value)) },
	limitBytes: func(kv *KeyValue) int { return 8 + len(kv.Key) + len(kv.Value) },
}

func (tx *Transaction) legacyGetRangeImpl(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
	ops := legacyPlainRangeScanOps
	ops.send = tx.legacySendGetRange
	return legacyRangeScanImpl(tx, ctx, begin, end, limit, byteTarget, reverse, ops)
}

func legacyRangeScanImpl[T any](tx *Transaction, ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, ops legacyRangeScanOps[T]) ([]T, bool, []byte, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	// C++ getExactRange locates GET_RANGE_SHARD_LIMIT (=2, ClientKnobs.cpp:101) shards at a time
	// (NativeAPI.actor.cpp:4272-4278) and the soft byte limit below depends on those pairs. Go
	// fetches more per locate RPC, always a whole number of pairs so pair origins coincide.
	const cppGetRangeShardLimit = 2
	const getRangeShardLimit = 50 * cppGetRangeShardLimit
	const maxRelocateRetries = 5 // Bound retry loop; C++ relies on transaction timeout (default 5s)

	var allKVs []T
	var materializedBytes int64 // RFC-115 §2: bound total materialized bytes vs WithRangeByteCeiling
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end
	bytesLeft := byteTarget // C++ limits.bytes after decrement
	relocateRetries := 0
	timeoutRetries := 0
	// C++ getRange's own loop, before any getRangeFallback, reports readThrough when the
	// first reply's rows end a shard the range continues past (NativeAPI.actor.cpp:4762,
	// 4795-4799). A byte-targeted call then stops; wrong_shard_server falls back (:4864-4870).
	firstReply := true
	var readThrough []byte

	// SOFT BYTE LIMIT — C++ getExactRange:4415:
	//
	//	// Soft byte limit - return results early if the user specified a byte limit and we got results
	//	if (limits.hasSatisfiedMinRows() && output.size() > 0) { output.more = true; return output; }
	//
	// hasSatisfiedMinRows() is hasByteLimit() && minRows == 0 (:2875), and minRows starts at 1
	// (FDBTypes.h GetRangeLimits ctor), so ANY non-empty reply satisfies it. C++ checks it before
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
	// BYTE_LIMIT_UNLIMITED (fdb_c.cpp:1002), so byteTarget is unlimited, this can never fire,
	// and the loop keeps absorbing across replies exactly as it did before.
	//
	// It is checked where the loop is about to fetch MORE data, never where the scan is
	// finishing. C's early return sits AFTER the shard-exhaustion block that returns
	// output.more = false (:4391-4400), so a range drained before its bytes run out still
	// reports more=false; a check placed before that would label every completed byte-limited
	// drain as having more.
	softByteStop := func() bool { return byteTarget != ByteLimitUnlimited && len(allKVs) > 0 }

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
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
			for remaining > 0 {
				kvs, more, err := ops.send(ctx, shardBegin, shardEnd, remaining, bytesLeft, reverse, loc.Servers)
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
					if byteTarget != ByteLimitUnlimited && len(kvs) > 0 && !more {
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
				remaining -= len(kvs)

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

				if byteTarget != ByteLimitUnlimited {
					for i := range kvs {
						bytesLeft -= ops.limitBytes(&kvs[i])
					}
					bytesLeft = max(0, bytesLeft)
				}
				// C++ limits.isReached(): rows == 0 || (bytes == 0 && minRows == 0). A row
				// or byte limit met reports more regardless of the shard's own flag
				// (:4359-4362), and no request goes out with a zero byte budget.
				if remaining <= 0 || (bytesLeft == 0 && len(allKVs) > 0) {
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

	return allKVs, remaining <= 0, readThrough, nil
}

func (tx *Transaction) legacySendGetRange(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, servers []ServerInfo) ([]KeyValue, bool, error) {
	return legacySendRangeRPC(tx, ctx, begin, end, limit, byteTarget, reverse, servers, legacyRangeRPCOps[KeyValue]{
		endpoint: EndpointGetKeyValues,
		build:    legacyBuildGetKeyValuesRequest,
		putBuf:   func(bufp *[]byte) { getKeyValuesBufPool.Put(bufp) },
		parse:    parseGetKeyValuesReply,
	})
}

type legacyRangeRPCOps[T any] struct {
	endpoint int
	build    func(begin, end []byte, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, serverToken transport.UID) ([]byte, *[]byte)
	putBuf   func(*[]byte)
	parse    func([]byte) ([]T, bool, float64, error)
}

func legacySendRangeRPC[T any](tx *Transaction, ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, servers []ServerInfo, ops legacyRangeRPCOps[T]) ([]T, bool, error) {
	// C++ transformRangeLimits (NativeAPI.actor.cpp:4223), called from BOTH range loops
	// (getExactRange:4299, getRange:4681). Both dimensions are clamped to REPLY_BYTE_LIMIT:
	// the row limit because "Can't get more than this many rows anyway", the byte limit
	// because it is the hard per-reply ceiling.
	wl := limit
	if wl > replyByteLimit {
		wl = replyByteLimit
	}
	wireLimit := int32(wl)
	if reverse {
		wireLimit = -wireLimit
	}
	wireLimitBytes := int32(replyByteLimit)
	if byteTarget != ByteLimitUnlimited && byteTarget < replyByteLimit {
		wireLimitBytes = int32(byteTarget)
	}
	servers, ok := tx.db.failMon.liveServers(servers, ops.endpoint)
	if !ok {
		return nil, false, tx.db.allAlternativesFailed(ctx)
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
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			gkvToken := getAdjustedEndpoint(server.Token, ops.endpoint)
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(gkvToken)
			body, poolBuf := ops.build(begin, end, readVersion, wireLimit, wireLimitBytes, lockAware, tenantId, span, replyToken, server.Token)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, gkvToken, body, replyHandle, nil); err != nil {
				ops.putBuf(poolBuf) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyHandle.Release()
				tx.db.handleReadConnError(server.Address, err)
				return inFlightRPC{err: err, addr: server.Address}
			}
			ops.putBuf(poolBuf)
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
		tx.db.endOther(o)
	}

	if result.addr != "" {
		if result.connErr {
			tx.db.handleReadConnError(result.addr, result.err)
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

func legacyBuildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
		End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
		Version:                version,
		Limit:                  limit,
		LimitBytes:             limitBytes,
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

func legacyBuildGetMappedKeyValuesRequest(mapper []byte) func(begin, end []byte, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	return func(begin, end []byte, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
		req := types.GetMappedKeyValuesRequest{
			Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
			End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
			Mapper:                 mapper,
			Version:                version,
			Limit:                  limit,
			LimitBytes:             limitBytes,
			Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo:             types.TenantInfo{TenantId: tenantId},
			SpanContext:            span,
			SsLatestCommitVersions: emptyVersionVector,
		}
		if lockAware {
			req.HasOptions = true
			req.Options = lockAwareReadOptions()
		}
		bufp := getMappedKeyValuesBufPool.Get().(*[]byte)
		result := req.MarshalFDBPooled(*bufp)
		*bufp = result
		return result, bufp
	}
}

var legacyMappedRangeScanOps = legacyRangeScanOps[MappedKeyValue]{
	key: func(m *MappedKeyValue) []byte { return m.Key },
	// MappedKeyValueRef inherits KeyValueRef::expectedSize (FDBTypes.h:920-922),
	// so a byte target is charged for the primary row only.
	limitBytes: func(m *MappedKeyValue) int { return 8 + len(m.Key) + len(m.Value) },
	sizeBytes: func(m *MappedKeyValue) int64 {
		// The ceiling bounds what the CLIENT materializes, so it must count the
		// secondary payload too — that is the bulk of a mapped reply, and
		// charging only the primary row would let a mapped scan blow through a
		// ceiling the plain path respects.
		n := int64(len(m.Key) + len(m.Value))
		switch m.Kind {
		case MappedResultGetValue:
			n += int64(len(m.GetValue.Key) + len(m.GetValue.Value))
		case MappedResultGetRange:
			n += int64(len(m.GetRange.Begin.Key) + len(m.GetRange.End.Key))
			for i := range m.GetRange.Rows {
				n += int64(len(m.GetRange.Rows[i].Key) + len(m.GetRange.Rows[i].Value))
			}
		}
		return n
	},
}

func (tx *Transaction) legacySendGetMappedRange(mapper []byte) func(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
	return func(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
		return legacySendRangeRPC(tx, ctx, begin, end, limit, byteTarget, reverse, servers, legacyRangeRPCOps[MappedKeyValue]{
			// getMappedKeyValues is adjusted endpoint 14, NOT 3. The field is
			// DECLARED immediately after getKeyValues (StorageServerInterface.h:103)
			// but is assigned getAdjustedEndpoint(14) (:184-185) — it was appended
			// after getKeyValuesStream to keep the older indices stable. Reading the
			// declaration order instead of the assignment would silently address
			// getShardState.
			endpoint: EndpointGetMappedKeyValues,
			build:    legacyBuildGetMappedKeyValuesRequest(mapper),
			putBuf:   func(bufp *[]byte) { getMappedKeyValuesBufPool.Put(bufp) },
			parse:    parseGetMappedKeyValuesReply,
		})
	}
}

func (tx *Transaction) legacyGetMappedRangeImpl(ctx context.Context, begin, end []byte, mapper []byte, limit, byteTarget int, reverse bool) ([]MappedKeyValue, bool, error) {
	ops := legacyMappedRangeScanOps
	ops.send = tx.legacySendGetMappedRange(mapper)
	// A mapped read keeps no RYW cache, so readThrough has no reader (ReadYourWrites.actor.cpp:1146-1161).
	rows, more, _, err := legacyRangeScanImpl(tx, ctx, begin, end, limit, byteTarget, reverse, ops)
	return rows, more, err
}

func (tx *Transaction) legacySendGetMappedSelectors(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
	// FDB 7.3.77 constructs a KeyRange before rejecting resolved inversion.
	if !mappedSelectorIsFGE(begin) || !mappedSelectorIsFGE(end) || bytes.Compare(begin.Key, end.Key) >= 0 {
		return nil, false, &wire.FDBError{Code: ErrInternalError}
	}
	rows, byteTarget := limits.requestLimits()
	return legacySendRangeRPC(tx, ctx, begin.Key, end.Key, rows, byteTarget, reverse, servers, legacyRangeRPCOps[MappedKeyValue]{
		endpoint: EndpointGetMappedKeyValues,
		build:    legacyBuildGetMappedKeyValuesRequest(mapper),
		putBuf:   func(bufp *[]byte) { getMappedKeyValuesBufPool.Put(bufp) },
		parse:    parseGetMappedKeyValuesReply,
	})
}

func (tx *Transaction) legacyGetMappedRangeSelectors(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	originalBegin, originalEnd, originalLimits := begin, end, limits
	var output mappedRangeResult
	var materializedBytes int64
	tx.hadRead.Store(true)
	for {
		if bytes.Compare(begin.Key, end.Key) >= 0 {
			return output, nil
		}
		locationKey, backward := begin.Key, keySelectorIsBackward(begin.OrEqual, begin.Offset)
		if reverse {
			locationKey = end.Key
			backward = keySelectorIsBackward(end.OrEqual, end.Offset-1)
		}
		loc, err := tx.db.locCache.locate(tx.db, ctx, locationKey, tx.tenantId, tx.currentSpan(), backward)
		if err != nil {
			return mappedRangeResult{}, fmt.Errorf("locate mapped range: %w", err)
		}
		if loc.ShardEnd == nil {
			loc.ShardEnd = allKeysEnd
		}
		reqBegin, reqEnd, modified := mappedRangeShardSelectors(begin, end, loc, reverse)
		rows, more, err := tx.legacySendGetMappedSelectors(ctx, reqBegin, reqEnd, mapper, limits, reverse, loc.Servers)
		if err != nil {
			if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
				tx.db.locCache.invalidate(locationKey, tx.tenantId, backward)
				if isWrongShardServer(err) {
					return tx.legacyGetMappedRangeSelectorFallback(ctx, originalBegin, originalEnd, mapper, originalLimits, reverse)
				}
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return mappedRangeResult{}, err
				}
				continue
			}
			if isReplyTimeout(err) {
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return mappedRangeResult{}, err
				}
				continue
			}
			return mappedRangeResult{}, err
		}
		if (more && len(rows) == 0) || (limits.rows != -1 && len(rows) > limits.rows) {
			return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
		}
		limits.decrement(rows)
		finished := limits.reached() || (!modified && !more) || limits.satisfiedMinRows()
		if err := tx.appendMappedSelectorRows(&output, rows, &materializedBytes); err != nil {
			return mappedRangeResult{}, err
		}
		if finished {
			output.more = modified || limits.reached() || more
			return output, nil
		}
		if !more {
			if len(rows) == 0 {
				return tx.legacyGetMappedRangeSelectorFallback(ctx, originalBegin, originalEnd, mapper, originalLimits, reverse)
			}
			if reverse {
				end = mappedSelectorFGE(loc.ShardBegin)
			} else {
				begin = mappedSelectorFGE(loc.ShardEnd)
			}
		} else if reverse {
			end = mappedSelectorFGE(output.rows[len(output.rows)-1].Key)
		} else {
			begin = mappedSelectorFGE(keyAfterBytes(output.rows[len(output.rows)-1].Key))
		}
	}
}

func (tx *Transaction) legacyGetMappedRangeSelectorFallback(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	if bytes.Compare(begin.Key, end.Key) >= 0 {
		return mappedRangeResult{}, nil
	}
	return tx.legacyGetMappedRangeSelectorExact(ctx, begin.Key, end.Key, mapper, limits, reverse)
}

func (tx *Transaction) legacyGetMappedRangeSelectorExact(ctx context.Context, begin, end, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	var output mappedRangeResult
	var materializedBytes int64
	for {
		locations, err := tx.mappedExactLocations(ctx, begin, end, reverse)
		if err != nil {
			return mappedRangeResult{}, err
		}
		if len(locations) == 0 {
			return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
		}
		for shard := 0; ; {
			loc := &locations[shard]
			rows, more, err := tx.legacySendGetMappedSelectors(ctx, mappedSelectorFGE(loc.ShardBegin), mappedSelectorFGE(loc.ShardEnd), mapper, limits, reverse, loc.Servers)
			if err != nil {
				if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
					if reverse {
						end = loc.ShardEnd
					} else {
						begin = loc.ShardBegin
					}
					tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
					if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
						return mappedRangeResult{}, err
					}
					break
				}
				if isReplyTimeout(err) {
					if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
						return mappedRangeResult{}, err
					}
					continue
				}
				return mappedRangeResult{}, err
			}
			if (more && len(rows) == 0) || (limits.rows != -1 && len(rows) > limits.rows) {
				return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
			}
			if err := tx.appendMappedSelectorRows(&output, rows, &materializedBytes); err != nil {
				return mappedRangeResult{}, err
			}
			limits.decrement(rows)
			if limits.reached() {
				output.more = true
				return output, nil
			}
			if reverse && more && len(rows) > 0 && bytes.Equal(output.rows[len(output.rows)-1].Key, loc.ShardBegin) {
				more = false
			}
			if more {
				last := output.rows[len(output.rows)-1].Key
				if reverse {
					loc.ShardEnd = last
				} else {
					loc.ShardBegin = keyAfterBytes(last)
				}
			}
			if !more || bytes.Compare(loc.ShardBegin, loc.ShardEnd) >= 0 {
				if shard == len(locations)-1 {
					if reverse {
						end = loc.ShardBegin
					} else {
						begin = loc.ShardEnd
					}
					if bytes.Compare(begin, end) >= 0 {
						output.more = false
						return output, nil
					}
					break
				}
				shard++
			}
			if limits.satisfiedMinRows() && len(output.rows) > 0 {
				output.more = true
				return output, nil
			}
		}
	}
}

func (tx *Transaction) legacyGetMappedRangeWithByteTargetAndSkip(ctx context.Context, begin, end, mapper []byte, limit, byteTarget int, skip int32, reverse bool) ([]MappedKeyValue, bool, error) {
	ctx, cancel := tx.opContext(ctx)
	defer cancel()
	// ThreadSafeTransaction checks deferredError before RYW's special-key
	// rejection, which still precedes resetPromise (C++ 7.3.77).
	if err := tx.readOperation(ctx).entryErr; err != nil {
		return nil, false, err
	}
	// C++ :1786 — the special key space is not mapped-readable. Checked after
	// deferred errors, before lifetime errors or taking the read version.
	if isSpecialKey(begin) && isSpecialKey(end) {
		return nil, false, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, tx.trackReadOperation(ctx, err)
	}
	// C++ :1801 — key_outside_legal_range.
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	// C++ :1817 — range_limits_invalid.
	if limit < -1 {
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	byteTarget, err := normalizeByteTarget(byteTarget)
	if err != nil {
		return nil, false, err
	}
	// A positive Skip narrows the range and cannot make inverted bounds readable.
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, nil
	}
	// C++ :1219-1233 makes snapshot isolation and RYW-disabled both
	// unsupported_operation. Only ONE of those is reachable here, and the
	// difference matters: RYW-disabled is a runtime state, so it is guarded
	// below; snapshot is unreachable BY CONSTRUCTION because *Snapshot exposes
	// no GetMappedRange at all, so there is no way to ask for the unsupported
	// thing. That is a stronger guarantee than returning 2108, not a weaker one
	// — but it holds only while *Snapshot lacks the method, which is why
	// TestGetMappedRange_SnapshotCannotRequestIt pins the absence rather than
	// leaving it as an unstated property of the API surface.
	if tx.rywDisabled {
		return nil, false, &wire.FDBError{Code: ErrUnsupportedOperation}
	}

	if skip > 0 {
		result, err := tx.legacyGetMappedRangeWithResolvedSkip(ctx, begin, end, mapper, newMappedRangeLimits(limit, byteTarget), skip, reverse)
		if err != nil {
			return nil, false, tx.trackReadOperation(ctx, err)
		}
		// Original anchors retain conflicts on skipped primaries, not secondaries.
		if err := tx.addMappedRangeConflicts(ctx, begin, end, reverse, result.more, result.rows); err != nil {
			return nil, false, err
		}
		return result.rows, result.more, nil
	}

	rows, more, err := tx.legacyGetMappedRangeImpl(ctx, begin, end, mapper, limit, byteTarget, reverse)
	if err != nil {
		return nil, false, tx.trackReadOperation(ctx, err)
	}

	// C++ addConflictRangeAndMustUnmodified (:1163-1192): the read went out at
	// Snapshot::True, so every conflict range it needs is added here, and each
	// one is first checked against the write map — an overlap means the mapped
	// read observed a key this transaction wrote, which RYW cannot honour for
	// mapped reads and which C++ reports rather than silently serving stale data.
	if err := tx.addMappedRangeConflicts(ctx, begin, end, reverse, more, rows); err != nil {
		return nil, false, err
	}
	return rows, more, nil
}

func (tx *Transaction) legacyGetMappedRangeWithResolvedSkip(ctx context.Context, begin, end, mapper []byte, limits mappedRangeLimits, skip int32, reverse bool) (mappedRangeResult, error) {
	if bytes.Compare(begin, end) >= 0 {
		return mappedRangeResult{}, nil
	}
	key, remaining := begin, int64(skip)
	if reverse {
		key, remaining = end, -remaining
	}
	exact := false
	for remaining != 0 {
		offset, rest := mappedSkipResolutionStep(remaining)
		// Native getKey adds neither a RYW overlay nor independent read conflicts.
		resolved, fallback, err := tx.legacyGetKeyForRange(ctx, key, false, offset)
		if err != nil {
			return mappedRangeResult{}, err
		}
		if (!reverse && bytes.Compare(resolved, end) >= 0) || (reverse && bytes.Compare(resolved, begin) <= 0) {
			return mappedRangeResult{}, nil
		}
		exact = exact || fallback || rest != 0
		key, remaining = resolved, rest
	}
	if reverse {
		end = key
	} else {
		begin = key
	}
	// Mapped storage requests must not contain offsets that can invert the range.
	if exact {
		return tx.legacyGetMappedRangeSelectorExact(ctx, begin, end, mapper, limits, reverse)
	}
	return tx.legacyGetMappedRangeSelectors(ctx, mappedSelectorFGE(begin), mappedSelectorFGE(end), mapper, limits, reverse)
}
