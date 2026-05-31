package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// marshalBufPools for read-path request types.
var (
	getKeyBufPool       = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
	getValueBufPool     = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
	getKeyValuesBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
)

const (
	wrongShardRetryDelay = 10 * time.Millisecond // CLIENT_KNOBS->WRONG_SHARD_SERVER_DELAY
	replyByteLimit       = 80000                 // CLIENT_KNOBS->REPLY_BYTE_LIMIT
)

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
func (tx *Transaction) getKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
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

		loc, err := tx.db.locCache.locate(tx.db, ctx, selectorKey, tx.tenantId)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		replyKey, replyOrEqual, replyOffset, err := tx.sendGetKey(ctx, selectorKey, orEqual, offset, loc.Servers)
		if err != nil {
			if isWrongShardServer(err) || isAllAlternativesFailed(err) {
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
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
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

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleConnError(server.Address)
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
				SsLatestCommitVersions: emptyVersionVector,
			}
			if lockAware {
				req.HasOptions = true
				req.Options = types.ReadOptions{HasLockAware: true, LockAware: []byte{}}
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
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, DefaultRPCTimeout)

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
// Other FDB errors (transaction_too_old, etc.) are returned to the caller
// for handling by the Transact retry loop.
func (tx *Transaction) getValue(ctx context.Context, key []byte) ([]byte, error) {
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId)
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
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
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

	// Build a sender closure for a given server.
	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleConnError(server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, replyToken, server.Token)

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
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, DefaultRPCTimeout)

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
		for i, server := range servers {
			if i == bestIdx || i == secondIdx {
				continue // already tried
			}
			val, err := tx.sendGetValueToServer(ctx, key, server, readVersion, lockAware, tenantId)
			if err == nil {
				return val, nil
			}
			if isWrongShardServer(err) || isAllAlternativesFailed(err) {
				return nil, err
			}
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
		tx.db.handleConnError(server.Address)
		return nil, err
	}
	replyToken, replyCh, replyHandle := conn.PrepareReply()
	defer replyHandle.Release()
	body, poolBuf := buildGetValueRequest(key, readVersion, lockAware, tenantId, replyToken, server.Token)

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
	resp, err := waitReply(replyCh, ctx, DefaultRPCTimeout)
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
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	const getRangeShardLimit = 100 // C++ CLIENT_KNOBS->GET_RANGE_SHARD_LIMIT
	const maxRelocateRetries = 5   // Bound retry loop; C++ relies on transaction timeout (default 5s)

	var allKVs []KeyValue
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end
	relocateRetries := 0

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Get all shard locations for current range. C++ getKeyRangeLocations
		// receives the reverse flag so the proxy returns shards in scan order.
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, curBegin, curEnd, getRangeShardLimit, reverse, tx.tenantId)
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
					if isWrongShardServer(err) || isAllAlternativesFailed(err) {
						relocateRetries++
						if relocateRetries > maxRelocateRetries {
							return nil, false, fmt.Errorf("getRange: exceeded %d relocate retries: %w", maxRelocateRetries, err)
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

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, err := tx.db.getOrDial(ctx, server.Address)
			if err != nil {
				tx.db.handleConnError(server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body, poolBuf := buildGetKeyValuesRequest(begin, end, readVersion, wireLimit, lockAware, tenantId, replyToken, server.Token)
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
	result := sendFrameWithHedge(ctx, hedgeDelay, primary, secondary, DefaultRPCTimeout)

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

// isWrongShardServer returns true if the error is FDB error 1062.
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

func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
		End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
		Version:                version,
		Limit:                  limit,
		LimitBytes:             replyByteLimit,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector,
	}
	if lockAware {
		req.HasOptions = true
		req.Options = types.ReadOptions{HasLockAware: true, LockAware: []byte{}}
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

func buildGetValueRequest(key []byte, version int64, lockAware bool, tenantId int64, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector,
	}
	if lockAware {
		req.HasOptions = true
		req.Options = types.ReadOptions{HasLockAware: true, LockAware: []byte{}}
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
	value, readVersion, err := tx.WatchSetup(ctx, key)
	if err != nil {
		return err
	}
	return tx.WatchPoll(ctx, key, value, readVersion)
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
func (tx *Transaction) WatchSetup(ctx context.Context, key []byte) ([]byte, int64, error) {
	// C++ NativeAPI.actor.cpp: watches are disabled when RYW is disabled.
	// Returns watches_disabled (1034) immediately.
	if tx.rywDisabled {
		return nil, 0, &wire.FDBError{Code: 1034} // watches_disabled
	}

	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, 0, err
	}
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()

	// C++ NativeAPI.actor.cpp watchValueMap: adds read conflict on watched key.
	tx.AddReadConflictKey(key)

	// Read current value so we can send it with the watch request.
	// C++ getValueOrStandby in watchValue actor reads the value at the watch version.
	value, err := tx.ryw.get(ctx, key, tx.getValue)
	return value, readVersion, err
}

// WatchPoll performs the ASYNCHRONOUS long-poll part of a watch: locate the
// storage server and wait for the WatchValueReply that fires when key's value
// differs from `value` (captured by WatchSetup), registered at `readVersion`
// (also captured by WatchSetup — NOT re-read from the possibly-reset transaction).
// Retries on wrong_shard_server with cache invalidation. Intended to run in the
// watch future's goroutine.
func (tx *Transaction) WatchPoll(ctx context.Context, key, value []byte, readVersion int64) error {
	// Use the transaction's watch context so Reset()/Cancel() cancels in-flight watches.
	// Matches C++ resetRyow() → resetPromise.sendError(transaction_cancelled).
	watchCtx := tx.getWatchCtx(ctx)

	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, locErr := tx.db.locCache.locate(tx.db, watchCtx, key, tx.tenantId)
		if locErr != nil {
			return fmt.Errorf("locate key: %w", locErr)
		}
		if len(loc.Servers) == 0 {
			return fmt.Errorf("no storage servers for key")
		}

		watchErr := tx.sendWatch(watchCtx, key, value, readVersion, loc.Servers)
		if watchErr == nil {
			return nil
		}
		if isWrongShardServer(watchErr) || isAllAlternativesFailed(watchErr) {
			tx.db.locCache.invalidate(key, tx.tenantId)
			if err := sleepCtx(watchCtx, wrongShardRetryDelay); err != nil {
				return err
			}
			continue
		}
		return watchErr
	}
	return &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func (tx *Transaction) sendWatch(ctx context.Context, key, value []byte, readVersion int64, servers []ServerInfo) error {
	_, chosenIdx := tx.db.queueModel.chooseServer(servers)
	order := loadBalanceOrder(servers, chosenIdx)

	// readVersion is captured synchronously by WatchSetup and passed in — it must
	// NOT be re-read from tx here, because the transaction may have been
	// postCommitReset() (readVersion → 0) by the time this async poll runs.
	tenantId := tx.tenantId

	for _, server := range order {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleConnError(server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		req := types.WatchValueRequest{
			Key:        key,
			Version:    readVersion,
			Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo: types.TenantInfo{TenantId: tenantId},
		}
		if value != nil {
			req.HasValue = true
			req.Value = value
		}
		reqData := req.MarshalFDB()
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
