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
// empty, RaBitQ-quantized (which must go through the VectorQuantizer instead),
// or truncated so that Java's RealVector.fromBytes rejects it (javaUnderflows;
// Deserialize reports that as an error).
func Payload(data []byte) (typeOrdinal byte, payload []byte, stride int, ok bool) {
	if len(data) < 1 {
		return 0, nil, 0, false
	}
	stride = componentStride(data[0])
	if stride == 0 || javaUnderflows(data, stride) { // RaBitQ, unknown, or truncated
		return data[0], data[1:], 0, false
	}
	return data[0], data[1:], stride, true
}

// componentStride is the bytes per component of a float VectorType ordinal, 0
// for RaBitQ and unknown ordinals.
func componentStride(typeOrdinal byte) int {
	switch typeOrdinal {
	case typeHalf:
		return 2
	case typeSingle:
		return 4
	case typeDouble:
		return 8
	}
	return 0
}

// javaUnderflows reports whether Java's RealVector.fromBytes rejects data.
// Each subtype's fromBytes sizes the vector from the WHOLE array, type byte
// included — numDimensions = vectorBytes.length >> log2(stride)
// (HalfRealVector.java:284, FloatRealVector.java:323,
// DoubleRealVector.decodeDoubleBytes :303) — and then reads that many
// components after the type byte. Whenever the length is a multiple of the
// stride that is one component more than the payload holds, and the read
// throws BufferUnderflowException. Any other trailing partial component is
// dropped by the floor, as Go's len(payload)/stride drops it.
func javaUnderflows(data []byte, stride int) bool {
	return len(data)%stride == 0
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
	if stride := componentStride(typeOrdinal); stride != 0 && javaUnderflows(data, stride) {
		return nil, fmt.Errorf("vectorcodec: truncated vector payload (%d bytes for %d-byte components)", len(payload), stride)
	}

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

// SerializeAs encodes vec at the precision named by a VectorType ordinal, as a
// Java RealVector of that type stores itself; RaBitQ and unknown ordinals fall
// back to DOUBLE.
func SerializeAs(typeOrdinal byte, vec []float64) []byte {
	switch typeOrdinal {
	case typeHalf:
		return SerializeHalf(vec)
	case typeSingle:
		buf := make([]byte, 1+4*len(vec))
		buf[0] = typeSingle
		for i, v := range vec {
			binary.BigEndian.PutUint32(buf[1+i*4:], math.Float32bits(float32(v)))
		}
		return buf
	}
	return Serialize(vec)
}

// SerializeHalf encodes a float64 vector into the HALF on-disk format
// (byte 0 = VectorType.HALF, then 2 big-endian bytes per component). Values are
// quantized as Java HalfRealVector (Half.valueOf): normal mantissas truncate,
// subnormals round, and magnitudes beyond half range become ±Inf. The SPFresh index
// (RFC-094) uses this for centroid/sidecar/staging vector fields — a raw
// fixed-width layout with no tuple escaping.
func SerializeHalf(vec []float64) []byte {
	buf := make([]byte, 1+2*len(vec))
	buf[0] = typeHalf
	for i, v := range vec {
		binary.BigEndian.PutUint16(buf[1+i*2:], javaHalfBits(float32(v)))
	}
	return buf
}

// javaHalfBits is Half.quantizeFloat followed by Half.floatToHalfShortBits.
func javaHalfBits(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	if math.IsNaN(float64(f)) {
		return 0x7e00
	}
	if f > 65504 || f < -65504 {
		return sign | 0x7c00
	}
	exponent := (b >> 23) & 0xff
	significand := b & 0x007fffff
	if exponent > 112 {
		return sign | uint16(((exponent-112)<<10)&0x7c00|significand>>13)
	}
	if exponent > 101 {
		return sign | uint16((((0x007ff000+significand)>>(125-exponent))+1)>>1)
	}
	return sign
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
