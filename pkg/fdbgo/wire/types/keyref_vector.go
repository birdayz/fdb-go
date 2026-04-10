package types

import "encoding/binary"

// ParseKeyRefStringVector decodes a VectorRef<KeyRef, VecSerStrategy::String>.
// Wire format: [count(4)][keylen(4)][key]...
func ParseKeyRefStringVector(data []byte) [][]byte {
	if len(data) < 4 {
		return nil
	}
	count := binary.LittleEndian.Uint32(data[0:4])
	if count == 0 {
		return nil
	}
	pos := 4
	// Clamp capacity to prevent OOM from crafted count values.
	// Each element needs at least 4 bytes (length prefix).
	maxElems := uint32((len(data) - pos) / 4)
	if count > maxElems {
		count = maxElems
	}
	result := make([][]byte, 0, count)
	for i := uint32(0); i < count; i++ {
		if pos+4 > len(data) {
			break
		}
		n := int(binary.LittleEndian.Uint32(data[pos:]))
		pos += 4
		if n < 0 || pos+n > len(data) {
			break
		}
		result = append(result, data[pos:pos+n:pos+n])
		pos += n
	}
	return result
}
