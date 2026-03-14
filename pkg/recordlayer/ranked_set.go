package recordlayer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
	// NLevels is the number of skip-list levels (2-8, default 6).
	NLevels int
	// CountDuplicates tracks duplicate keys separately, increasing ranks below them.
	CountDuplicates bool
}

// RankedSetHashFunction computes a hash for level determination.
// Must return int32 to match Java's int semantics.
type RankedSetHashFunction func(key []byte) int32

const (
	rankedSetLevelFanPow   = 4
	rankedSetMaxLevels     = 32 / rankedSetLevelFanPow // 8
	rankedSetDefaultLevels = 6
)

var rankedSetLevelFanValues [rankedSetMaxLevels]int32

func init() {
	for i := range rankedSetMaxLevels {
		rankedSetLevelFanValues[i] = (1 << (i * rankedSetLevelFanPow)) - 1
	}
}

// DefaultRankedSetConfig is the default configuration matching Java's defaults.
var DefaultRankedSetConfig = RankedSetConfig{
	HashFunction: JDKArrayHash,
	NLevels:      rankedSetDefaultLevels,
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

// NewRankedSet creates a RankedSet backed by the given subspace.
func NewRankedSet(sub subspace.Subspace, config RankedSetConfig) *RankedSet {
	if config.NLevels <= 0 {
		config.NLevels = rankedSetDefaultLevels
	}
	if config.NLevels > rankedSetMaxLevels {
		config.NLevels = rankedSetMaxLevels
	}
	if config.HashFunction == nil {
		config.HashFunction = JDKArrayHash
	}
	return &RankedSet{subspace: sub, config: config}
}

// Init initializes the ranked set by creating sentinel entries at each level.
// Idempotent — skips levels that already have sentinels.
// Must be called before first use. Matches Java's RankedSet.init().
func (rs *RankedSet) Init(tx fdb.Transaction) error {
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

// Add inserts a key into the ranked set. Returns true if the set was modified.
// If CountDuplicates is false and key already exists, returns false.
// Matches Java's RankedSet.add().
func (rs *RankedSet) Add(tx fdb.Transaction, key []byte) (bool, error) {
	if len(key) == 0 {
		return false, &RankedSetEmptyKeyError{}
	}

	keyHash := rs.config.HashFunction(key)

	count, err := rs.countCheckedKey(tx, key)
	if err != nil {
		return false, err
	}
	duplicate := count != nil && *count > 0
	if duplicate && !rs.config.CountDuplicates {
		return false, nil
	}

	for level := 0; level < rs.config.NLevels; level++ {
		if level == 0 {
			k := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(0), key}))
			if duplicate {
				tx.Add(k, rsEncodeLong(1))
			} else {
				tx.Set(k, rsEncodeLong(1))
			}
		} else if duplicate || (keyHash&rankedSetLevelFanValues[level]) != 0 {
			// Key doesn't get a new entry at this level — just increment the count
			// of the entry at or before this key.
			prevKey, err := rs.getPreviousKey(tx, level, key, duplicate)
			if err != nil {
				return false, err
			}
			tx.Add(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), prevKey})), rsEncodeLong(1))
		} else {
			// Insert new entry at this level. Lower levels are already done (sequential in Go).
			if err := rs.addInsertLevelKey(tx, key, level); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// addInsertLevelKey inserts a new entry for key at the given level.
// Splits the count from the previous entry by recounting from the level below.
func (rs *RankedSet) addInsertLevelKey(tx fdb.Transaction, key []byte, level int) error {
	prevKey, err := rs.getPreviousKey(tx, level, key, false)
	if err != nil {
		return err
	}

	prevCountBytes, err := tx.Get(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), prevKey}))).Get()
	if err != nil {
		return err
	}
	prevCount := rsDecodeLong(prevCountBytes)

	newPrevCount, err := rs.countRange(tx, level-1, prevKey, key)
	if err != nil {
		return err
	}

	count := prevCount - newPrevCount + 1
	tx.Set(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), prevKey})), rsEncodeLong(newPrevCount))
	tx.Set(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), key})), rsEncodeLong(count))
	return nil
}

// Remove removes a key from the ranked set. Returns true if the key was present.
// Matches Java's RankedSet.remove().
func (rs *RankedSet) Remove(tx fdb.Transaction, key []byte) (bool, error) {
	if len(key) == 0 {
		return false, &RankedSetEmptyKeyError{}
	}

	count, err := rs.countCheckedKey(tx, key)
	if err != nil {
		return false, err
	}
	if count == nil || *count <= 0 {
		return false, nil
	}

	duplicate := *count > 1

	for level := 0; level < rs.config.NLevels; level++ {
		if duplicate {
			if level == 0 {
				// Direct write — we already have a read conflict from countCheckedKey.
				tx.Set(
					fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(0), key})),
					rsEncodeLong(*count-1),
				)
			} else {
				// Atomic subtract from the entry at or before this key.
				prevKey, err := rs.getPreviousKey(tx, level, key, true)
				if err != nil {
					return false, err
				}
				tx.Add(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), prevKey})), rsEncodeLong(-1))
			}
		} else {
			k := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), key}))
			if level == 0 {
				tx.Clear(k)
			} else {
				// Check if key has an entry at this level (hash may not have matched).
				existing, err := tx.Get(k).Get()
				if err != nil {
					return false, err
				}
				prevKey, err := rs.getPreviousKey(tx, level, key, false)
				if err != nil {
					return false, err
				}

				var countChange int64 = -1
				if existing != nil {
					// Give back this entry's extra count to the predecessor.
					countChange += rsDecodeLong(existing)
					tx.Clear(k)
				}
				tx.Add(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), prevKey})), rsEncodeLong(countChange))
			}
		}
	}
	return true, nil
}

// Rank returns the 0-indexed rank (position in sorted order) of key.
// If nullIfMissing is true and key is absent, returns nil.
// If nullIfMissing is false, returns the rank key would have if inserted.
// Matches Java's RankedSet.rank().
func (rs *RankedSet) Rank(tx fdb.ReadTransaction, key []byte, nullIfMissing bool) (*int64, error) {
	if len(key) == 0 {
		return nil, &RankedSetEmptyKeyError{}
	}

	if nullIfMissing {
		count, err := rs.countCheckedKey(tx, key)
		if err != nil {
			return nil, err
		}
		if count == nil || *count <= 0 {
			return nil, nil
		}
	}

	keyShouldBePresent := nullIfMissing
	rankKey := []byte{}
	var rank int64

	for level := rs.config.NLevels - 1; level >= 0; level-- {
		levelSub := rs.subspace.Sub(int64(level))

		// Scan [rankKey, key] inclusive at this level.
		// begin = firstGreaterOrEqual(pack(rankKey)), end = firstGreaterThan(pack(key))
		begin := fdb.Key(levelSub.Pack(tuple.Tuple{rankKey}))
		endPacked := []byte(levelSub.Pack(tuple.Tuple{key}))
		end := fdb.Key(append(append([]byte(nil), endPacked...), 0x00))

		kvs, err := tx.GetRange(
			fdb.KeyRange{Begin: begin, End: end},
			fdb.RangeOptions{},
		).GetSliceWithError()
		if err != nil {
			return nil, err
		}

		var lastCount int64
		for _, kv := range kvs {
			t, err := levelSub.Unpack(kv.Key)
			if err != nil {
				return nil, err
			}
			var ok bool
			rankKey, ok = t[0].([]byte)
			if !ok {
				return nil, fmt.Errorf("ranked set: expected []byte key at level %d, got %T", level, t[0])
			}
			lastCount = rsDecodeLong(kv.Value)
			rank += lastCount
		}

		// Undo the last entry's count (we went up to but not past the target).
		rank -= lastCount

		if bytes.Equal(rankKey, key) {
			break // Exact match — rank is final.
		}
		if !keyShouldBePresent && level == 0 && lastCount > 0 {
			rank++
		}
	}

	return &rank, nil
}

// GetNth returns the key at the given 0-indexed rank (select operation).
// Returns nil if rank is out of bounds.
// Matches Java's RankedSet.getNth().
func (rs *RankedSet) GetNth(tx fdb.ReadTransaction, rank int64) ([]byte, error) {
	if rank < 0 {
		return nil, nil
	}

	key := []byte{}

	for level := rs.config.NLevels - 1; level >= 0; level-- {
		levelSub := rs.subspace.Sub(int64(level))

		begin := fdb.Key(levelSub.Pack(tuple.Tuple{key}))
		end := rs.levelEnd(level)

		kvs, err := tx.GetRange(
			fdb.KeyRange{Begin: begin, End: end},
			fdb.RangeOptions{},
		).GetSliceWithError()
		if err != nil {
			return nil, err
		}

		drillDown := false
		for _, kv := range kvs {
			t, err := levelSub.Unpack(kv.Key)
			if err != nil {
				return nil, err
			}
			var ok bool
			key, ok = t[0].([]byte)
			if !ok {
				return nil, fmt.Errorf("ranked set: expected []byte key at level %d, got %T", level, t[0])
			}

			if rank == 0 && len(key) > 0 {
				return key, nil // Found the element.
			}

			count := rsDecodeLong(kv.Value)
			if count > rank {
				drillDown = true
				break // Drill down to finer level.
			}
			rank -= count
		}

		if !drillDown {
			return nil, nil // Rank out of bounds.
		}
	}

	// With CountDuplicates, a key's count at level 0 may exceed 1.
	// When count > rank at level 0, we drilled down but there's no lower level.
	// The key variable holds the answer. Matches Java's getNth() return path.
	if len(key) > 0 {
		return key, nil
	}
	return nil, nil
}

// Contains checks if key is present in the set.
func (rs *RankedSet) Contains(tx fdb.ReadTransaction, key []byte) (bool, error) {
	if len(key) == 0 {
		return false, &RankedSetEmptyKeyError{}
	}
	count, err := rs.countCheckedKey(tx, key)
	if err != nil {
		return false, err
	}
	return count != nil && *count > 0, nil
}

// Count returns the number of occurrences of key (0 if absent, 1 normally,
// or more if CountDuplicates is enabled).
func (rs *RankedSet) Count(tx fdb.ReadTransaction, key []byte) (int64, error) {
	if len(key) == 0 {
		return 0, &RankedSetEmptyKeyError{}
	}
	count, err := rs.countCheckedKey(tx, key)
	if err != nil {
		return 0, err
	}
	if count == nil {
		return 0, nil
	}
	return *count, nil
}

// Size returns the total number of elements in the set.
// Sums counts at the coarsest level. Matches Java's RankedSet.size().
func (rs *RankedSet) Size(tx fdb.ReadTransaction) (int64, error) {
	topLevel := rs.config.NLevels - 1
	levelSub := rs.subspace.Sub(int64(topLevel))
	beginKC, endKC := levelSub.FDBRangeKeys()

	kvs, err := tx.GetRange(
		fdb.KeyRange{Begin: beginKC.FDBKey(), End: endKC.FDBKey()},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return 0, err
	}

	var total int64
	for _, kv := range kvs {
		total += rsDecodeLong(kv.Value)
	}
	return total, nil
}

// Clear removes all entries and reinitializes the ranked set.
func (rs *RankedSet) Clear(tx fdb.Transaction) error {
	beginKC, endKC := rs.subspace.FDBRangeKeys()
	tx.ClearRange(fdb.KeyRange{Begin: beginKC.FDBKey(), End: endKC.FDBKey()})
	return rs.Init(tx)
}

// --- Internal helpers ---

// countCheckedKey reads the count for a key at level 0.
// Returns nil if key has no entry.
func (rs *RankedSet) countCheckedKey(tx fdb.ReadTransaction, key []byte) (*int64, error) {
	v, err := tx.Get(fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(0), key}))).Get()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	count := rsDecodeLong(v)
	return &count, nil
}

// getPreviousKey finds the entry at or before key at the given level.
// Uses snapshot reads with manual conflict ranges, matching Java's RankedSet.getPreviousKey().
//
// If orEqual is true, the key itself may be returned (used for duplicates
// where the key already exists at this level).
func (rs *RankedSet) getPreviousKey(tx fdb.Transaction, level int, key []byte, orEqual bool) ([]byte, error) {
	k := rs.subspace.Pack(tuple.Tuple{int64(level), key})
	begin := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(level), []byte{}}))

	var end fdb.Key
	if orEqual {
		// Include the key itself: ByteArrayUtil.join(k, ZERO_ARRAY)
		end = fdb.Key(append(append([]byte(nil), k...), 0x00))
	} else {
		end = fdb.Key(k)
	}

	kvs, err := tx.Snapshot().GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{Limit: 1, Reverse: true},
	).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	if len(kvs) == 0 {
		return nil, fmt.Errorf("ranked set: no key found on level %d", level)
	}

	prevk := kvs[0].Key

	if !orEqual || !bytes.Equal([]byte(prevk), k) {
		// Conflict if a new key is inserted between prevk and k.
		exclusiveBegin := fdb.Key(append(append([]byte(nil), prevk...), 0x00))
		if err := tx.AddReadConflictRange(fdb.KeyRange{Begin: exclusiveBegin, End: fdb.Key(k)}); err != nil {
			return nil, err
		}
	}

	// Conflict if the previous key is removed entirely.
	prevKeyTuple, err := rs.subspace.Unpack(prevk)
	if err != nil {
		return nil, err
	}
	prevKeyBytes, ok := prevKeyTuple[1].([]byte)
	if !ok {
		return nil, fmt.Errorf("ranked set: expected []byte at tuple position 1, got %T", prevKeyTuple[1])
	}
	level0Key := fdb.Key(rs.subspace.Pack(tuple.Tuple{int64(0), prevKeyBytes}))
	if err := tx.AddReadConflictKey(level0Key); err != nil {
		return nil, err
	}

	return prevKeyBytes, nil
}

// countRange sums all counts at the given level in the range [beginKey, endKey).
func (rs *RankedSet) countRange(tx fdb.ReadTransaction, level int, beginKey, endKey []byte) (int64, error) {
	levelSub := rs.subspace.Sub(int64(level))

	var begin fdb.Key
	if beginKey == nil {
		b, _ := levelSub.FDBRangeKeys()
		begin = b.FDBKey()
	} else {
		begin = levelSub.Pack(tuple.Tuple{beginKey})
	}

	var end fdb.Key
	if endKey == nil {
		_, e := levelSub.FDBRangeKeys()
		end = e.FDBKey()
	} else {
		end = levelSub.Pack(tuple.Tuple{endKey})
	}

	kvs, err := tx.GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return 0, err
	}

	var sum int64
	for _, kv := range kvs {
		sum += rsDecodeLong(kv.Value)
	}
	return sum, nil
}

// levelEnd returns the end key of the range for a given level's subspace.
func (rs *RankedSet) levelEnd(level int) fdb.Key {
	_, end := rs.subspace.Sub(int64(level)).FDBRangeKeys()
	return end.FDBKey()
}

// rsEncodeLong encodes an int64 as 8-byte little-endian.
// Matches Java's RankedSet.encodeLong().
func rsEncodeLong(count int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(count))
	return b
}

// rsDecodeLong decodes 8-byte little-endian to int64.
// Returns 0 for nil or short slices (defensive: missing key = zero count).
// Matches Java's RankedSet.decodeLong().
func rsDecodeLong(v []byte) int64 {
	if len(v) < 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(v))
}
