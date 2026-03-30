package client

import (
	"context"
	"encoding/binary"
	"encoding/hex"
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

// C++ ground truth template for GetValueRequest (240 bytes, without version prefix).
var getValueRequestTemplate, _ = hex.DecodeString("5400000082018100000012001400040010001100080012000c0013000a001d00040014001c0018002a000c00040028001000140018001c002900200024000a001100040010000c000600140004000600080004000600000004000000360000000564417c9a7f00008400000000000000640000004000000024000000000000000800000000000000100000000000000000000000ffffffffffffffff5e000000ffffffffffffffff00000000000000000000000098000000000000000000000000000000000000000000000000000000000000008c0000004a4a5ff66661c8f603000000e42aabcf0000000000000000")

// buildGetValueRequest patches the C++ ground truth template with our values.
func buildGetValueRequest(key []byte, version int64, replyToken transport.UID, _ transport.UID) []byte {
	// Start with a copy of the template
	buf := make([]byte, len(getValueRequestTemplate))
	copy(buf, getValueRequestTemplate)

	// Navigate to message object
	root := binary.LittleEndian.Uint32(buf[0:4])
	fr_f0 := root + 4
	msg := int(fr_f0) + int(binary.LittleEndian.Uint32(buf[fr_f0:]))

	// Patch Version at msg+4 (slot 1)
	binary.LittleEndian.PutUint64(buf[msg+4:], uint64(version))

	// Patch Reply token: find the Reply nested struct and update the UID.
	// Reply at msg+20 (slot 4). Follow RelOff to nested struct.
	replyRelOff := binary.LittleEndian.Uint32(buf[msg+20:])
	replyTarget := int(msg+20) + int(replyRelOff)
	// In the nested struct, UID is at offset 4 (vtable {6,20,4})
	binary.LittleEndian.PutUint64(buf[replyTarget+4:], replyToken.First)
	binary.LittleEndian.PutUint64(buf[replyTarget+12:], replyToken.Second)

	// Append key data at the end (the template has empty key)
	// The key RelOff at msg+12 currently points to the end of the template
	// where there's a [length=0] entry. We need to replace it with our key.
	keyOOL := make([]byte, 4+len(key))
	binary.LittleEndian.PutUint32(keyOOL, uint32(len(key)))
	copy(keyOOL[4:], key)
	if pad := (4 - len(keyOOL)%4) % 4; pad > 0 {
		keyOOL = append(keyOOL, make([]byte, pad)...)
	}

	// The template's key RelOff points to the default empty key.
	// Find where the empty key [length=0] is and replace it.
	// For now, just append new key data and update the RelOff.
	keyOOLStart := len(buf)
	buf = append(buf, keyOOL...)
	// Update key RelOff at msg+12
	binary.LittleEndian.PutUint32(buf[msg+12:], uint32(keyOOLStart-(msg+12)))

	return buf
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

	// The inner struct is GetValueReply. The Reader is positioned at it.
	// GetValueReply fields: penalty(float64), error(Optional), value(Optional<Value>), cached(bool)
	// The value is an Optional<Value>. In FlatBuffers, Optional has type tag + value.
	// Value is at some slot — search for it.
	//
	// Actually, let's try parsing with the generated UnmarshalFDB first.
	// It calls NewReader which navigates FakeRoot → message. But our data
	// already went through FakeRoot navigation. So UnmarshalFDB would navigate
	// AGAIN through a "second level" FakeRoot which doesn't exist.
	//
	// Instead, use the Reader we already have.
	// GetValueReply::serialize: serializer(ar, penalty, error, value, cached)
	// slot 0: penalty (float64)
	// slot 1: error (Optional<Error> type tag)
	// slot 2: error value (RelOff)
	// slot 3: value (Optional<Value> type tag)
	// slot 4: value value (RelOff)
	// slot 5: cached (bool)
	//
	// Read value from slot 4 (the value's data RelOff).
	if r.FieldPresent(3) && r.ReadUint8(3) > 0 {
		// Optional<Value> is present. Read the value data.
		valData := r.ReadBytes(4)
		return valData, nil
	}
	// Value not present (key not found)
	return nil, nil
}

// Ensure imports are used
var _ = binary.LittleEndian
