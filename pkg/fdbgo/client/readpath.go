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

// C++ ground truth template for GetKeyValuesRequest (304 bytes, from FDB 7.3.75 ObjectWriter).
// Default values: empty begin/end keys, version=0, limit=0, limitBytes=0.
var getKeyValuesRequestTemplate, _ = hex.DecodeString("64000000e2b16700000012001400040010001100080012000c0013000a001d00040014001c000a000d0004000c0008001e0036000c00100004001400180034001c0020002400280035002c0030000a001100040010000c0006001400040006000800040006000000040000003c0000005070fa1300000000a4000000900000000564417c9a7f000000000000680000004400000028000000000000000c0000000000000000000000100000000000000000000000ffffffffffffffff6e000000ffffffffffffffff000000000000000000000000b8000000000000000000000000000000000000000000000000000000000000009c0000004a4a5ff66661c8f603000000e42aabcf00000000e60000001c0000000000000000000000f60000000c000000000000000000000000000000")

// buildGetKeyValuesRequest patches the C++ template with our values.
// Template layout (from decode):
//
//	Message at byte 108:  version at +4(=112), limit at +20(=128), limitBytes at +24(=132)
//	Reply at byte 244:    UID at +4(=248)
//	Begin KSR at byte 284: key RelOff at +4(=288), offset(int32) at +8(=292)
//	End KSR at byte 268:   key RelOff at +4(=272), offset(int32) at +8(=276)
//	Shared key [len=0] at byte 300
func buildGetKeyValuesRequest(begin, end []byte, version int64, limit int32, replyToken transport.UID, _ transport.UID) []byte {
	buf := make([]byte, len(getKeyValuesRequestTemplate))
	copy(buf, getKeyValuesRequestTemplate)

	// Message fields.
	binary.LittleEndian.PutUint64(buf[112:], uint64(version))
	binary.LittleEndian.PutUint32(buf[128:], uint32(limit))
	binary.LittleEndian.PutUint32(buf[132:], 0x7FFFFFFF) // limitBytes = INT_MAX (unlimited)

	// Reply token.
	binary.LittleEndian.PutUint64(buf[248:], replyToken.First)
	binary.LittleEndian.PutUint64(buf[256:], replyToken.Second)

	// Begin KeySelectorRef: offset=1 (firstGreaterOrEqual), orEqual=false.
	binary.LittleEndian.PutUint32(buf[292:], 1) // offset field

	// End KeySelectorRef: offset=1 (firstGreaterOrEqual), orEqual=false.
	binary.LittleEndian.PutUint32(buf[276:], 1) // offset field

	// Append begin key data at end (template has empty keys).
	if len(begin) > 0 {
		beginOOL := make([]byte, 4+len(begin))
		binary.LittleEndian.PutUint32(beginOOL, uint32(len(begin)))
		copy(beginOOL[4:], begin)
		if pad := (4 - len(beginOOL)%4) % 4; pad > 0 {
			beginOOL = append(beginOOL, make([]byte, pad)...)
		}
		beginOOLStart := len(buf)
		buf = append(buf, beginOOL...)
		// Update Begin KSR key RelOff at byte 288.
		binary.LittleEndian.PutUint32(buf[288:], uint32(beginOOLStart-288))
	}

	// Append end key data.
	if len(end) > 0 {
		endOOL := make([]byte, 4+len(end))
		binary.LittleEndian.PutUint32(endOOL, uint32(len(end)))
		copy(endOOL[4:], end)
		if pad := (4 - len(endOOL)%4) % 4; pad > 0 {
			endOOL = append(endOOL, make([]byte, pad)...)
		}
		endOOLStart := len(buf)
		buf = append(buf, endOOL...)
		// Update End KSR key RelOff at byte 272.
		binary.LittleEndian.PutUint32(buf[272:], uint32(endOOLStart-272))
	}

	return buf
}

// parseGetKeyValuesReply parses the ErrorOr-wrapped GetKeyValuesReply.
// Returns (keyValues, more, error).
func parseGetKeyValuesReply(data []byte) ([]KeyValue, bool, error) {
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse GetKeyValues reply: %w", err)
	}

	// ErrorOr flattened by FakeRoot: <=1 fields = Error, >1 = GetKeyValuesReply.
	nfields := r.VTableLength() - 2
	if nfields <= 1 {
		if r.FieldPresent(0) {
			errCode := r.ReadInt32(0)
			return nil, false, &FDBError{Code: int(errCode), Message: fmt.Sprintf("GetKeyValues error %d", errCode)}
		}
		return nil, false, fmt.Errorf("empty GetKeyValues response")
	}

	// Parse with the generated UnmarshalFDB.
	var reply protocol.GetKeyValuesReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return nil, false, fmt.Errorf("unmarshal GetKeyValuesReply: %w", err)
	}

	kvs := parseKeyValueVector(reply.Data)
	return kvs, reply.More, nil
}

// parseKeyValueVector parses a VectorRef<KeyValueRef> with VecSerStrategy::String.
// The data (after ReadBytes follows the RelOff and strips the length prefix) is:
// [count(4)][elem0][elem1]...
// Each element: [key_len(4)][key_data][value_len(4)][value_data]
func parseKeyValueVector(data []byte) []KeyValue {
	if len(data) < 4 {
		return nil
	}
	count := binary.LittleEndian.Uint32(data[0:4])
	if count == 0 {
		return nil
	}
	pos := 4
	result := make([]KeyValue, 0, count)
	for i := uint32(0); i < count; i++ {
		if pos+4 > len(data) {
			break
		}
		keyLen := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 4
		if pos+keyLen > len(data) {
			break
		}
		key := make([]byte, keyLen)
		copy(key, data[pos:pos+keyLen])
		pos += keyLen

		if pos+4 > len(data) {
			break
		}
		valLen := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 4
		if pos+valLen > len(data) {
			break
		}
		val := make([]byte, valLen)
		copy(val, data[pos:pos+valLen])
		pos += valLen

		result = append(result, KeyValue{Key: key, Value: val})
	}
	return result
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

// getAdjustedEndpoint computes the endpoint token for interface method at given index.
// C++ Endpoint::getAdjustedEndpoint(n): first += (n << 32), second.lower32 += n.
func getAdjustedEndpoint(base transport.UID, index int) transport.UID {
	baseIndex := uint32(base.Second)
	return transport.UID{
		First:  base.First + (uint64(index) << 32),
		Second: (base.Second & 0xFFFFFFFF00000000) | uint64(baseIndex+uint32(index)),
	}
}

// Ensure imports are used
var _ = binary.LittleEndian
