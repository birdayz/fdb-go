package client

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

const (
	// ErrWrongShardServer is returned when the storage server no longer owns the key.
	ErrWrongShardServer = 1062
	// wrongShardRetryDelay matches CLIENT_KNOBS->WRONG_SHARD_SERVER_DELAY.
	wrongShardRetryDelay = 10 * time.Millisecond
)

// getValue sends a GetValueRequest to the appropriate storage server.
// Handles wrong_shard_server by invalidating the locality cache and retrying.
func (tx *Transaction) getValue(ctx context.Context, key []byte) ([]byte, error) {
	for attempts := 0; attempts < 5; attempts++ {
		servers, err := tx.db.locationCache.Locate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		// Try each server (simple failover, no load balancing yet).
		for _, server := range servers {
			conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
			if err != nil {
				continue
			}

			replyToken, replyCh := conn.PrepareReply()
			body := buildGetValueRequest(key, tx.readVersion, replyToken, server.Token)
			// Use the server token directly (getValue = base endpoint, index 0 in batch)
			if err := conn.SendFrame(server.Token, body); err != nil {
				continue
			}

			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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

		// All servers failed — possibly wrong shard. Invalidate and retry.
		tx.db.locationCache.Invalidate(key)
		time.Sleep(wrongShardRetryDelay)
	}

	return nil, fmt.Errorf("getValue: all attempts failed for key")
}

// getRange sends a GetKeyValuesRequest for a key range.
// Returns the key-value pairs and a boolean indicating whether there are more results.
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	for attempts := 0; attempts < 5; attempts++ {
		servers, err := tx.db.locationCache.Locate(ctx, begin)
		if err != nil {
			return nil, false, fmt.Errorf("locate range begin: %w", err)
		}
		if len(servers) == 0 {
			return nil, false, fmt.Errorf("no storage servers for range")
		}

		// Try each server (simple failover).
		for _, server := range servers {
			conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
			if err != nil {
				continue
			}

			replyToken, replyCh := conn.PrepareReply()
			body := buildGetKeyValuesRequest(begin, end, tx.readVersion, int32(limit), replyToken, server.Token)

			// getKeyValues is at endpoint index 2 in StorageServerInterface:
			//   getValue=0, getKey=1, getKeyValues=2
			gkvToken := getAdjustedEndpoint(server.Token, 2)
			if err := conn.SendFrame(gkvToken, body); err != nil {
				continue
			}

			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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

		// All servers failed — possibly wrong shard. Invalidate and retry.
		tx.db.locationCache.Invalidate(begin)
		time.Sleep(wrongShardRetryDelay)
	}

	return nil, false, fmt.Errorf("getRange: all attempts failed")
}

// buildGetKeyValuesRequest uses WriteMessageWithVTables with the generated closure.
func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, replyToken transport.UID, _ transport.UID) []byte {
	vt := types.GetKeyValuesRequestVTable
	fileID := types.GetKeyValuesRequestFileID
	w := wire.NewWriter(nil)
	return w.WriteMessageWithVTables(fileID, vt, 8, types.GetKeyValuesRequestVTableClosure, func(obj *wire.ObjectWriter) {
		tenantVT := types.TenantInfoVTable
		obj.WriteStruct(int(vt[11]), tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1)
		})
		obj.WriteStruct(int(vt[10]), types.SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
		replyVT := types.ReplyPromiseVTable
		obj.WriteStruct(int(vt[9]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
		obj.WriteStruct(int(vt[3]), types.KeySelectorRefVTable, 4, func(inner *wire.ObjectWriter) {
			inner.WriteBytes(4, end)
			inner.WriteInt32(8, 1) // firstGreaterOrEqual
		})
		obj.WriteStruct(int(vt[2]), types.KeySelectorRefVTable, 4, func(inner *wire.ObjectWriter) {
			inner.WriteBytes(4, begin)
			inner.WriteInt32(8, 1) // firstGreaterOrEqual
		})
		obj.WriteInt64(int(vt[4]), version)
		obj.WriteInt32(int(vt[5]), limit)
		obj.WriteInt32(int(vt[6]), 0x7FFFFFFF)          // limitBytes
		obj.WriteBytes(int(vt[14]), emptyVersionVector) // ssLatestCommitVersions (16 bytes)
	})
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
// sizeof(size_t) + sizeof(Version) = 16 bytes (utlCount=0, maxVersion=invalidVersion).
var emptyVersionVector = make([]byte, 16)

// buildGetValueRequest uses WriteMessageWithVTables with the generated vtable closure.
func buildGetValueRequest(key []byte, version int64, replyToken transport.UID, _ transport.UID) []byte {
	vt := types.GetValueRequestVTable
	fileID := types.GetValueRequestFileID
	w := wire.NewWriter(nil)
	return w.WriteMessageWithVTables(fileID, vt, 8, types.GetValueRequestVTableClosure, func(obj *wire.ObjectWriter) {
		tenantVT := types.TenantInfoVTable
		obj.WriteStruct(int(vt[8]), tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1)
		})
		obj.WriteStruct(int(vt[7]), types.SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
		replyVT := types.ReplyPromiseVTable
		obj.WriteStruct(int(vt[6]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
		obj.WriteInt64(int(vt[3]), version)
		obj.WriteBytes(int(vt[2]), key)
		obj.WriteBytes(int(vt[11]), emptyVersionVector) // 16 bytes, not nil
	})
}

// parseGetValueReply parses the ErrorOr-wrapped GetValueReply.
func parseGetValueReply(data []byte) ([]byte, error) {
	r, err := wire.ReadErrorOr(data)
	if err != nil {
		return nil, fmt.Errorf("GetValue: %w", err)
	}
	var reply types.GetValueReply
	reply.UnmarshalFrom(r)
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
