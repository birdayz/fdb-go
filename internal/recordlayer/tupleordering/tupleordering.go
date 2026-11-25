// Portions derived from FoundationDB Record Layer (TupleOrdering.java),
// Copyright 2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package tupleordering is Java's TupleOrdering: tuples packed so that their
// byte order is a chosen direction and NULL placement, the encoding order
// functions store in index keys. The planner's ordered-bytes Values and the
// record layer's key evaluation share it.
package tupleordering

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Direction is Java's TupleOrdering.Direction.
type Direction struct {
	// Inverted is DESC: the packed bytes are inverted.
	Inverted bool
	// CounterflowNulls places NULLs opposite to their natural position.
	CounterflowNulls bool
}

// The four directions of Java's TupleOrdering.Direction.
var (
	AscNullsFirst  = Direction{Inverted: false, CounterflowNulls: false}
	AscNullsLast   = Direction{Inverted: false, CounterflowNulls: true}
	DescNullsFirst = Direction{Inverted: true, CounterflowNulls: true}
	DescNullsLast  = Direction{Inverted: true, CounterflowNulls: false}
)

const nullLastByte = byte(0xFE)

// Pack encodes t in direction dir (TupleOrdering.pack). A tuple holding an
// incomplete versionstamp cannot be packed; the caller refuses it first.
func Pack(t tuple.Tuple, dir Direction) []byte {
	var packed []byte
	if dir.CounterflowNulls {
		packed = PackNullsLast(t)
	} else {
		packed = t.Pack()
	}
	if dir.Inverted {
		packed = Invert(packed)
	}
	return packed
}

// Unpack decodes bytes packed in direction dir (TupleOrdering.unpack).
func Unpack(packed []byte, dir Direction) (tuple.Tuple, error) {
	data := packed
	if dir.Inverted {
		var err error
		data, err = Uninvert(data)
		if err != nil {
			return nil, fmt.Errorf("uninvert: %w", err)
		}
	}
	if dir.CounterflowNulls {
		return UnpackNullsLast(data)
	}
	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, fmt.Errorf("unpack: %w", err)
	}
	return t, nil
}

// PackNullsLast encodes a tuple element-by-element, replacing nil elements
// with a single 0xFE byte (which sorts after all standard tuple type codes).
// Matches Java's TupleOrdering.PackNullsLast().
func PackNullsLast(t tuple.Tuple) []byte {
	var buf []byte
	for _, elem := range t {
		if elem == nil {
			buf = append(buf, nullLastByte)
		} else {
			buf = append(buf, tuple.Tuple{elem}.Pack()...)
		}
	}
	return buf
}

// UnpackNullsLast decodes bytes that were encoded with PackNullsLast.
// 0xFE bytes are decoded as nil elements; other bytes use standard tuple decoding.
// Matches Java's TupleOrdering.UnpackNullsLast().
func UnpackNullsLast(data []byte) (tuple.Tuple, error) {
	var result tuple.Tuple
	pos := 0
	for pos < len(data) {
		if data[pos] == nullLastByte {
			result = append(result, nil)
			pos++
			continue
		}
		endPos, err := ElementEndPos(data, pos)
		if err != nil {
			return nil, fmt.Errorf("at offset %d: %w", pos, err)
		}
		elemTuple, err := tuple.Unpack(data[pos:endPos])
		if err != nil {
			return nil, fmt.Errorf("unpack element at offset %d: %w", pos, err)
		}
		if len(elemTuple) != 1 {
			return nil, fmt.Errorf("expected 1 element at offset %d, got %d", pos, len(elemTuple))
		}
		result = append(result, elemTuple[0])
		pos = endPos
	}
	return result, nil
}

// Invert applies byte-level inversion for DESC ordering.
// Each input byte is bit-flipped (XOR 0xFF), then the flipped bits are packed
// 7 bits per output byte (high bit always 0). A terminator byte with high bit
// set encodes the padding length.
//
// This ensures that when comparing inverted byte sequences lexicographically:
//   - Opposite ordering of original values (due to bit flip)
//   - Shorter strings sort AFTER longer ones with the same prefix (due to 7-bit
//     packing where terminators with high bit 1 sort after data bytes with high bit 0)
//
// Matches Java's TupleOrdering.invert() exactly.
func Invert(input []byte) []byte {
	originalLength := len(input)
	invertedLength := (originalLength*8+6)/7 + 1
	inverted := make([]byte, invertedLength)
	bits := 0
	nbits := 0
	in := 0
	out := 0
	for in < originalLength {
		bits = (bits << 8) | (int(input[in]) ^ 0xFF)
		in++
		nbits += 8
		for nbits >= 7 {
			inverted[out] = byte((bits >> (nbits - 7)) & 0x7F)
			out++
			nbits -= 7
		}
	}
	if nbits == 0 {
		inverted[out] = 0x80
		out++
	} else {
		npad := 7 - nbits
		inverted[out] = byte(((bits << npad) | ((1 << npad) - 1)) & 0x7F)
		out++
		inverted[out] = byte(0x80 | (npad << 4))
		out++
	}
	if out != invertedLength {
		panic(fmt.Sprintf("ordering invert: expected %d bytes, got %d", invertedLength, out))
	}
	return inverted
}

// Uninvert reverses Invert, recovering the original byte sequence.
// Matches Java's TupleOrdering.uninvert() exactly.
func Uninvert(inverted []byte) ([]byte, error) {
	invertedLength := len(inverted)
	if invertedLength == 0 || (inverted[invertedLength-1]&0x80) == 0 {
		return nil, fmt.Errorf("inverted bytes not in expected format")
	}
	npadBits := int((inverted[invertedLength-1] & 0x70) >> 4)
	uninvertedBitLength := (invertedLength-1)*7 - npadBits
	if (uninvertedBitLength % 8) != 0 {
		return nil, fmt.Errorf("inverted length not even number of bytes")
	}
	uninvertedLength := uninvertedBitLength / 8
	uninverted := make([]byte, uninvertedLength)
	bits := 0
	nbits := 0
	in := 0
	out := 0
	for in < invertedLength-1 {
		next := int(inverted[in]) ^ 0x7F
		in++
		if (next & 0x80) != 0 {
			return nil, fmt.Errorf("non-final inverted byte has high bit set at offset %d", in-1)
		}
		bits = (bits << 7) | next
		nbits += 7
		for nbits >= 8 {
			uninverted[out] = byte(bits >> (nbits - 8))
			out++
			nbits -= 8
		}
	}
	if out != uninvertedLength {
		return nil, fmt.Errorf("ordering uninvert: expected %d bytes, got %d", uninvertedLength, out)
	}
	return uninverted, nil
}

// ElementEndPos finds the end position (exclusive) of a single tuple element
// starting at data[pos]. Used by UnpackNullsLast to parse element boundaries.
func ElementEndPos(data []byte, pos int) (int, error) {
	if pos >= len(data) {
		return 0, fmt.Errorf("position %d beyond data length %d", pos, len(data))
	}
	typeCode := data[pos]
	switch {
	case typeCode == 0x00:
		// Standard null (shouldn't appear in nulls-last context, but handle it)
		return pos + 1, nil

	case typeCode == 0x01, typeCode == 0x02:
		// Byte string (0x01) or unicode string (0x02)
		// Scans for unescaped 0x00 terminator (0x00 0xFF is escaped null)
		i := pos + 1
		for i < len(data) {
			if data[i] == 0x00 {
				if i+1 < len(data) && data[i+1] == 0xFF {
					i += 2 // escaped null byte
					continue
				}
				return i + 1, nil // terminator found
			}
			i++
		}
		return 0, fmt.Errorf("unterminated string/bytes at offset %d", pos)

	case typeCode == 0x05:
		// Nested tuple: uses same 0x00-terminated, 0x00 0xFF-escaped format
		// but we need to handle nested nesting (depth tracking)
		i := pos + 1
		depth := 1
		for i < len(data) && depth > 0 {
			if data[i] == 0x05 {
				depth++
				i++
			} else if data[i] == 0x00 {
				if i+1 < len(data) && data[i+1] == 0xFF {
					i += 2 // escaped null within nested tuple
				} else {
					depth--
					i++ // terminator for this nesting level
				}
			} else {
				// Skip over a regular element within the nested tuple
				end, err := ElementEndPos(data, i)
				if err != nil {
					return 0, fmt.Errorf("in nested tuple: %w", err)
				}
				i = end
			}
		}
		if depth != 0 {
			return 0, fmt.Errorf("unterminated nested tuple at offset %d", pos)
		}
		return i, nil

	case typeCode == 0x14:
		// Integer zero
		return pos + 1, nil

	case typeCode >= 0x0C && typeCode <= 0x13:
		// Negative integers: type code encodes byte length as (0x14 - typeCode)
		n := int(0x14 - typeCode)
		end := pos + 1 + n
		if end > len(data) {
			return 0, fmt.Errorf("truncated negative int at offset %d: need %d bytes, have %d", pos, 1+n, len(data)-pos)
		}
		return end, nil

	case typeCode >= 0x15 && typeCode <= 0x1C:
		// Positive integers: type code encodes byte length as (typeCode - 0x14)
		n := int(typeCode - 0x14)
		end := pos + 1 + n
		if end > len(data) {
			return 0, fmt.Errorf("truncated positive int at offset %d: need %d bytes, have %d", pos, 1+n, len(data)-pos)
		}
		return end, nil

	case typeCode == 0x0B:
		// Negative arbitrary precision integer
		if pos+1 >= len(data) {
			return 0, fmt.Errorf("truncated negative bigint at offset %d", pos)
		}
		n := 255 - int(data[pos+1])
		end := pos + 2 + n
		if end > len(data) {
			return 0, fmt.Errorf("truncated negative bigint at offset %d: need %d bytes, have %d", pos, 2+n, len(data)-pos)
		}
		return end, nil

	case typeCode == 0x1D:
		// Positive arbitrary precision integer
		if pos+1 >= len(data) {
			return 0, fmt.Errorf("truncated positive bigint at offset %d", pos)
		}
		n := int(data[pos+1])
		end := pos + 2 + n
		if end > len(data) {
			return 0, fmt.Errorf("truncated positive bigint at offset %d: need %d bytes, have %d", pos, 2+n, len(data)-pos)
		}
		return end, nil

	case typeCode == 0x20:
		// Float32: type code + 4 bytes
		if pos+5 > len(data) {
			return 0, fmt.Errorf("truncated float32 at offset %d: need 5 bytes, have %d", pos, len(data)-pos)
		}
		return pos + 5, nil

	case typeCode == 0x21:
		// Float64: type code + 8 bytes
		if pos+9 > len(data) {
			return 0, fmt.Errorf("truncated float64 at offset %d: need 9 bytes, have %d", pos, len(data)-pos)
		}
		return pos + 9, nil

	case typeCode == 0x26:
		// Boolean false
		return pos + 1, nil

	case typeCode == 0x27:
		// Boolean true
		return pos + 1, nil

	case typeCode == 0x30:
		// UUID: type code + 16 bytes
		if pos+17 > len(data) {
			return 0, fmt.Errorf("truncated UUID at offset %d: need 17 bytes, have %d", pos, len(data)-pos)
		}
		return pos + 17, nil

	case typeCode == 0x33:
		// Versionstamp: type code + 12 bytes
		if pos+13 > len(data) {
			return 0, fmt.Errorf("truncated versionstamp at offset %d: need 13 bytes, have %d", pos, len(data)-pos)
		}
		return pos + 13, nil

	default:
		return 0, fmt.Errorf("unknown tuple type code 0x%02X at offset %d", typeCode, pos)
	}
}
