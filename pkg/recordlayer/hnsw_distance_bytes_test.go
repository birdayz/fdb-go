package recordlayer

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// TestVectorDistanceFromBytes_MatchesDeserialize asserts the zero-alloc
// byte-direct distance is bit-for-bit identical to deserialize + vectorDistance
// across every metric and stored precision, and allocates nothing.
func TestVectorDistanceFromBytes_MatchesDeserialize(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	metrics := []VectorMetric{VectorMetricEuclidean, VectorMetricCosine, VectorMetricInnerProduct}

	for _, dims := range []int{1, 2, 7, 128, 1536} {
		query := make([]float64, dims)
		raw := make([]float64, dims)
		for i := range query {
			query[i] = rng.NormFloat64()
			raw[i] = rng.NormFloat64()
		}

		// DOUBLE encoding (what Serialize writes).
		double := vectorcodec.Serialize(raw)
		// SINGLE and HALF encodings, built by hand.
		single := make([]byte, 1+4*dims)
		single[0] = vectorcodec.TypeSingle
		for i, v := range raw {
			binary.BigEndian.PutUint32(single[1+i*4:], math.Float32bits(float32(v)))
		}
		half := make([]byte, 1+2*dims)
		half[0] = vectorcodec.TypeHalf
		for i, v := range raw {
			binary.BigEndian.PutUint16(half[1+i*2:], float32ToHalf(float32(v)))
		}

		for _, stored := range [][]byte{double, single, half} {
			vec, err := deserializeVector(stored)
			if err != nil {
				t.Fatalf("deserialize: %v", err)
			}
			for _, m := range metrics {
				want := vectorDistance(query, vec, m)
				got, ok := vectorDistanceFromBytes(query, stored, m)
				if !ok {
					t.Fatalf("dims=%d type=%d metric=%d: fast path declined", dims, stored[0], m)
				}
				if got != want {
					t.Errorf("dims=%d type=%d metric=%d: got %v want %v (delta %g)",
						dims, stored[0], m, got, want, got-want)
				}
			}
		}
	}

	// RaBitQ / unknown ordinal must decline the fast path.
	if _, ok := vectorDistanceFromBytes([]float64{1, 2}, []byte{3, 0xab, 0xcd}, VectorMetricEuclidean); ok {
		t.Error("RaBitQ ordinal must decline the byte-direct fast path")
	}
	if _, ok := vectorDistanceFromBytes(nil, nil, VectorMetricEuclidean); ok {
		t.Error("empty stored bytes must decline the fast path")
	}
}

func TestVectorDistanceFromBytes_ZeroAlloc(t *testing.T) {
	query := make([]float64, 1536)
	raw := make([]float64, 1536)
	for i := range raw {
		raw[i] = float64(i) * 0.01
		query[i] = float64(i) * 0.02
	}
	stored := vectorcodec.Serialize(raw)
	for _, m := range []VectorMetric{VectorMetricEuclidean, VectorMetricCosine, VectorMetricInnerProduct} {
		allocs := testing.AllocsPerRun(50, func() {
			_, _ = vectorDistanceFromBytes(query, stored, m)
		})
		if allocs != 0 {
			t.Errorf("metric=%d: vectorDistanceFromBytes allocated %v/op, want 0", m, allocs)
		}
	}
}

// float32ToHalf is the inverse of vectorcodec.HalfToFloat32, for building test
// HALF payloads. Round-to-nearest-even is not required here — the test compares
// the two decoders against the SAME stored bytes, so any encoding is fine.
func float32ToHalf(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16((b >> 16) & 0x8000)
	exp := int((b>>23)&0xff) - 127 + 15
	frac := b & 0x7fffff
	if exp <= 0 {
		return sign // flush subnormals/zero to signed zero
	}
	if exp >= 0x1f {
		return sign | 0x7c00 // inf
	}
	return sign | uint16(exp<<10) | uint16(frac>>13)
}
