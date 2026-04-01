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
		servers, err := tx.db.locationCache.Locate(ctx, selectorKey)
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
		if isWrongShardServer(err) {
			tx.db.locationCache.Invalidate(selectorKey)
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("getKey: wrong_shard_server after %d attempts", MaxWrongShardRetries)
}

func (tx *Transaction) sendGetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32, servers []ServerInfo) ([]byte, error) {
	for _, server := range servers {
		conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
		if err != nil {
			continue
		}
		replyToken, replyCh := conn.PrepareReply()
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
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				continue
			}
			return parseGetKeyReply(resp.Body)
		case <-rctx.Done():
			cancel()
			continue
		}
	}
	return nil, fmt.Errorf("all servers unreachable")
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
		servers, err := tx.db.locationCache.Locate(ctx, key)
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
		// wrong_shard_server → invalidate cache, retry.
		if isWrongShardServer(err) {
			tx.db.locationCache.Invalidate(key)
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		// Other FDB error → bubble up for Transact retry.
		return nil, err
	}
	return nil, fmt.Errorf("getValue: wrong_shard_server after 5 attempts")
}

func (tx *Transaction) sendGetValue(ctx context.Context, key []byte, servers []ServerInfo) ([]byte, error) {
	for _, server := range servers {
		conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
		if err != nil {
			continue
		}
		replyToken, replyCh := conn.PrepareReply()
		body := buildGetValueRequest(key, tx.readVersion, replyToken, server.Token)
		if err := conn.SendFrame(server.Token, body); err != nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				continue
			}
			return parseGetValueReply(resp.Body)
		case <-rctx.Done():
			cancel()
			continue
		}
	}
	return nil, fmt.Errorf("all servers unreachable")
}

// getRange reads a key range, automatically continuing across shard boundaries.
// Each shard is queried independently; results are concatenated until limit is
// reached or no more data exists.
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	var allKVs []KeyValue
	remaining := limit
	curBegin := begin

	for remaining > 0 {
		kvs, more, err := tx.getRangeOneShard(ctx, curBegin, end, remaining)
		if err != nil {
			return nil, false, err
		}

		allKVs = append(allKVs, kvs...)
		remaining -= len(kvs)

		if remaining <= 0 {
			// Hit our limit. There may be more data.
			return allKVs, more || len(kvs) > 0, nil
		}

		if len(kvs) == 0 {
			// Shard returned nothing for this range — done.
			return allKVs, false, nil
		}

		if !more {
			// Shard exhausted for this range. Continue from next key
			// on the next shard (if the range extends past this shard).
			lastKey := kvs[len(kvs)-1].Key
			curBegin = append(lastKey, 0) // next key after lastKey
			if bytes.Compare(curBegin, end) >= 0 {
				// Past the end of our range — done.
				return allKVs, false, nil
			}
			continue // query next shard
		}

		// more=true but we haven't hit limit: server had more data but
		// we need to continue. Advance past last key.
		lastKey := kvs[len(kvs)-1].Key
		curBegin = append(lastKey, 0)
	}

	return allKVs, remaining <= 0, nil
}

// getRangeOneShard queries a single shard with wrong_shard_server retry.
func (tx *Transaction) getRangeOneShard(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		servers, err := tx.db.locationCache.Locate(ctx, begin)
		if err != nil {
			return nil, false, fmt.Errorf("locate range begin: %w", err)
		}
		if len(servers) == 0 {
			return nil, false, fmt.Errorf("no storage servers for range")
		}

		kvs, more, err := tx.sendGetRange(ctx, begin, end, limit, servers)
		if err == nil {
			return kvs, more, nil
		}
		if isWrongShardServer(err) {
			tx.db.locationCache.Invalidate(begin)
			time.Sleep(wrongShardRetryDelay)
			continue
		}
		return nil, false, err
	}
	return nil, false, fmt.Errorf("getRange: wrong_shard_server after %d attempts", MaxWrongShardRetries)
}

func (tx *Transaction) sendGetRange(ctx context.Context, begin, end []byte, limit int, servers []ServerInfo) ([]KeyValue, bool, error) {
	for _, server := range servers {
		conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
		if err != nil {
			continue
		}
		replyToken, replyCh := conn.PrepareReply()
		body := buildGetKeyValuesRequest(begin, end, tx.readVersion, int32(limit), replyToken, server.Token)
		gkvToken := getAdjustedEndpoint(server.Token, EndpointGetKeyValues)
		if err := conn.SendFrame(gkvToken, body); err != nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				continue
			}
			return parseGetKeyValuesReply(resp.Body)
		case <-rctx.Done():
			cancel()
			continue
		}
	}
	return nil, false, fmt.Errorf("all servers unreachable")
}

// isWrongShardServer returns true if the error is FDB error 1062.
func isWrongShardServer(err error) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == ErrWrongShardServer
}

func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, replyToken transport.UID, _ transport.UID) []byte {
	req := types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: begin, OrEqual: true, Offset: 1},
		End:                    types.KeySelectorRef{Key: end, OrEqual: true, Offset: 1},
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

	tkvs := types.ParseKeyValueVector(reply.Data)
	kvs := make([]KeyValue, len(tkvs))
	for i, kv := range tkvs {
		kvs[i] = KeyValue{Key: kv.Key, Value: kv.Value}
	}
	return kvs, reply.More, nil
}

// KeyValue is a key-value pair returned from reads.
type KeyValue struct {
	Key   []byte
	Value []byte
}

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
