package recordlayer

// Zero-allocation FDB tuple decoder.
//
// The standard FDB Go tuple.Unpack allocates bytes.NewBuffer + binary.Read
// per integer element (~3 heap objects per int). This decoder uses
// binary.BigEndian.Uint64 on stack arrays instead. Wire-format compatible
// with the canonical FDB tuple layer.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// FDB tuple type codes.
const (
	tcNil           = 0x00
	tcBytes         = 0x01
	tcString        = 0x02
	tcNested        = 0x05
	tcNegIntStart   = 0x0b
	tcIntZero       = 0x14
	tcPosIntEnd     = 0x1d
	tcFloat         = 0x20
	tcDouble        = 0x21
	tcFalse         = 0x26
	tcTrue          = 0x27
	tcUUID          = 0x30
	tcVersionstamp  = 0x33
	versionstampLen = 12
)

// sizeLimits for negative int encoding.
// sizeLimits matches the FDB tuple layer's size limits for negative int encoding.
// Entry [8] must be -1 (= int64 of uint64(1<<64 - 1)), NOT MaxInt64.
var sizeLimits = [9]int64{
	0,
	(1 << 8) - 1,
	(1 << 16) - 1,
	(1 << 24) - 1,
	(1 << 32) - 1,
	(1 << 40) - 1,
	(1 << 48) - 1,
	(1 << 56) - 1,
	-1, // int64 overflow of (1<<64)-1, matches FDB's uint64 sizeLimits[8]
}

var minInt64Big = big.NewInt(math.MinInt64)

// tupleSkip returns the byte size of the first tuple element, or -1 on truncated input.
func tupleSkip(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	switch {
	case b[0] == tcNil:
		return 1
	case b[0] == tcBytes || b[0] == tcString:
		if len(b) < 2 {
			return -1
		}
		idx := findTerminator(b[1:])
		if idx < 0 {
			return -1
		}
		return 1 + idx + 1
	case b[0] > tcNegIntStart && b[0] < tcPosIntEnd:
		n := int(b[0]) - tcIntZero
		if n < 0 {
			n = -n
		}
		if 1+n > len(b) {
			return -1
		}
		return 1 + n
	case b[0] == tcNegIntStart || b[0] == tcPosIntEnd:
		if len(b) < 2 {
			return -1
		}
		length := int(b[1])
		if b[0] == tcNegIntStart {
			length ^= 0xff
		}
		if 2+length > len(b) {
			return -1
		}
		return 2 + length
	case b[0] == tcFloat:
		if len(b) < 5 {
			return -1
		}
		return 5
	case b[0] == tcDouble:
		if len(b) < 9 {
			return -1
		}
		return 9
	case b[0] == tcFalse || b[0] == tcTrue:
		return 1
	case b[0] == tcUUID:
		if len(b) < 17 {
			return -1
		}
		return 17
	case b[0] == tcVersionstamp:
		if len(b) < 1+versionstampLen {
			return -1
		}
		return 1 + versionstampLen
	case b[0] == tcNested:
		// A nested tuple is 0x05, the encoding of each child element, then a
		// terminating 0x00. Element *values* escape interior 0x00 as 0x00 0xFF,
		// but element *terminators* (e.g. a bytes element's trailing 0x00) are
		// left bare — so a naive 0x00 scan stops at the first inner terminator.
		// The only correct skip is to parse element by element: a NIL child is
		// 0x00 0xFF, the bare 0x00 is the nested terminator, and every other
		// child is consumed by a recursive tupleSkip (which knows how bytes,
		// strings, ints, and deeper nested tuples are framed).
		i := 1
		for i < len(b) {
			if b[i] == 0x00 {
				if i+1 < len(b) && b[i+1] == 0xff {
					i += 2 // nil child element (escaped 0x00)
					continue
				}
				return i + 1 // end-of-nested marker
			}
			sz := tupleSkip(b[i:])
			if sz <= 0 {
				return -1
			}
			i += sz
		}
		return len(b)
	}
	return -1 // unknown type code
}

// nestedPKSpans splits the content of a neighbor-list tuple (the bytes between
// the 0x05 marker and its terminating 0x00) into one byte-span per element,
// WITHOUT decoding/boxing the elements. Children are stored verbatim, so each
// returned span is exactly the bytes Tuple{pk}.Pack() produces — which is both
// the fetch-key suffix (after the per-layer prefix) and a stable visited-set
// key. Spans are sub-slices of content.
func nestedPKSpans(content []byte) ([][]byte, error) {
	if len(content) == 0 {
		return nil, nil
	}
	spans := make([][]byte, 0, 16)
	i := 0
	for i < len(content) {
		n := tupleSkip(content[i:])
		if n < 0 {
			return nil, fmt.Errorf("hnsw: truncated neighbor PK span at offset %d (len=%d)", i, len(content))
		}
		spans = append(spans, content[i:i+n])
		i += n
	}
	return spans, nil
}

// findTerminator returns the offset of the unescaped 0x00 terminator in b.
// Returns -1 if no terminator is found (truncated input).
func findTerminator(b []byte) int {
	bp := b
	var length int
	for {
		idx := bytes.IndexByte(bp, 0x00)
		if idx < 0 {
			return -1 // no terminator found
		}
		length += idx
		if idx+1 == len(bp) || bp[idx+1] != 0xFF {
			break
		}
		length += 2
		bp = bp[idx+2:]
	}
	return length
}

// fastDecodeInt decodes an FDB-tuple-encoded integer. Zero allocation.
func fastDecodeInt(b []byte) (any, int, error) {
	if b[0] == tcIntZero {
		return int64(0), 1, nil
	}
	n := int(b[0]) - tcIntZero
	neg := n < 0
	if neg {
		n = -n
	}
	if 1+n > len(b) {
		return nil, 0, fmt.Errorf("truncated integer at offset 0 (need %d bytes, have %d)", 1+n, len(b))
	}
	var buf [8]byte
	copy(buf[8-n:], b[1:1+n])
	ret := int64(binary.BigEndian.Uint64(buf[:]))
	if neg {
		return ret - sizeLimits[n], 1 + n, nil
	}
	if ret > 0 {
		return ret, 1 + n, nil
	}
	// Positive value that overflows int64 → uint64
	return uint64(ret), 1 + n, nil
}

// fastDecodeBigInt decodes big integers (type codes 0x0b, 0x1d). Allocates big.Int.
func fastDecodeBigInt(b []byte) (any, int, error) {
	if len(b) < 2 {
		return nil, 0, fmt.Errorf("truncated big integer at offset 0")
	}
	val := new(big.Int)
	offset := 1
	var length int
	if b[0] == tcNegIntStart || b[0] == tcPosIntEnd {
		length = int(b[1])
		if b[0] == tcNegIntStart {
			length ^= 0xff
		}
		offset++
	} else {
		length = 8
	}
	if offset+length > len(b) {
		return nil, 0, fmt.Errorf("truncated big integer (need %d bytes, have %d)", offset+length, len(b))
	}
	val.SetBytes(b[offset : length+offset])
	if b[0] < tcIntZero {
		sub := new(big.Int).Lsh(big.NewInt(1), uint(length)*8)
		sub.Sub(sub, big.NewInt(1))
		val.Sub(val, sub)
	}
	if val.Cmp(minInt64Big) == 0 {
		return val.Int64(), length + offset, nil
	}
	return val, length + offset, nil
}

// fastDecodeFloat decodes a float32. Uses stack array instead of bytes.NewBuffer.
func fastDecodeFloat(b []byte) (float32, int) {
	var buf [4]byte
	copy(buf[:], b[1:5])
	adjustFloatBytes(buf[:], false)
	return math.Float32frombits(binary.BigEndian.Uint32(buf[:])), 5
}

// fastDecodeDouble decodes a float64. Uses stack array instead of bytes.NewBuffer.
func fastDecodeDouble(b []byte) (float64, int) {
	var buf [8]byte
	copy(buf[:], b[1:9])
	adjustFloatBytes(buf[:], false)
	return math.Float64frombits(binary.BigEndian.Uint64(buf[:])), 9
}

func adjustFloatBytes(b []byte, encode bool) {
	if (encode && b[0]&0x80 != 0x00) || (!encode && b[0]&0x80 == 0x00) {
		for i := range b {
			b[i] ^= 0xff
		}
	} else {
		b[0] ^= 0x80
	}
}

func fastDecodeBytes(b []byte) ([]byte, int, error) {
	if len(b) < 2 {
		return nil, 0, fmt.Errorf("truncated bytes/string at offset 0 (len=%d)", len(b))
	}
	idx := findTerminator(b[1:])
	if idx < 0 {
		return nil, 0, fmt.Errorf("unterminated bytes/string element")
	}
	return bytes.Replace(b[1:idx+1], []byte{0x00, 0xFF}, []byte{0x00}, -1), idx + 2, nil
}

func fastDecodeString(b []byte) (string, int, error) {
	bp, idx, err := fastDecodeBytes(b)
	if err != nil {
		return "", 0, err
	}
	return string(bp), idx, nil
}

func fastDecodeUUID(b []byte) (tuple.UUID, int) {
	var u tuple.UUID
	copy(u[:], b[1:17])
	return u, 17
}

func fastDecodeVersionstamp(b []byte) (tuple.Versionstamp, int) {
	var tv [10]byte
	copy(tv[:], b[1:11])
	uv := binary.BigEndian.Uint16(b[11:13])
	return tuple.Versionstamp{TransactionVersion: tv, UserVersion: uv}, versionstampLen + 1
}

// fastUnpack decodes a tuple from raw bytes. Drop-in replacement for tuple.Unpack
// with zero allocation on the integer decode path.
func fastUnpack(b []byte) (tuple.Tuple, error) {
	t, _, err := fastDecodeTuple(b, false)
	return t, err
}

// fastSubspaceUnpack strips the subspace prefix and decodes the remainder using
// fastUnpack. Drop-in replacement for subspace.Unpack() with zero-alloc integer
// decode. Returns error if key is shorter than prefix.
func fastSubspaceUnpack(key []byte, prefixLen int) (tuple.Tuple, error) {
	if len(key) < prefixLen {
		return nil, fmt.Errorf("key (%d bytes) shorter than subspace prefix (%d bytes)", len(key), prefixLen)
	}
	return fastUnpack(key[prefixLen:])
}

func fastDecodeTuple(b []byte, nested bool) (tuple.Tuple, int, error) {
	// Pre-allocate based on byte length. A typical tuple element is 5-10 bytes
	// (1-byte type code + 1-8 bytes of payload). Avoids slice grow operations.
	// ~21% of decode-path allocs, -12.3% total benchmark allocs on BenchmarkScanIndex.
	// For nested calls, b includes bytes past the nested tuple end; over-allocation
	// is bounded and harmless.
	t := make(tuple.Tuple, 0, max(len(b)/5, 4))
	i := 0
	for i < len(b) {
		var el any
		var off int

		switch {
		case b[i] == tcNil:
			if !nested {
				el = nil
				off = 1
			} else if i+1 < len(b) && b[i+1] == 0xff {
				el = nil
				off = 2
			} else {
				return t, i + 1, nil
			}
		case b[i] == tcBytes:
			var err error
			el, off, err = fastDecodeBytes(b[i:])
			if err != nil {
				return nil, i, err
			}
		case b[i] == tcString:
			var err error
			el, off, err = fastDecodeString(b[i:])
			if err != nil {
				return nil, i, err
			}
		case tcNegIntStart+1 < b[i] && b[i] < tcPosIntEnd:
			var err error
			el, off, err = fastDecodeInt(b[i:])
			if err != nil {
				return nil, i, err
			}
		case tcNegIntStart+1 == b[i] && i+1 < len(b) && (b[i+1]&0x80 != 0):
			var err error
			el, off, err = fastDecodeInt(b[i:])
			if err != nil {
				return nil, i, err
			}
		case tcNegIntStart <= b[i] && b[i] <= tcPosIntEnd:
			var err error
			el, off, err = fastDecodeBigInt(b[i:])
			if err != nil {
				return nil, i, err
			}
		case b[i] == tcFloat:
			if i+5 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes for float at %d", i)
			}
			el, off = fastDecodeFloat(b[i:])
		case b[i] == tcDouble:
			if i+9 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes for double at %d", i)
			}
			el, off = fastDecodeDouble(b[i:])
		case b[i] == tcTrue:
			el = true
			off = 1
		case b[i] == tcFalse:
			el = false
			off = 1
		case b[i] == tcUUID:
			if i+17 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes for UUID at %d", i)
			}
			el, off = fastDecodeUUID(b[i:])
		case b[i] == tcVersionstamp:
			if i+versionstampLen+1 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes for Versionstamp at %d", i)
			}
			el, off = fastDecodeVersionstamp(b[i:])
		case b[i] == tcNested:
			var err error
			el, off, err = fastDecodeTuple(b[i+1:], true)
			if err != nil {
				return nil, i, err
			}
			off++
		default:
			return nil, i, fmt.Errorf("unknown tuple typecode 0x%02x at %d", b[i], i)
		}
		t = append(t, el)
		i += off
	}
	return t, i, nil
}

// splitKeySuffix splits a tuple-encoded key into PK portion and trailing int64 suffix.
// Zero allocation.
func splitKeySuffix(tupleBytes []byte) (suffix int64, pkEnd int, err error) {
	// Defense-in-depth: an empty suffix has no tuple element, so the scan loop below
	// never runs and `tupleBytes[lastStart]` would index-panic. A record key is always
	// prefix + PK-tuple + suffix, so callers should never pass an empty slice — but a
	// stray/malformed key under the records subspace (foreign client, corruption) must
	// surface as an error here, never a panic on the scan path (don't-leak-panics).
	if len(tupleBytes) == 0 {
		return 0, 0, fmt.Errorf("empty key suffix: no tuple element to parse")
	}
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
	tc := tupleBytes[lastStart]
	if tc == tcIntZero || (tc > tcNegIntStart && tc < tcPosIntEnd) {
		val, _, decErr := fastDecodeInt(tupleBytes[lastStart:])
		if decErr != nil {
			return 0, 0, decErr
		}
		switch v := val.(type) {
		case int64:
			return v, lastStart, nil
		case uint64:
			return int64(v), lastStart, nil
		}
	}
	return 0, 0, fmt.Errorf("suffix is not an integer (typecode 0x%02x)", tc)
}
