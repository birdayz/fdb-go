// Portions derived from FoundationDB Record Layer (RankedSet.java,
// RankedSetHashFunctions.java, RankIndexMaintainerFactory.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rlcollections

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RankedSetEmptyKeyError is returned when a RankedSet operation receives an empty key.
type RankedSetEmptyKeyError struct{}

func (e *RankedSetEmptyKeyError) Error() string { return "ranked set: empty key not allowed" }

// RankedSet is a persistent skip-list that supports efficient retrieval of elements by rank.
// Wire-compatible with Java's com.apple.foundationdb.async.RankedSet.
//
// Elements are byte-array keys. The FDB key format is:
//
//	[subspace][level][key] → count (little-endian int64)
//
// Level 0 has one entry per element. Coarser levels sample values by hash.
// The count at each entry is the number of level-0 elements between this key
// and the previous key at the same level.
type RankedSet struct {
	subspace subspace.Subspace
	config   RankedSetConfig
}

// RankedSetConfig configures a RankedSet.
// Matches Java's RankedSet.Config.
type RankedSetConfig struct {
	// HashFunction determines which levels a key appears on.
	// Default: JDKArrayHash (matches Java's Arrays.hashCode).
	HashFunction RankedSetHashFunction
	// HashFunctionName is HashFunction's name in RankedSetHashFunctions, the
	// identity Java's option check compares (RankIndexMaintainerFactory's
	// getHashFunction().equals), since a Go func value has none.
	HashFunctionName string
	// NLevels is the number of skip-list levels (2-8, default 6).
	NLevels int
	// CountDuplicates tracks duplicate keys separately, increasing ranks below them.
	CountDuplicates bool
}

// RankedSetHashFunction computes a hash for level determination.
// Must return int32 to match Java's int semantics. Only RANDOM can fail: its
// draw reads a randomness source, and a failed read fails the write rather
// than choosing levels.
type RankedSetHashFunction func(key []byte) (int32, error)

// PureHash is a hash of the key alone, which cannot fail.
func PureHash(f func(key []byte) int32) RankedSetHashFunction {
	return func(key []byte) (int32, error) { return f(key), nil }
}

const (
	rankedSetLevelFanPow   = 4
	RankedSetMaxLevels     = 32 / rankedSetLevelFanPow // 8
	RankedSetDefaultLevels = 6
)

var rankedSetLevelFanValues [RankedSetMaxLevels]int32

func init() {
	for i := range RankedSetMaxLevels {
		rankedSetLevelFanValues[i] = (1 << (i * rankedSetLevelFanPow)) - 1
	}
}

// DefaultRankedSetConfig is the default configuration matching Java's defaults.
var DefaultRankedSetConfig = RankedSetConfig{
	HashFunction:     PureHash(JDKArrayHash),
	HashFunctionName: RankedSetHashJDK,
	NLevels:          RankedSetDefaultLevels,
}

// The names Java's RankedSetHashFunctions knows for the rankHashFunction
// index option.
const (
	RankedSetHashJDK     = "JDK"
	RankedSetHashCRC     = "CRC"
	RankedSetHashRandom  = "RANDOM"
	RankedSetHashMurmur3 = "MURMUR3"
)

// RankedSetHashFunctions is Java's RankedSetHashFunctions extent: the option
// value is looked up exactly, and a name it does not hold is refused
// ("hash function not found", RankedSetHashFunctions.java:47-59).
var RankedSetHashFunctions = map[string]RankedSetHashFunction{
	RankedSetHashJDK:     PureHash(JDKArrayHash),
	RankedSetHashCRC:     PureHash(CRCHash),
	RankedSetHashRandom:  RandomHashFrom(nil),
	RankedSetHashMurmur3: PureHash(Murmur3Hash),
}

// JDKArrayHash matches Java's Arrays.hashCode(byte[]).
// Uses signed byte arithmetic for compatibility.
func JDKArrayHash(key []byte) int32 {
	result := int32(1)
	for _, b := range key {
		result = 31*result + int32(int8(b))
	}
	return result
}

// CRCHash uses CRC-32 (IEEE) for better distribution.
// Matches Java's RankedSet.CRC_HASH.
func CRCHash(key []byte) int32 {
	return int32(crc32.ChecksumIEEE(key))
}

// RandomHashFrom matches Java's RankedSet.RANDOM_HASH: a uniformly random int
// per call, whatever the key (ThreadLocalRandom.current().nextInt()). A key's
// levels are then a coin toss on insert; a delete draws nothing, in either
// engine (Java's remove never calls the hash, RankedSet.java:487-491, nor does
// Remove), so it removes the key from every level that holds it. The draw
// decides persisted ranked-set bytes, so it goes through the randomness reader
// seam: nil is production's crypto/rand, and a maintainer binds its store's
// env reader (WithRandomReader) so a seeded run replays. A failed read is
// returned: a zero hash would put the key on every level.
func RandomHashFrom(r io.Reader) RankedSetHashFunction {
	return func([]byte) (int32, error) {
		var b [4]byte
		var err error
		if r == nil {
			_, err = crand.Read(b[:])
		} else {
			_, err = r.Read(b[:])
		}
		if err != nil {
			return 0, fmt.Errorf("ranked set RANDOM hash: %w", err)
		}
		return int32(binary.LittleEndian.Uint32(b[:])), nil
	}
}

// WithRandomReader binds a RANDOM hash to r; other hashes are pure.
func (c RankedSetConfig) WithRandomReader(r io.Reader) RankedSetConfig {
	if c.HashFunctionName == RankedSetHashRandom {
		c.HashFunction = RandomHashFrom(r)
	}
	return c
}

// Murmur3Hash matches Java's MURMUR3 entry, Guava's
// Hashing.murmur3_32_fixed().hashBytes(key).asInt(): MurmurHash3 x86_32 with
// seed 0, blocks read little-endian. ("fixed" repairs Guava's hashString of
// supplementary characters; hashBytes is the same in both.)
func Murmur3Hash(key []byte) int32 {
	const c1, c2 = 0xcc9e2d51, 0x1b873593
	var h uint32
	n := len(key)
	i := 0
	for ; i+4 <= n; i += 4 {
		k := binary.LittleEndian.Uint32(key[i:])
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
		h = bits.RotateLeft32(h, 13)
		h = h*5 + 0xe6546b64
	}
	var k uint32
	for j := n - 1; j >= i; j-- {
		k = k<<8 | uint32(key[j])
	}
	if i < n {
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
	}
	h ^= uint32(n)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return int32(h)
}

// NewRankedSet creates a RankedSet backed by the given subspace.
func NewRankedSet(sub subspace.Subspace, config RankedSetConfig) *RankedSet {
	if config.NLevels <= 0 {
		config.NLevels = RankedSetDefaultLevels
	}
	if config.NLevels > RankedSetMaxLevels {
		config.NLevels = RankedSetMaxLevels
	}
	if config.HashFunction == nil {
		config.HashFunction, config.HashFunctionName = PureHash(JDKArrayHash), RankedSetHashJDK
	}
	return &RankedSet{subspace: sub, config: config}
}

// Init initializes the ranked set by creating sentinel entries at each level.
// Idempotent — skips levels that already have sentinels.
// Must be called before first use. Matches Java's RankedSet.init().
func (rs *RankedSet) Init(tx fdb.WritableTransaction) error {
	for level := 0; level < rs.config.NLevels; level++ {
		k := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), []byte{}}))
		v, err := tx.Get(k).Get()
		if err != nil {
			return err
		}
		if v == nil {
			tx.Set(k, rsEncodeLong(0))
		}
	}
	return nil
}

// InitNeeded checks whether Init needs to be called.
func (rs *RankedSet) InitNeeded(tx fdb.ReadTransaction) (bool, error) {
	k := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(0), []byte{}}))
	v, err := tx.Get(k).Get()
	if err != nil {
		return false, err
	}
	return v == nil, nil
}

// Clear removes all entries and reinitializes the ranked set.
func (rs *RankedSet) Clear(tx fdb.WritableTransaction) error {
	beginKC, endKC := rs.subspace.FDBRangeKeys()
	tx.ClearRange(fdb.KeyRange{Begin: beginKC.FDBKey(), End: endKC.FDBKey()})
	return rs.Init(tx)
}

// rsEncodeLong encodes an int64 as 8-byte little-endian.
// Matches Java's RankedSet.encodeLong().
func rsEncodeLong(count int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(count))
	return b
}

// rsDecodeLong decodes 8-byte little-endian to int64.
// Returns 0 for nil (missing FDB key = zero count).
// Returns an error for non-nil values shorter than 8 bytes (data corruption).
func rsDecodeLong(v []byte) (int64, error) {
	if v == nil {
		return 0, nil
	}
	if len(v) < 8 {
		return 0, fmt.Errorf("ranked set: corrupted count value: expected 8 bytes, got %d", len(v))
	}
	return int64(binary.LittleEndian.Uint64(v)), nil
}
