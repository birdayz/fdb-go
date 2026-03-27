package recordlayer

import (
	"encoding/binary"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// FDB tuple type codes (from tuple layer spec).
const (
	tcNil            = 0x00
	tcBytes          = 0x01
	tcString         = 0x02
	tcNested         = 0x05
	tcNegIntStart    = 0x0b // big negative ints
	tcIntZero        = 0x14
	tcPosIntEnd      = 0x1d // big positive ints
	tcFloat          = 0x20
	tcDouble         = 0x21
	tcFalse          = 0x26
	tcTrue           = 0x27
	tcUUID           = 0x28
	tcVersionstamp80 = 0x30
	tcVersionstamp96 = 0x33
)

// tupleSkip returns the byte size of the first tuple element in b.
// Panics on malformed data — callers should validate length first.
func tupleSkip(b []byte) int {
	switch {
	case b[0] == tcNil:
		return 1
	case b[0] == tcBytes || b[0] == tcString:
		// Null-terminated with \xff escaping
		i := 1
		for i < len(b) {
			if b[i] == 0x00 {
				if i+1 < len(b) && b[i+1] == 0xff {
					i += 2
					continue
				}
				return i + 1
			}
			i++
		}
		return len(b)
	case b[0] > tcNegIntStart && b[0] < tcPosIntEnd:
		// Integer: byte count = |typecode - 0x14|
		n := int(b[0]) - tcIntZero
		if n < 0 {
			n = -n
		}
		return 1 + n
	case b[0] == tcNegIntStart || b[0] == tcPosIntEnd:
		// Big integer: next byte is length (possibly inverted for negative)
		length := int(b[1])
		if b[0] == tcNegIntStart {
			length ^= 0xff
		}
		return 2 + length
	case b[0] == tcFloat:
		return 5
	case b[0] == tcDouble:
		return 9
	case b[0] == tcFalse || b[0] == tcTrue:
		return 1
	case b[0] == tcUUID:
		return 17
	case b[0] == tcVersionstamp80:
		return 13
	case b[0] == tcVersionstamp96:
		return 15
	case b[0] == tcNested:
		// Skip nested: scan for unescaped 0x00
		i := 1
		for i < len(b) {
			if b[i] == 0x00 {
				if i+1 < len(b) && b[i+1] == 0xff {
					i += 2
					continue
				}
				return i + 1
			}
			i++
		}
		return len(b)
	}
	return 1 // unknown — skip 1 byte
}

// tupleDecodeInt decodes an FDB-tuple-encoded integer at b[0:]. Zero allocation.
func tupleDecodeInt(b []byte) (int64, int) {
	if b[0] == tcIntZero {
		return 0, 1
	}
	n := int(b[0]) - tcIntZero
	neg := n < 0
	if neg {
		n = -n
	}
	// Read n bytes big-endian into int64
	var buf [8]byte
	copy(buf[8-n:], b[1:1+n])
	ret := int64(binary.BigEndian.Uint64(buf[:]))
	if neg {
		// Negative ints are stored offset by size limit
		ret -= sizeLimits[n]
	}
	return ret, 1 + n
}

// sizeLimits matches the FDB tuple layer's size limits for negative int encoding.
var sizeLimits = [9]int64{
	0,
	(1 << 8) - 1,
	(1 << 16) - 1,
	(1 << 24) - 1,
	(1 << 32) - 1,
	(1 << 40) - 1,
	(1 << 48) - 1,
	(1 << 56) - 1,
	(1 << 63) - 1, // int64 max
}

// splitKeySuffix splits a tuple-encoded key (after subspace prefix stripping) into
// the PK portion and the trailing int64 suffix. Zero allocation.
// Returns the suffix value and the byte offset where the suffix starts.
func splitKeySuffix(tupleBytes []byte) (suffix int64, pkEnd int, err error) {
	pos := 0
	lastStart := 0
	for pos < len(tupleBytes) {
		lastStart = pos
		size := tupleSkip(tupleBytes[pos:])
		if size <= 0 {
			return 0, 0, fmt.Errorf("invalid tuple element at offset %d", pos)
		}
		pos += size
	}
	// Last element is the suffix
	tc := tupleBytes[lastStart]
	if tc < tcNegIntStart+1 || tc >= tcPosIntEnd {
		if tc != tcIntZero && tc != tcNegIntStart && tc != tcPosIntEnd {
			return 0, 0, fmt.Errorf("suffix is not an integer (typecode 0x%02x)", tc)
		}
	}
	suffix, _ = tupleDecodeInt(tupleBytes[lastStart:])
	return suffix, lastStart, nil
}

// unpackTupleAt decodes the tuple bytes at the given range. Delegates to tuple.Unpack.
func unpackTupleAt(key []byte, start, end int) (tuple.Tuple, error) {
	return tuple.Unpack(key[start:end])
}
