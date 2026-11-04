// Portions derived from FoundationDB Record Layer (
// TextIndexBunchedSerializer.java, BunchedSerializer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"fmt"
	"math/bits"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

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

// appendVarInt appends Java's most-significant-group-first base-128 varint.
func appendVarInt(dst []byte, val int) []byte {
	if val == 0 {
		return append(dst, 0x00)
	}
	for i := varIntSize(val) - 1; i >= 0; i-- {
		b := byte((val >> (7 * i)) & 0x7f)
		if i != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
	}
	return dst
}

// appendPositionList appends the byte size followed by delta-compressed positions.
func appendPositionList(dst []byte, list []int, serializedSize int) []byte {
	dst = appendVarInt(dst, serializedSize)
	last := 0
	for _, val := range list {
		dst = appendVarInt(dst, val-last)
		last = val
	}
	return dst
}

var errVarIntEOF = fmt.Errorf("unexpected end of varint data: EOF")

// readVarInt reads a varint from data[pos:]: the value, the offset after
// it, and whether its encoding is the one appendVarInt writes.
func readVarInt(data []byte, pos int) (val, next int, canonical bool, err error) {
	start := pos
	for pos < len(data) {
		b := data[pos]
		pos++
		val = (val << 7) | int(b&0x7f)
		if b&0x80 == 0 {
			return val, pos, pos-start == varIntSize(val), nil
		}
	}
	return 0, pos, false, errVarIntEOF
}

// walkPositionList reads the position list at data[pos:], appending its
// positions to out when out is not nil, and returns the offset after it and
// its number of positions. canonical reports whether appendPositionList
// writes the list's positions to exactly these bytes.
func walkPositionList(data []byte, pos int, out *[]int) (next, count int, canonical bool, err error) {
	serializedSize, pos, canonical, err := readVarInt(data, pos)
	if err != nil {
		return 0, 0, false, fmt.Errorf("reading list size: %w", err)
	}
	if serializedSize == 0 {
		return pos, 0, canonical, nil
	}
	if serializedSize < 0 || serializedSize > len(data)-pos {
		return 0, 0, false, fmt.Errorf("position list size %d exceeds remaining data %d", serializedSize, len(data)-pos)
	}
	start, last := pos, 0
	for pos-start < serializedSize {
		delta, next, minimal, err := readVarInt(data, pos)
		if err != nil {
			return 0, 0, false, fmt.Errorf("reading position delta: %w", err)
		}
		pos, canonical = next, canonical && minimal
		last += delta
		count++
		if out != nil {
			*out = append(*out, last)
		}
	}
	// A last delta read past the declared size is re-encoded with the size of
	// its deltas.
	return pos, count, canonical && pos-start == serializedSize, nil
}

// countBunch counts the entries of a serialized bunch and, with values, their
// positions, following deserializeBunch's framing. It stops at the first
// malformed byte; deserializeBunch reports it.
func countBunch(data []byte, values bool) (entries, positions int) {
	pos := len(textIndexBunchedSerializerPrefix)
	for first := true; pos < len(data); first = false {
		if !first {
			tupleSize, next, _, err := readVarInt(data, pos)
			if err != nil || tupleSize < 0 || tupleSize > len(data)-next {
				return entries, positions
			}
			pos = next + tupleSize
		}
		if values {
			next, count, _, err := walkPositionList(data, pos, nil)
			if err != nil {
				return entries, positions
			}
			pos, positions = next, positions+count
		} else {
			listSize, next, _, err := readVarInt(data, pos)
			if err != nil || listSize < 0 || listSize > len(data)-next {
				return entries, positions
			}
			pos = next + listSize
		}
		entries++
	}
	return entries, positions
}

// bunchItem is one entry of a bunch as BunchedMap's put and remove handle it.
//
// key is the entry's packed tuple. Java decodes each key with
// Tuple.fromBytes, which keeps the bytes it decoded (Tuple.java fromBytes:
// t.packed = packed); Tuple.compareTo then compares those bytes, and
// serializeEntries writes them back unchanged, since Tuple.pack returns them.
// The tuple's elements are never needed, so they are validated, not decoded.
//
// list is the entry's stored position list bytes, size varint included. Java
// decodes every list and serializeEntries encodes it again, which gives back
// the stored bytes when they are canonical, so a canonical list is kept as
// stored; otherwise list is nil and positions holds the list, as for a new
// entry.
type bunchItem struct {
	key       []byte
	list      []byte
	positions []int
}

// itemListSize is the serialized size of the item's position list.
func itemListSize(item *bunchItem) (int, error) {
	if item.list != nil {
		return len(item.list), nil
	}
	ls, err := positionListSize(item.positions)
	if err != nil {
		return 0, err
	}
	return varIntSize(ls) + ls, nil
}

func appendItemList(dst []byte, item *bunchItem) []byte {
	if item.list != nil {
		return append(dst, item.list...)
	}
	ls, _ := positionListSize(item.positions) // checked by itemListSize
	return appendPositionList(dst, item.positions, ls)
}

// itemPositions is the item's position list.
func itemPositions(item *bunchItem) ([]int, error) {
	if item.list == nil {
		return item.positions, nil
	}
	positions := []int{} // Java's Collections.emptyList() for an empty list
	if _, _, _, err := walkPositionList(item.list, 0, &positions); err != nil {
		return nil, err
	}
	return positions, nil
}

// bunchItemsSize is the size serializeEntries gives items. It fails where
// serializeEntries does: on no items or a position list that is not
// monotonically increasing non-negative integers.
func bunchItemsSize(items []bunchItem) (int, error) {
	if len(items) == 0 {
		return 0, fmt.Errorf("text index serializer: cannot serialize empty entry list")
	}
	size := len(textIndexBunchedSerializerPrefix)
	for i := range items {
		if i != 0 {
			size += varIntSize(len(items[i].key)) + len(items[i].key)
		}
		ls, err := itemListSize(&items[i])
		if err != nil {
			return 0, fmt.Errorf("text index serializer: serialize entries: %w", err)
		}
		size += ls
	}
	return size, nil
}

// encodeBunchItems is serializeEntries over items, into one allocation of
// the size bunchItemsSize returned.
func encodeBunchItems(items []bunchItem, size int) []byte {
	out := make([]byte, 0, size)
	out = append(out, textIndexBunchedSerializerPrefix...)
	for i := range items {
		if i != 0 {
			out = appendVarInt(out, len(items[i].key))
			out = append(out, items[i].key...)
		}
		out = appendItemList(out, &items[i])
	}
	return out
}

// serializeBunchItems is serializeEntries over items.
func serializeBunchItems(items []bunchItem) ([]byte, error) {
	size, err := bunchItemsSize(items)
	if err != nil {
		return nil, err
	}
	return encodeBunchItems(items, size), nil
}

// serializeBunchItem is serializeEntry: the bytes that append item to a bunch.
func serializeBunchItem(item *bunchItem) ([]byte, error) {
	ls, err := itemListSize(item)
	if err != nil {
		return nil, fmt.Errorf("text index serializer: serialize entry: %w", err)
	}
	out := make([]byte, 0, varIntSize(len(item.key))+len(item.key)+ls)
	out = appendVarInt(out, len(item.key))
	out = append(out, item.key...)
	return appendItemList(out, item), nil
}

// decodeBunchItems is deserializeEntries keeping each key's packed bytes and
// each canonical list's bytes, slices of firstKey and data, appended to items.
// It fails where deserializeEntries does: firstKey is the signpost key Java
// decodes with deserializeKey, and each later key is unpacked by Java.
func decodeBunchItems(firstKey, data []byte, items []bunchItem) ([]bunchItem, error) {
	if err := tuplefast.Validate(firstKey); err != nil {
		return items, &BunchedSerializationError{Message: fmt.Sprintf("unable to deserialize key: %v", err), Data: firstKey}
	}
	if !bytes.HasPrefix(data, textIndexBunchedSerializerPrefix) {
		return items, &BunchedSerializationError{
			Message: fmt.Sprintf("data begins with incorrect prefix: %x", data[:min(len(data), 4)]),
			Data:    data,
		}
	}
	fail := func(format string, args ...any) error {
		return &BunchedSerializationError{Message: fmt.Sprintf(format, args...), Data: data}
	}
	pos := len(textIndexBunchedSerializerPrefix)
	first := true
	for pos < len(data) {
		var item bunchItem
		if first {
			item.key = firstKey[:len(firstKey):len(firstKey)]
			first = false
		} else {
			tupleSize, next, _, err := readVarInt(data, pos)
			if err != nil {
				return items, fail("reading tuple size: %v", err)
			}
			pos = next
			if tupleSize < 0 || tupleSize > len(data)-pos {
				return items, fail("tuple size %d exceeds remaining data %d", tupleSize, len(data)-pos)
			}
			item.key = data[pos : pos+tupleSize : pos+tupleSize]
			if err := tuplefast.Validate(item.key); err != nil {
				return items, fail("unpacking key: %v", err)
			}
			pos += tupleSize
		}
		next, _, canonical, err := walkPositionList(data, pos, nil)
		if err != nil {
			return items, fail("deserializing position list: %v", err)
		}
		if canonical {
			item.list = data[pos:next:next]
		} else {
			item.positions = []int{}
			if _, _, _, err := walkPositionList(data, pos, &item.positions); err != nil {
				return items, fail("deserializing position list: %v", err)
			}
		}
		pos = next
		items = append(items, item)
	}
	return items, nil
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
	serializedKey := key.PackWithPrefix(nil)
	listSize, err := positionListSize(value)
	if err != nil {
		return nil, fmt.Errorf("text index serializer: serialize entry: %w", err)
	}

	totalSize := varIntSize(len(serializedKey)) + len(serializedKey) + varIntSize(listSize) + listSize
	out := make([]byte, 0, totalSize)
	out = appendVarInt(out, len(serializedKey))
	out = append(out, serializedKey...)
	return appendPositionList(out, value, listSize), nil
}

// SerializeEntries packs an entry list into a single byte array.
// Format: PREFIX + entries (first entry's key omitted).
// Matches Java's TextIndexBunchedSerializer.serializeEntries().
func (s *TextIndexBunchedSerializer) SerializeEntries(entries []BunchedEntry[tuple.Tuple, []int]) ([]byte, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("text index serializer: cannot serialize empty entry list")
	}

	// Calculate total size. The keys after the first are packed once, one
	// after another into a scratch buffer; ends[i] is where key i ends in it.
	size := len(textIndexBunchedSerializerPrefix)
	pk := tuple.GetPacker()
	defer tuple.PutPacker(pk)
	var keys []byte
	sizes := make([]int, 2*len(entries))
	ends, listSizes := sizes[:len(entries)], sizes[len(entries):]

	for i, entry := range entries {
		if i != 0 {
			pk.Reset()
			pk.EncodeTuple(entry.Key)
			sk := pk.AppendInto(&keys, nil)
			ends[i] = len(keys)
			size += varIntSize(len(sk)) + len(sk)
		}
		ls, err := positionListSize(entry.Value)
		if err != nil {
			return nil, fmt.Errorf("text index serializer: serialize entries: %w", err)
		}
		listSizes[i] = ls
		size += varIntSize(ls) + ls
	}

	out := make([]byte, 0, size)
	out = append(out, textIndexBunchedSerializerPrefix...)
	for i, entry := range entries {
		if i != 0 {
			sk := keys[ends[i-1]:ends[i]]
			out = appendVarInt(out, len(sk))
			out = append(out, sk...)
		}
		out = appendPositionList(out, entry.Value, listSizes[i])
	}
	return out, nil
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
	t, err := tuplefast.Unpack(data[offset : offset+length])
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

	fail := func(format string, args ...any) error {
		return &BunchedSerializationError{Message: fmt.Sprintf(format, args...), Data: data}
	}
	// The entries and all their positions take one allocation each. Keys are
	// unpacked from data in place; Unpack copies what it keeps.
	nEntries, nPositions := countBunch(data, deserializeValues)
	entries := make([]BunchedEntry[tuple.Tuple, []int], 0, nEntries)
	var arena []int
	if deserializeValues {
		arena = make([]int, 0, nPositions)
	}
	pos := len(textIndexBunchedSerializerPrefix)
	first := true
	for pos < len(data) {
		var entryKey tuple.Tuple
		if !first {
			tupleSize, next, _, err := readVarInt(data, pos)
			if err != nil {
				return nil, fail("reading tuple size: %v", err)
			}
			pos = next
			if tupleSize < 0 || tupleSize > len(data)-pos {
				return nil, fail("tuple size %d exceeds remaining data %d", tupleSize, len(data)-pos)
			}
			if tupleSize == 0 {
				entryKey = tuple.Tuple{}
			} else {
				entryKey, err = tuplefast.Unpack(data[pos : pos+tupleSize])
				if err != nil {
					return nil, fail("unpacking key: %v", err)
				}
			}
			pos += tupleSize
		} else {
			entryKey = key
			first = false
		}

		var entryValue []int
		if deserializeValues {
			start := len(arena)
			next, _, _, err := walkPositionList(data, pos, &arena)
			if err != nil {
				return nil, fail("deserializing position list: %v", err)
			}
			pos = next
			// A full slice expression, so appending to one entry's list
			// cannot overwrite the next one's.
			entryValue = arena[start:len(arena):len(arena)]
		} else {
			// Skip value: read list size, then skip that many bytes.
			listSize, next, _, err := readVarInt(data, pos)
			if err != nil {
				return nil, fail("reading list size for skip: %v", err)
			}
			pos = next
			if listSize < 0 || listSize > len(data)-pos {
				return nil, fail("list size %d exceeds remaining data %d", listSize, len(data)-pos)
			}
			pos += listSize
		}

		entries = append(entries, BunchedEntry[tuple.Tuple, []int]{Key: entryKey, Value: entryValue})
	}

	return entries, nil
}

// Compile-time check that TextIndexBunchedSerializer implements BunchedSerializer.
var _ BunchedSerializer[tuple.Tuple, []int] = (*TextIndexBunchedSerializer)(nil)

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

// BunchedMapException is the error type for BunchedMap operations.
type BunchedMapException struct {
	Message string
}

func (e *BunchedMapException) Error() string {
	return fmt.Sprintf("bunched map error: %s", e.Message)
}
