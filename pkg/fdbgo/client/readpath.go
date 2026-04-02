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
		servers, err := tx.db.locCache.locate(tx.db, ctx, selectorKey)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		key, err := tx.sendGetKey(ctx, selectorKey, orEqual, offset, servers)
		if err == nil {
			return key, nil
		}
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(selectorKey)
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
			TenantInfo:             types.TenantInfo{TenantId: NoTenantID},
			SsLatestCommitVersions: emptyVersionVector,
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
		servers, err := tx.db.locCache.locate(tx.db, ctx, key)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		val, err := tx.sendGetValue(ctx, key, servers)
		if err == nil {
			return val, nil
		}
		// wrong_shard_server or all_alternatives_failed → invalidate cache, retry.
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(key)
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
		body := buildGetValueRequest(key, tx.readVersion, replyToken, server.Token)
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

// getRange reads a key range, automatically continuing across shard boundaries.
// Each shard is queried independently; results are concatenated until limit is
// reached or no more data exists. If reverse is true, keys are returned in
// descending order (C++ uses negative limit for reverse scans).
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	var allKVs []KeyValue
	remaining := limit
	curBegin := begin
	curEnd := end

	for remaining > 0 {
		// For reverse scans, locate by end key (we scan backwards).
		locateKey := curBegin
		if reverse {
			// Locate the shard containing the last key in range.
			// Use end-1 conceptually; the shard containing end covers it.
			locateKey = curEnd
			if len(locateKey) > 0 {
				locateKey = append([]byte(nil), curEnd...)
				// Decrement to get into the right shard.
				locateKey[len(locateKey)-1]--
			}
		}
		kvs, more, err := tx.getRangeOneShard(ctx, curBegin, curEnd, remaining, reverse)
		if err != nil {
			return nil, false, err
		}

		allKVs = append(allKVs, kvs...)
		remaining -= len(kvs)

		if remaining <= 0 {
			return allKVs, more || len(kvs) > 0, nil
		}

		if len(kvs) == 0 {
			return allKVs, false, nil
		}

		if !more {
			if reverse {
				// For reverse: advance end backwards past first key returned.
				firstKey := kvs[len(kvs)-1].Key // last in result = smallest key
				curEnd = firstKey
				if bytes.Compare(curEnd, begin) <= 0 {
					return allKVs, false, nil
				}
			} else {
				lastKey := kvs[len(kvs)-1].Key
				curBegin = append(lastKey, 0)
				if bytes.Compare(curBegin, end) >= 0 {
					return allKVs, false, nil
				}
			}
			continue
		}

		if reverse {
			firstKey := kvs[len(kvs)-1].Key
			curEnd = firstKey
		} else {
			lastKey := kvs[len(kvs)-1].Key
			curBegin = append(lastKey, 0)
		}
	}

	return allKVs, remaining <= 0, nil
}

// getRangeOneShard queries a single shard with wrong_shard_server retry.
func (tx *Transaction) getRangeOneShard(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		servers, err := tx.db.locCache.locate(tx.db, ctx, begin)
		if err != nil {
			return nil, false, fmt.Errorf("locate range begin: %w", err)
		}
		if len(servers) == 0 {
			return nil, false, fmt.Errorf("no storage servers for range")
		}

		kvs, more, err := tx.sendGetRange(ctx, begin, end, limit, reverse, servers)
		if err == nil {
			return kvs, more, nil
		}
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(begin)
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		return nil, false, err
	}
	return nil, false, &wire.FDBError{Code: ErrAllAlternativesFailed}
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
		body := buildGetKeyValuesRequest(begin, end, tx.readVersion, wireLimit, replyToken, server.Token)
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

func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, replyToken transport.UID, _ transport.UID) []byte {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
		End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
		Version:                version,
		Limit:                  limit,
		LimitBytes:             UnlimitedBytes,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: -1},
		SsLatestCommitVersions: emptyVersionVector,
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
// C++ VersionVector::getEncodedSize() = sizeof(size_t) + sizeof(Version) = 16.
var emptyVersionVector = make([]byte, 16)

func buildGetValueRequest(key []byte, version int64, replyToken transport.UID, _ transport.UID) []byte {
	req := types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: NoTenantID},
		SsLatestCommitVersions: emptyVersionVector,
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
