package types

import "encoding/binary"

// KeyValue is a key-value pair from a VectorRef<KeyValueRef>.
type KeyValue struct {
	Key   []byte
	Value []byte
}

// ParseKeyValueVector decodes a VectorRef<KeyValueRef> with VecSerStrategy::String.
// Wire format: [count(4)][key_len(4)][key_data][value_len(4)][value_data]...
func ParseKeyValueVector(data []byte) []KeyValue {
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
