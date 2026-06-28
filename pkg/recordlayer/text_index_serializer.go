package recordlayer

import (
	"bytes"
	"fmt"
	"math/bits"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// BunchedSerializer serializes keys and values for BunchedMap storage.
// Keys must serialize in a way that preserves sort order (unsigned lexicographic).
// Matches Java's com.apple.foundationdb.map.BunchedSerializer interface.
type BunchedSerializer[K any, V any] interface {
	SerializeKey(key K) []byte
	SerializeEntry(key K, value V) ([]byte, error)
	SerializeEntries(entries []BunchedEntry[K, V]) ([]byte, error)
	DeserializeKey(data []byte, offset, length int) (K, error)
	DeserializeEntries(key K, data []byte) ([]BunchedEntry[K, V], error)
	DeserializeKeys(key K, data []byte) ([]K, error)
	CanAppend() bool
}

// BunchedEntry is a key-value pair in a BunchedMap.
type BunchedEntry[K any, V any] struct {
	Key   K
	Value V
}

// textIndexBunchedSerializerPrefix is the version prefix for the TEXT index serialization format.
// Matches Java's TextIndexBunchedSerializer.PREFIX = { 0x20 }.
var textIndexBunchedSerializerPrefix = []byte{0x20}

// TextIndexBunchedSerializer serializes Tuple keys and position lists ([]int) for TEXT indexes.
// Position lists are delta-compressed using base-128 variable-length integer encoding.
// Wire-compatible with Java's com.apple.foundationdb.record.provider.foundationdb.indexes.TextIndexBunchedSerializer.
type TextIndexBunchedSerializer struct{}

var textIndexBunchedSerializerInstance = &TextIndexBunchedSerializer{}

// TextIndexBunchedSerializerInstance returns the singleton serializer.
func TextIndexBunchedSerializerInstance() *TextIndexBunchedSerializer {
	return textIndexBunchedSerializerInstance
}

// varIntSize returns the number of bytes needed to encode val using base-128 varint.
// Matches Java's getVarIntSize().
func varIntSize(val int) int {
	if val == 0 {
		return 1
	}
	// (32 - leadingZeros + 6) / 7
	return (bits.Len(uint(val)) + 6) / 7
}

// serializeVarInt writes val as a base-128 variable-length integer.
// MSB of each byte is 1 if more bytes follow, 0 for the last byte.
// Matches Java's serializeVarInt().
func serializeVarInt(buf *bytes.Buffer, val int) {
	if val == 0 {
		buf.WriteByte(0x00)
		return
	}
	numBytes := varIntSize(val)
	for i := numBytes - 1; i >= 0; i-- {
		b := byte((val >> (7 * i)) & 0x7f)
		if i != 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
}

// deserializeVarInt reads a base-128 varint from the buffer.
// Returns the decoded value and any error.
func deserializeVarInt(buf *bytes.Reader) (int, error) {
	val := 0
	for {
		b, err := buf.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("unexpected end of varint data: %w", err)
		}
		val = (val << 7) | int(b&0x7f)
		if b&0x80 == 0 {
			break
		}
	}
	return val, nil
}

// positionListSize returns the serialized size of a delta-compressed position list
// (not including the list size prefix). Validates monotonic non-negative.
func positionListSize(list []int) (int, error) {
	sum := 0
	last := 0
	for _, val := range list {
		if val < 0 || val < last {
			return 0, fmt.Errorf("position list is not monotonically increasing non-negative integers: %v", list)
		}
		sum += varIntSize(val - last)
		last = val
	}
	return sum, nil
}

// serializePositionList writes the list size (varint) followed by delta-compressed entries.
func serializePositionList(buf *bytes.Buffer, list []int, serializedSize int) {
	serializeVarInt(buf, serializedSize)
	last := 0
	for _, val := range list {
		serializeVarInt(buf, val-last)
		last = val
	}
}

// deserializePositionList reads a delta-compressed position list from the buffer.
func deserializePositionList(buf *bytes.Reader) ([]int, error) {
	serializedSize, err := deserializeVarInt(buf)
	if err != nil {
		return nil, fmt.Errorf("reading list size: %w", err)
	}
	if serializedSize == 0 {
		return []int{}, nil // empty list (non-nil, matching Java's Collections.emptyList())
	}
	if serializedSize < 0 || serializedSize > buf.Len() {
		return nil, fmt.Errorf("position list size %d exceeds remaining data %d", serializedSize, buf.Len())
	}
	// serializedSize is an upper bound on the number of entries (exact if all varints are 1 byte).
	result := make([]int, 0, serializedSize)
	startPos := buf.Len()
	last := 0
	for startPos-buf.Len() < serializedSize {
		delta, err := deserializeVarInt(buf)
		if err != nil {
			return nil, fmt.Errorf("reading position delta: %w", err)
		}
		last += delta
		result = append(result, last)
	}
	return result, nil
}

// SerializeKey packs a key using standard Tuple encoding.
// Matches Java's TextIndexBunchedSerializer.serializeKey().
func (s *TextIndexBunchedSerializer) SerializeKey(key tuple.Tuple) []byte {
	return key.Pack()
}

// SerializeEntry packs a single key-value pair (for appending to a bunch).
// Format: varInt(keyLen) + keyBytes + varInt(listSize) + deltaCompressedList
// Matches Java's TextIndexBunchedSerializer.serializeEntry().
func (s *TextIndexBunchedSerializer) SerializeEntry(key tuple.Tuple, value []int) ([]byte, error) {
	serializedKey := key.Pack()
	listSize, err := positionListSize(value)
	if err != nil {
		return nil, fmt.Errorf("text index serializer: serialize entry: %w", err)
	}

	totalSize := varIntSize(len(serializedKey)) + len(serializedKey) + varIntSize(listSize) + listSize
	buf := bytes.NewBuffer(make([]byte, 0, totalSize))
	serializeVarInt(buf, len(serializedKey))
	buf.Write(serializedKey)
	serializePositionList(buf, value, listSize)
	return buf.Bytes(), nil
}

// SerializeEntries packs an entry list into a single byte array.
// Format: PREFIX + entries (first entry's key omitted).
// Matches Java's TextIndexBunchedSerializer.serializeEntries().
func (s *TextIndexBunchedSerializer) SerializeEntries(entries []BunchedEntry[tuple.Tuple, []int]) ([]byte, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("text index serializer: cannot serialize empty entry list")
	}

	// Calculate total size.
	size := len(textIndexBunchedSerializerPrefix)
	serializedKeys := make([][]byte, 0, len(entries)-1)
	listSizes := make([]int, len(entries))

	for i, entry := range entries {
		if i != 0 {
			sk := entry.Key.Pack()
			size += varIntSize(len(sk)) + len(sk)
			serializedKeys = append(serializedKeys, sk)
		}
		ls, err := positionListSize(entry.Value)
		if err != nil {
			return nil, fmt.Errorf("text index serializer: serialize entries: %w", err)
		}
		listSizes[i] = ls
		size += varIntSize(ls) + ls
	}

	buf := bytes.NewBuffer(make([]byte, 0, size))
	buf.Write(textIndexBunchedSerializerPrefix)
	for i, entry := range entries {
		if i != 0 {
			sk := serializedKeys[i-1]
			serializeVarInt(buf, len(sk))
			buf.Write(sk)
		}
		serializePositionList(buf, entry.Value, listSizes[i])
	}
	return buf.Bytes(), nil
}

// DeserializeKey unpacks a Tuple key from data[offset:offset+length].
// Matches Java's TextIndexBunchedSerializer.deserializeKey().
func (s *TextIndexBunchedSerializer) DeserializeKey(data []byte, offset, length int) (tuple.Tuple, error) {
	if offset < 0 || offset > len(data) || length < 0 || offset+length > len(data) {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("offset (%d) or length (%d) out of range (%d)", offset, length, len(data)),
			Data:    data,
		}
	}
	t, err := fastUnpack(data[offset : offset+length])
	if err != nil {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("unable to deserialize key: %v", err),
			Data:    data[offset : offset+length],
		}
	}
	return t, nil
}

// DeserializeEntries deserializes a bunch of entries from data, using key as the first entry's key.
// Matches Java's TextIndexBunchedSerializer.deserializeEntries().
func (s *TextIndexBunchedSerializer) DeserializeEntries(key tuple.Tuple, data []byte) ([]BunchedEntry[tuple.Tuple, []int], error) {
	return s.deserializeBunch(key, data, true)
}

// DeserializeKeys deserializes only the keys from a bunch (skipping values for efficiency).
// Matches Java's TextIndexBunchedSerializer.deserializeKeys().
func (s *TextIndexBunchedSerializer) DeserializeKeys(key tuple.Tuple, data []byte) ([]tuple.Tuple, error) {
	entries, err := s.deserializeBunch(key, data, false)
	if err != nil {
		return nil, err
	}
	keys := make([]tuple.Tuple, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys, nil
}

// CanAppend returns true — this format supports appending entries without re-serialization.
func (s *TextIndexBunchedSerializer) CanAppend() bool {
	return true
}

// deserializeBunch is the shared implementation for DeserializeEntries and DeserializeKeys.
func (s *TextIndexBunchedSerializer) deserializeBunch(key tuple.Tuple, data []byte, deserializeValues bool) ([]BunchedEntry[tuple.Tuple, []int], error) {
	if !bytes.HasPrefix(data, textIndexBunchedSerializerPrefix) {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("data begins with incorrect prefix: %x", data[:min(len(data), 4)]),
			Data:    data,
		}
	}

	buf := bytes.NewReader(data[len(textIndexBunchedSerializerPrefix):])
	var entries []BunchedEntry[tuple.Tuple, []int]
	first := true

	for buf.Len() > 0 {
		var entryKey tuple.Tuple
		if !first {
			tupleSize, err := deserializeVarInt(buf)
			if err != nil {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("reading tuple size: %v", err),
					Data:    data,
				}
			}
			if tupleSize < 0 || tupleSize > buf.Len() {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("tuple size %d exceeds remaining data %d", tupleSize, buf.Len()),
					Data:    data,
				}
			}
			if tupleSize == 0 {
				entryKey = tuple.Tuple{}
			} else {
				keyBytes := make([]byte, tupleSize)
				if _, err := buf.Read(keyBytes); err != nil {
					return nil, &BunchedSerializationError{
						Message: fmt.Sprintf("reading key bytes: %v", err),
						Data:    data,
					}
				}
				entryKey, err = fastUnpack(keyBytes)
				if err != nil {
					return nil, &BunchedSerializationError{
						Message: fmt.Sprintf("unpacking key: %v", err),
						Data:    data,
					}
				}
			}
		} else {
			entryKey = key
			first = false
		}

		var entryValue []int
		if deserializeValues {
			var err error
			entryValue, err = deserializePositionList(buf)
			if err != nil {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("deserializing position list: %v", err),
					Data:    data,
				}
			}
		} else {
			// Skip value: read list size, then skip that many bytes.
			listSize, err := deserializeVarInt(buf)
			if err != nil {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("reading list size for skip: %v", err),
					Data:    data,
				}
			}
			if listSize < 0 || listSize > buf.Len() {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("list size %d exceeds remaining data %d", listSize, buf.Len()),
					Data:    data,
				}
			}
			skipBytes := make([]byte, listSize)
			if _, err := buf.Read(skipBytes); err != nil {
				return nil, &BunchedSerializationError{
					Message: fmt.Sprintf("skipping value bytes: %v", err),
					Data:    data,
				}
			}
		}

		entries = append(entries, BunchedEntry[tuple.Tuple, []int]{Key: entryKey, Value: entryValue})
	}

	return entries, nil
}

// Compile-time check that TextIndexBunchedSerializer implements BunchedSerializer.
var _ BunchedSerializer[tuple.Tuple, []int] = (*TextIndexBunchedSerializer)(nil)

// packVarInt is a convenience for encoding a single varint to bytes (used in tests).
func packVarInt(val int) []byte {
	buf := &bytes.Buffer{}
	serializeVarInt(buf, val)
	return buf.Bytes()
}

// unpackVarInt is a convenience for decoding a single varint from bytes (used in tests).
func unpackVarInt(data []byte) (int, error) {
	return deserializeVarInt(bytes.NewReader(data))
}

// BunchedSerializationError is returned when bunched map serialization/deserialization fails.
type BunchedSerializationError struct {
	Message string
	Data    []byte
}

func (e *BunchedSerializationError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("bunched serialization error: %s (data len=%d)", e.Message, len(e.Data))
	}
	return fmt.Sprintf("bunched serialization error: %s", e.Message)
}

// bytesToHex returns a hex string for debugging.
func bytesToHex(b []byte) string {
	return fmt.Sprintf("%x", b)
}

// BunchedMapException is the error type for BunchedMap operations.
type BunchedMapException struct {
	Message string
}

func (e *BunchedMapException) Error() string {
	return fmt.Sprintf("bunched map error: %s", e.Message)
}
