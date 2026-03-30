package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, error) {
	servers, err := tx.db.locationCache.Locate(ctx, begin)
	if err != nil {
		return nil, fmt.Errorf("locate range begin: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no storage servers for range")
	}

	server := servers[0]
	conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
	if err != nil {
		return nil, fmt.Errorf("dial storage server: %w", err)
	}

	req := protocol.GetKeyValuesRequest{
		Version: tx.readVersion,
	}
	// TODO: Set begin/end key selectors, limit, limitBytes.
	// These are scalar fields typed as []byte due to codegen gap (base class fields).
	_ = begin
	_ = end
	_ = limit

	body := req.MarshalFDB()

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	replyBody, err := conn.SendAndWait(rctx, server.Token, body)
	if err != nil {
		return nil, fmt.Errorf("getRange RPC: %w", err)
	}

	var reply protocol.GetKeyValuesReply
	if err := reply.UnmarshalFDB(replyBody); err != nil {
		return nil, fmt.Errorf("unmarshal GetKeyValuesReply: %w", err)
	}

	// TODO: Parse reply.Data (VectorRef<KeyValueRef>) into KeyValue slice.
	return nil, nil
}

// KeyValue is a key-value pair returned from reads.
type KeyValue struct {
	Key   []byte
	Value []byte
}

// buildGetValueRequest constructs the request with embedded reply token.
func buildGetValueRequest(key []byte, version int64, replyToken transport.UID, _ transport.UID) []byte {
	vt := protocol.GetValueRequest_VTable
	fileID := protocol.GetValueRequest_FileIdentifier

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 0 (Key): vt[2]
		obj.WriteBytes(int(vt[0+2]), key)
		// slot 1 (Version): vt[3]
		obj.WriteInt64(int(vt[1+2]), version)
		// slot 4 (Reply): vt[6] — nested struct
		replyVT := wire.VTable{6, 20, 4}
		obj.WriteStruct(int(vt[4+2]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
		// slot 5 (SpanContext): vt[7] — nested struct (3 fields, obj=29)
		spanVT := wire.VTable{10, 29, 4, 20, 28}
		obj.WriteStruct(int(vt[5+2]), spanVT, 8, func(inner *wire.ObjectWriter) {
			// Default: all zeros (traceID=0, spanID=0, flags=0)
		})
		// slot 6 (TenantInfo): vt[8] — nested struct (3 fields)
		tenantVT := wire.VTable{10, 17, 4, 16, 12}
		obj.WriteStruct(int(vt[6+2]), tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1) // INVALID_TENANT
		})
	})
}

// parseGetValueReply parses the ErrorOr-wrapped GetValueReply.
func parseGetValueReply(data []byte) ([]byte, error) {
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("parse GetValue reply: %w", err)
	}

	nfields := r.VTableLength() - 2
	if nfields <= 1 {
		if r.FieldPresent(0) {
			errCode := r.ReadInt32(0)
			return nil, fmt.Errorf("FDB GetValue error: code %d", errCode)
		}
		return nil, fmt.Errorf("empty GetValue response")
	}

	// GetValueReply has: penalty(float64), error(Optional), value(Optional<Value>), cached(bool)
	// The value is at some slot. Use the generated UnmarshalFDB to extract.
	var reply protocol.GetValueReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return nil, fmt.Errorf("unmarshal GetValueReply: %w", err)
	}

	return reply.Value, nil
}

// Ensure imports are used
var _ = binary.LittleEndian
