// Portions derived from Guava (com.google.common.hash.BloomFilter,
// BloomFilterStrategies, Murmur3_128HashFunction) as bundled with the Java
// Record Layer's ProbableIntersectionCursorState.java,
// Copyright (C) 2011 The Guava Authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/bits"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// guavaBloomFilter is the Guava BloomFilter<List<Object>> that Java's
// ProbableIntersectionCursorState keeps per child and serializes into
// ProbableIntersectionContinuation.CursorState.bloom_filter, so a continuation
// minted by either engine resumes in the other. Hashing, sizing and the
// serialized form follow Guava 33 (BloomFilter.create, writeTo, readFrom and
// BloomFilterStrategies.MURMUR128_MITZ_64), with keys fed through
// ProbableIntersectionCursorState.KeyFunnel.VERSION_0.
type guavaBloomFilter struct {
	data             []uint64
	numHashFunctions int
	strategy         byte // ordinal: 0 MURMUR128_MITZ_32, 1 MURMUR128_MITZ_64
}

const (
	bloomStrategyMitz32 = 0
	bloomStrategyMitz64 = 1
)

// newGuavaBloomFilter is BloomFilter.create(funnel, expectedInsertions, fpp)
// with optimalNumOfBits and optimalNumOfHashFunctions(fpp) (Guava 33).
func newGuavaBloomFilter(expectedInsertions int64, fpp float64) (*guavaBloomFilter, error) {
	if expectedInsertions < 0 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("Expected insertions (%d) must be >= 0", expectedInsertions)}
	}
	if !(fpp > 0.0) {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("False positive probability (%v) must be > 0.0", fpp)}
	}
	if !(fpp < 1.0) {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("False positive probability (%v) must be < 1.0", fpp)}
	}
	if expectedInsertions == 0 {
		expectedInsertions = 1
	}
	// Guava's LOG_TWO and SQUARED_LOG_TWO are double arithmetic; a Go constant
	// expression would be evaluated exactly and round differently.
	logTwo := math.Log(2)
	squaredLogTwo := logTwo * logTwo
	numBits := int64(float64(-expectedInsertions) * math.Log(fpp) / squaredLogTwo)
	numHashFunctions := int(math.Max(1, float64(javaRound(-math.Log(fpp)/logTwo))))
	// Guava stores this count as an unsigned byte in continuations.
	if numHashFunctions > 255 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("numHashFunctions (%d) must be <= 255 (expectedInsertions=%d, fpp=%v)", numHashFunctions, expectedInsertions, fpp)}
	}
	if numBits <= 0 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("Could not create BloomFilter of %d bits", numBits)}
	}
	words := (numBits + 63) / 64
	if words > math.MaxInt32 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("Could not create BloomFilter of %d bits", numBits)}
	}
	return &guavaBloomFilter{data: make([]uint64, words), numHashFunctions: numHashFunctions, strategy: bloomStrategyMitz64}, nil
}

// javaRound is Math.round(double): floor(x + 0.5) as a long.
func javaRound(x float64) int64 {
	if math.IsNaN(x) {
		return 0
	}
	return int64(math.Floor(x + 0.5))
}

// readGuavaBloomFilter is BloomFilter.readFrom: strategy ordinal byte,
// unsigned hash-function count byte, big-endian int word count, then the
// big-endian words.
func readGuavaBloomFilter(b []byte) (*guavaBloomFilter, error) {
	fail := func(msg string) error {
		return &RecordCoreError{Message: "unable to deserialize bloom filter", Cause: &RecordCoreArgumentError{Message: msg}}
	}
	if len(b) < 6 {
		return nil, fail("truncated header")
	}
	strategy := int8(b[0])
	numHashFunctions := int(b[1])
	dataLength := int32(binary.BigEndian.Uint32(b[2:6]))
	if strategy != bloomStrategyMitz32 && strategy != bloomStrategyMitz64 {
		return nil, fail(fmt.Sprintf("unknown strategy ordinal %d", strategy))
	}
	if numHashFunctions == 0 {
		return nil, fail(fmt.Sprintf("numHashFunctions (%d) must be > 0 (strategy=%d, dataLength=%d)", numHashFunctions, strategy, dataLength))
	}
	if dataLength <= 0 {
		return nil, fail(fmt.Sprintf("invalid data length %d", dataLength))
	}
	if len(b) < 6+int(dataLength)*8 {
		return nil, fail("truncated bit array")
	}
	data := make([]uint64, dataLength)
	for i := range data {
		data[i] = binary.BigEndian.Uint64(b[6+8*i:])
	}
	return &guavaBloomFilter{data: data, numHashFunctions: numHashFunctions, strategy: byte(strategy)}, nil
}

// writeTo is BloomFilter.writeTo.
func (f *guavaBloomFilter) writeTo() []byte {
	out := make([]byte, 6+8*len(f.data))
	out[0] = f.strategy
	out[1] = byte(f.numHashFunctions)
	binary.BigEndian.PutUint32(out[2:6], uint32(len(f.data)))
	for i, w := range f.data {
		binary.BigEndian.PutUint64(out[6+8*i:], w)
	}
	return out
}

func (f *guavaBloomFilter) bitSize() uint64 { return uint64(len(f.data)) * 64 }

func (f *guavaBloomFilter) setBit(index uint64) {
	f.data[index>>6] |= 1 << (index & 63)
}

func (f *guavaBloomFilter) getBit(index uint64) bool {
	return f.data[index>>6]&(1<<(index&63)) != 0
}

// indexes yields the bit indexes the strategy derives for key
// (BloomFilterStrategies.MURMUR128_MITZ_32 / MURMUR128_MITZ_64 put).
func (f *guavaBloomFilter) indexes(key tuple.Tuple, visit func(uint64) bool) error {
	sink, err := bloomKeyFunnel(key)
	if err != nil {
		return err
	}
	h1, h2 := murmur3x64128(sink)
	size := f.bitSize()
	if f.strategy == bloomStrategyMitz32 {
		hash1 := int32(h1)
		hash2 := int32(h1 >> 32)
		for i := int32(1); i <= int32(f.numHashFunctions); i++ {
			combined := hash1 + i*hash2
			if combined < 0 {
				combined = ^combined
			}
			if !visit(uint64(combined) % size) {
				return nil
			}
		}
		return nil
	}
	combined := h1
	for i := 0; i < f.numHashFunctions; i++ {
		if !visit((combined & math.MaxInt64) % size) {
			return nil
		}
		combined += h2
	}
	return nil
}

func (f *guavaBloomFilter) put(key tuple.Tuple) error {
	return f.indexes(key, func(i uint64) bool { f.setBit(i); return true })
}

func (f *guavaBloomFilter) mightContain(key tuple.Tuple) (bool, error) {
	all := true
	err := f.indexes(key, func(i uint64) bool {
		if !f.getBit(i) {
			all = false
		}
		return all
	})
	return all, err
}

// bloomKeyFunnel is ProbableIntersectionCursorState.KeyFunnel.VERSION_0
// (ProbableIntersectionCursorState.java:123-157) feeding a Guava hasher, whose
// primitives are little-endian. Integers decoded from a tuple are Java Longs;
// a value Java holds as some other class (BigInteger, UUID, Versionstamp, a
// nested tuple) is fed as Tuple.from(o).pack().
func bloomKeyFunnel(key tuple.Tuple) ([]byte, error) {
	var sink []byte
	for _, o := range key {
		switch v := o.(type) {
		case nil:
			sink = append(sink, 0x00)
		case []byte:
			sink = append(sink, v...)
		case string:
			sink = append(sink, v...)
		case float32:
			sink = binary.LittleEndian.AppendUint32(sink, math.Float32bits(v))
		case float64:
			sink = binary.LittleEndian.AppendUint64(sink, math.Float64bits(v))
		case int64:
			sink = binary.LittleEndian.AppendUint64(sink, uint64(v))
		case int:
			sink = binary.LittleEndian.AppendUint64(sink, uint64(int64(v)))
		case bool:
			if v {
				sink = append(sink, 1)
			} else {
				sink = append(sink, 0)
			}
		case uint64:
			if v <= math.MaxInt64 {
				sink = binary.LittleEndian.AppendUint64(sink, v)
			} else {
				sink = append(sink, tuple.Tuple{new(big.Int).SetUint64(v)}.Pack()...)
			}
		default:
			packed, err := packTupleElement(o)
			if err != nil {
				return nil, err
			}
			sink = append(sink, packed...)
		}
	}
	return sink, nil
}

func packTupleElement(o any) ([]byte, error) {
	t := tuple.Tuple{o}
	if err := checkPackable(t); err != nil {
		return nil, err
	}
	if incomplete, err := t.HasIncompleteVersionstamp(); err != nil || incomplete {
		return nil, &RecordCoreError{Message: fmt.Sprintf("bloom filter key: incomplete versionstamp %v", o), Cause: err}
	}
	return t.Pack(), nil
}

// checkPackable reports an element tuple.Pack cannot encode.
func checkPackable(t tuple.Tuple) error {
	for _, e := range t {
		switch v := e.(type) {
		case tuple.Tuple:
			if err := checkPackable(v); err != nil {
				return err
			}
		case nil, int, int64, uint, uint64, *big.Int, big.Int, []byte, fdb.KeyConvertible,
			string, float32, float64, bool, tuple.UUID, tuple.Versionstamp:
		default:
			return &RecordCoreArgumentError{Message: fmt.Sprintf("bloom filter key: unencodable tuple element %v (type %T)", e, e)}
		}
	}
	return nil
}

// murmur3x64128 is Guava's Hashing.murmur3_128() (seed 0) over data; h1 and h2
// are the hash code's first and second little-endian longs
// (BloomFilterStrategies.lowerEight / upperEight).
func murmur3x64128(data []byte) (uint64, uint64) {
	const (
		c1 = 0x87c37b91114253d5
		c2 = 0x4cf5ad432745937f
	)
	var h1, h2 uint64
	n := len(data)
	for len(data) >= 16 {
		k1 := binary.LittleEndian.Uint64(data)
		k2 := binary.LittleEndian.Uint64(data[8:])
		data = data[16:]
		k1 *= c1
		k1 = bits.RotateLeft64(k1, 31)
		k1 *= c2
		h1 ^= k1
		h1 = bits.RotateLeft64(h1, 27)
		h1 += h2
		h1 = h1*5 + 0x52dce729
		k2 *= c2
		k2 = bits.RotateLeft64(k2, 33)
		k2 *= c1
		h2 ^= k2
		h2 = bits.RotateLeft64(h2, 31)
		h2 += h1
		h2 = h2*5 + 0x38495ab5
	}
	var k1, k2 uint64
	for i := len(data) - 1; i >= 8; i-- {
		k2 ^= uint64(data[i]) << (8 * uint(i-8))
	}
	for i := min(len(data), 8) - 1; i >= 0; i-- {
		k1 ^= uint64(data[i]) << (8 * uint(i))
	}
	if len(data) > 8 {
		k2 *= c2
		k2 = bits.RotateLeft64(k2, 33)
		k2 *= c1
		h2 ^= k2
	}
	if len(data) > 0 {
		k1 *= c1
		k1 = bits.RotateLeft64(k1, 31)
		k1 *= c2
		h1 ^= k1
	}
	h1 ^= uint64(n)
	h2 ^= uint64(n)
	h1 += h2
	h2 += h1
	h1 = fmix64(h1)
	h2 = fmix64(h2)
	h1 += h2
	h2 += h1
	return h1, h2
}

func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}
