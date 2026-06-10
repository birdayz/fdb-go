// Package vectorcodec is the on-disk byte codec for HNSW vector columns,
// wire-compatible with Java's RealVector.fromBytes / VectorType. It is a leaf
// package (stdlib only) so it can be shared by the record-layer maintainer and
// the Cascades values package without an import cycle — the latter needs it to
// decode a stored VECTOR column for a row-by-row distance expression.
//
// Format: byte 0 = VectorType ordinal, bytes 1.. = big-endian IEEE-754 payload.
//
//	0 = HALF   (16-bit, 2 bytes/component)
//	1 = SINGLE (32-bit, 4 bytes/component)
//	2 = DOUBLE (64-bit, 8 bytes/component)
//	3 = RABITQ (quantized — not decodable here; use the quantizer)
package vectorcodec

import (
	"encoding/binary"
	"fmt"
	"math"
)

// VectorType ordinals, matching Java's VectorType enum.
const (
	typeHalf   = 0
	typeSingle = 1
	typeDouble = 2
	typeRaBitQ = 3
)

// Serialize encodes a float64 vector into the on-disk DOUBLE byte format the
// HNSW vector index reads (Java RealVector.fromBytes, VectorType.DOUBLE).
func Serialize(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = typeDouble
	for i, v := range vec {
		binary.BigEndian.PutUint64(buf[1+i*8:], math.Float64bits(v))
	}
	return buf
}

// Deserialize decodes a stored vector's bytes into float64 components. The
// precision is self-describing (byte 0), so no external type info is needed.
// RaBitQ-quantized vectors are not decodable here (they require the quantizer)
// and return an error.
//
// Payload exposes a stored vector's raw IEEE-754 payload for zero-allocation,
// component-at-a-time reads (e.g. computing a distance without materializing a
// []float64). It returns the type ordinal, the payload slice (sans the leading
// type byte), the number of bytes per component, and ok=false when the data is
// empty or RaBitQ-quantized (which must go through the VectorQuantizer instead).
func Payload(data []byte) (typeOrdinal byte, payload []byte, stride int, ok bool) {
	if len(data) < 1 {
		return 0, nil, 0, false
	}
	switch data[0] {
	case typeHalf:
		return typeHalf, data[1:], 2, true
	case typeSingle:
		return typeSingle, data[1:], 4, true
	case typeDouble:
		return typeDouble, data[1:], 8, true
	default: // RaBitQ or unknown
		return data[0], data[1:], 0, false
	}
}

// Type ordinals re-exported for callers that read components directly.
const (
	TypeHalf   = typeHalf
	TypeSingle = typeSingle
	TypeDouble = typeDouble
)

// HalfToFloat32 converts an IEEE-754 half-precision (16-bit) value to float32.
// Exported for zero-alloc readers that decode HALF payloads component by
// component (see Payload).
func HalfToFloat32(h uint16) float32 { return halfToFloat32(h) }

func Deserialize(data []byte) ([]float64, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("vectorcodec: empty vector data")
	}
	typeOrdinal := data[0]
	payload := data[1:]

	switch typeOrdinal {
	case typeHalf:
		numFloats := len(payload) / 2
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			bits := binary.BigEndian.Uint16(payload[i*2:])
			vec[i] = float64(halfToFloat32(bits))
		}
		return vec, nil
	case typeSingle:
		numFloats := len(payload) / 4
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			vec[i] = float64(math.Float32frombits(binary.BigEndian.Uint32(payload[i*4:])))
		}
		return vec, nil
	case typeDouble:
		numFloats := len(payload) / 8
		vec := make([]float64, numFloats)
		for i := 0; i < numFloats; i++ {
			vec[i] = math.Float64frombits(binary.BigEndian.Uint64(payload[i*8:]))
		}
		return vec, nil
	case typeRaBitQ:
		return nil, fmt.Errorf("vectorcodec: RaBitQ vectors must be decoded via the VectorQuantizer interface")
	default:
		return nil, fmt.Errorf("vectorcodec: unsupported vector type ordinal %d", typeOrdinal)
	}
}

// SerializeHalf encodes a float64 vector into the HALF on-disk format
// (byte 0 = VectorType.HALF, then 2 big-endian bytes per component). Values are
// rounded to nearest-even half precision; magnitudes beyond half range become
// ±Inf, exactly as a Java HalfRealVector would store them. The SPFresh index
// (RFC-094) uses this for centroid/sidecar/staging vector fields — a raw
// fixed-width layout with no tuple escaping.
func SerializeHalf(vec []float64) []byte {
	buf := make([]byte, 1+2*len(vec))
	buf[0] = typeHalf
	for i, v := range vec {
		binary.BigEndian.PutUint16(buf[1+i*2:], float32ToHalf(float32(v)))
	}
	return buf
}

// float32ToHalf converts float32 to IEEE 754 half precision with
// round-to-nearest-even, the inverse of halfToFloat32.
func float32ToHalf(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int32(b>>23) & 0xff
	frac := b & 0x7fffff

	switch {
	case exp == 0xff: // Inf or NaN
		if frac == 0 {
			return sign | 0x7c00
		}
		nan := uint16(frac >> 13)
		if nan == 0 {
			nan = 1 // keep NaN a NaN after truncation
		}
		return sign | 0x7c00 | nan
	case exp > 142: // overflow (half exp > 30) -> Inf
		return sign | 0x7c00
	case exp >= 113: // normalized half
		he := uint32(exp - 112)
		mant := frac >> 13
		// round to nearest even on the 13 dropped bits
		round := frac & 0x1fff
		if round > 0x1000 || (round == 0x1000 && mant&1 == 1) {
			mant++
			if mant == 0x400 { // mantissa overflow -> bump exponent
				mant = 0
				he++
				if he >= 0x1f {
					return sign | 0x7c00
				}
			}
		}
		return sign | uint16(he<<10) | uint16(mant)
	case exp >= 102: // subnormal half (incl. the rounding band just below it)
		// target mantissa = round(value / 2^-24) = (frac|2^23) >> (126 - exp),
		// round-to-nearest-even on the dropped bits. A carry can reach 0x400,
		// which is exactly the min-normal half bit pattern — still correct.
		shift := uint32(126 - exp) // in [14, 24]
		full := frac | 0x800000
		mant := full >> shift
		dropped := full & ((1 << shift) - 1)
		half := uint32(1) << (shift - 1)
		if dropped > half || (dropped == half && mant&1 == 1) {
			mant++
		}
		return sign | uint16(mant)
	default: // underflow -> signed zero
		return sign
	}
}

// halfToFloat32 converts an IEEE 754 half-precision (16-bit) float to float32.
func halfToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h & 0x3ff)

	switch {
	case exp == 0: // subnormal or zero
		if frac == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal: normalize
		for frac&0x400 == 0 {
			frac <<= 1
			exp--
		}
		exp++
		frac &= 0x3ff
		return math.Float32frombits(sign | ((exp + 112) << 23) | (frac << 13))
	case exp == 0x1f: // Inf or NaN
		if frac == 0 {
			return math.Float32frombits(sign | 0x7f800000)
		}
		return math.Float32frombits(sign | 0x7f800000 | (frac << 13))
	default: // normalized
		return math.Float32frombits(sign | ((exp + 112) << 23) | (frac << 13))
	}
}
