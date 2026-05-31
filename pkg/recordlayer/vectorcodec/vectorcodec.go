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
