package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

const wrongShardRetryDelay = 10 * time.Millisecond // CLIENT_KNOBS->WRONG_SHARD_SERVER_DELAY

// getKey resolves a key selector via the storage server.
func (tx *Transaction) getKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, err := tx.db.locCache.locate(tx.db, ctx, selectorKey, tx.tenantId)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		key, err := tx.sendGetKey(ctx, selectorKey, orEqual, offset, loc.Servers)
		if err == nil {
			return key, nil
		}
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(selectorKey, tx.tenantId)
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		return nil, err
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func (tx *Transaction) sendGetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo) ([]byte, error) {
	for _, server := range servers {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleConnError(server.Address)
			continue
		}
		replyToken, replyCh, cancelReply := conn.PrepareReply()
		req := types.GetKeyRequest{
			Sel: types.KeySelectorRef{
				Key:     selectorKey,
				OrEqual: orEqual,
				Offset:  offset,
			},
			Version:                tx.readVersion,
			Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo:             types.TenantInfo{TenantId: tx.tenantId},
			SsLatestCommitVersions: emptyVersionVector,
		}
		if tx.lockAware || tx.readLockAware {
			req.HasOptions = true
			req.Options = types.ReadOptions{HasLockAware: true, LockAware: []byte{}}
		}
		reqData := req.MarshalFDB()
		gkToken := getAdjustedEndpoint(server.Token, EndpointGetKey)
		if err := conn.SendFrame(gkToken, reqData); err != nil {
			cancelReply()
			tx.db.handleConnError(server.Address)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				tx.db.handleConnError(server.Address)
				continue
			}
			return parseGetKeyReply(resp.Body)
		case <-rctx.Done():
			cancel()
			cancelReply()
			continue
		}
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func parseGetKeyReply(data []byte) ([]byte, error) {
	r, err := wire.ReadErrorOr(data)
	if err != nil {
		return nil, fmt.Errorf("GetKey: %w", err)
	}
	// Navigate into the KeySelector nested struct (slot 3) to extract the key (inner slot 0).
	selR, err := r.ReadNestedReader(types.GetKeyReplySlotSel)
	if err != nil {
		return nil, fmt.Errorf("read KeySelector: %w", err)
	}
	return selR.ReadBytes(types.KeySelectorRefSlotKey), nil
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
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		// Other FDB error → bubble up for Transact retry.
		return nil, err
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func (tx *Transaction) sendGetValue(ctx context.Context, key []byte, servers []ServerInfo) ([]byte, error) {
	for _, server := range servers {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleConnError(server.Address)
			continue
		}
		replyToken, replyCh, cancelReply := conn.PrepareReply()
		body := buildGetValueRequest(key, tx.readVersion, tx.lockAware || tx.readLockAware, tx.tenantId, replyToken, server.Token)
		if err := conn.SendFrame(server.Token, body); err != nil {
			cancelReply()
			tx.db.handleConnError(server.Address)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				tx.db.handleConnError(server.Address)
				continue
			}
			return parseGetValueReply(resp.Body)
		case <-rctx.Done():
			cancel()
			cancelReply()
			continue
		}
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// getRange reads a key range [begin, end), fetching all overlapping shard locations
// at once and iterating them in scan order. Matches C++ getExactRange in
// NativeAPI.actor.cpp: re-queries same shard on more=true (no re-locate),
// invalidates entire remaining range on wrong_shard_server, and passes reverse
// to getKeyRangeLocations so the proxy returns shards in the right order.
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	const getRangeShardLimit = 100 // C++ CLIENT_KNOBS->GET_RANGE_SHARD_LIMIT

	var allKVs []KeyValue
	remaining := limit
	curBegin := begin
	curEnd := end

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
						// C++ invalidates the entire remaining range, not just one key.
						// This handles shard splits that affect multiple adjacent entries.
						if reverse {
							tx.db.locCache.invalidateRange(curBegin, shardEnd, tx.tenantId)
							curEnd = shardEnd
						} else {
							tx.db.locCache.invalidateRange(shardBegin, curEnd, tx.tenantId)
							curBegin = shardBegin
						}
						time.Sleep(wrongShardRetryDelay)
						relocated = true
						break // break to outer loop for re-locate
					}
					return nil, false, err
				}

				allKVs = append(allKVs, kvs...)
				remaining -= len(kvs)

				if remaining <= 0 {
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
	// C++ uses negative limit for reverse scans (transformRangeLimits in NativeAPI.actor.cpp:4231).
	wireLimit := int32(limit)
	if reverse {
		wireLimit = -wireLimit
	}
	for _, server := range servers {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleConnError(server.Address)
			continue
		}
		replyToken, replyCh, cancelReply := conn.PrepareReply()
		body := buildGetKeyValuesRequest(begin, end, tx.readVersion, wireLimit, tx.lockAware || tx.readLockAware, tx.tenantId, replyToken, server.Token)
		gkvToken := getAdjustedEndpoint(server.Token, EndpointGetKeyValues)
		if err := conn.SendFrame(gkvToken, body); err != nil {
			cancelReply()
			tx.db.handleConnError(server.Address)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				tx.db.handleConnError(server.Address)
				continue
			}
			return parseGetKeyValuesReply(resp.Body)
		case <-rctx.Done():
			cancel()
			cancelReply()
			continue
		}
	}
	return nil, false, &wire.FDBError{Code: ErrAllAlternativesFailed}
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

func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, replyToken transport.UID, _ transport.UID) []byte {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
		End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
		Version:                version,
		Limit:                  limit,
		LimitBytes:             UnlimitedBytes,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector,
	}
	if lockAware {
		req.HasOptions = true
		req.Options = types.ReadOptions{HasLockAware: true, LockAware: []byte{}}
	}
	return req.MarshalFDB()
}

// parseGetKeyValuesReply parses the ErrorOr-wrapped GetKeyValuesReply.
// Returns (keyValues, more, error).
func parseGetKeyValuesReply(data []byte) ([]KeyValue, bool, error) {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return nil, false, fmt.Errorf("GetKeyValues: %w", err)
	}
	var reply types.GetKeyValuesReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return nil, false, fmt.Errorf("unmarshal GetKeyValuesReply: %w", err)
	}

	kvs := types.ParseKeyValueRefStringVector(reply.Data)
	return kvs, reply.More, nil
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

func buildGetValueRequest(key []byte, version int64, lockAware bool, tenantId int64, replyToken transport.UID, _ transport.UID) []byte {
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
	return req.MarshalFDB()
}

// parseGetValueReply parses the ErrorOr-wrapped GetValueReply.
func parseGetValueReply(data []byte) ([]byte, error) {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return nil, fmt.Errorf("GetValue: %w", err)
	}
	var reply types.GetValueReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return nil, fmt.Errorf("unmarshal GetValueReply: %w", err)
	}
	if !reply.HasValue {
		return nil, nil // key not found
	}
	return reply.Value, nil
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
