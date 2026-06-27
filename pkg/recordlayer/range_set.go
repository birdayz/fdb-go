package recordlayer

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Wire-compatible Go implementation of Java's com.apple.foundationdb.async.RangeSet.
//
// Storage format: each FDB key-value pair represents a contiguous range [begin, end).
//   - Key:   subspace.Pack(tuple.Tuple{rangeBeginBytes})  (tuple-packed in subspace)
//   - Value: rangeEndBytes                                (raw bytes, NOT tuple-packed)
//
// Valid key space: [\x00, \xff). FIRST_KEY = {0x00}, FINAL_KEY = {0xff}.

var (
	// rangeSetFirstKey is the minimum valid range boundary.
	rangeSetFirstKey = []byte{0x00}
	// rangeSetFinalKey is the exclusive upper bound sentinel.
	rangeSetFinalKey = []byte{0xff}
)

// RangeSetEmptyKeyError is returned when a RangeSet operation receives an empty key.
type RangeSetEmptyKeyError struct{}

func (e *RangeSetEmptyKeyError) Error() string { return "rangeset: key must be non-empty" }

// RangeSetKeyTooLargeError is returned when a key is >= \xff.
type RangeSetKeyTooLargeError struct {
	Key []byte
}

func (e *RangeSetKeyTooLargeError) Error() string {
	return fmt.Sprintf("rangeset: key %x must be less than \\xff", e.Key)
}

// RangeSetInvertedRangeError is returned when begin > end in a range operation.
type RangeSetInvertedRangeError struct {
	Begin []byte
	End   []byte
}

func (e *RangeSetInvertedRangeError) Error() string {
	return fmt.Sprintf("rangeset: begin %x must be <= end %x", e.Begin, e.End)
}

// RangeSetRange represents a gap (missing range) as [Begin, End).
type RangeSetRange struct {
	Begin []byte
	End   []byte
}

// RangeSet tracks ranges of completed work in an FDB subspace.
// Wire-compatible with Java's com.apple.foundationdb.async.RangeSet.
type RangeSet struct {
	subspace subspace.Subspace
}

// NewRangeSet creates a RangeSet that stores data in the given subspace.
func NewRangeSet(ss subspace.Subspace) *RangeSet {
	return &RangeSet{subspace: ss}
}

func rangeSetCheckKey(key []byte) error {
	if len(key) == 0 {
		return &RangeSetEmptyKeyError{}
	}
	if bytes.Compare(key, rangeSetFinalKey) >= 0 {
		return &RangeSetKeyTooLargeError{Key: key}
	}
	return nil
}

// rangeSetKeyAfter returns the lexicographic successor by appending 0x00.
// Matches Java's RangeSet.keyAfter().
func rangeSetKeyAfter(key []byte) []byte {
	ret := make([]byte, len(key)+1)
	copy(ret, key)
	return ret
}

// Contains checks if key is in any range in the set.
// Adds a read conflict on the key for proper isolation.
func (rs *RangeSet) Contains(tr fdb.WritableTransaction, key []byte) (bool, error) {
	if err := rangeSetCheckKey(key); err != nil {
		return false, err
	}

	frobnicated := rs.subspace.Pack(tuple.Tuple{key})
	if err := tr.AddReadConflictKey(fdb.Key(frobnicated)); err != nil {
		return false, err
	}

	// Reverse scan: find last range entry that starts at or before our key.
	ssBegin, _ := rs.subspace.FDBRangeKeys()
	kvs, err := tr.Snapshot().GetRange(
		fdb.KeyRange{Begin: ssBegin, End: fdb.Key(rangeSetKeyAfter(frobnicated))},
		fdb.RangeOptions{Limit: 1, Reverse: true},
	).GetSliceWithError()
	if err != nil {
		return false, err
	}

	if len(kvs) == 0 {
		return false, nil
	}

	endRange := kvs[0].Value
	return bytes.Compare(key, endRange) < 0, nil
}

// InsertRange adds range [begin, end) to the set.
// If begin is nil, uses FIRST_KEY. If end is nil, uses FINAL_KEY.
// If requireEmpty is true, only inserts if the range is currently empty (atomic test-and-set).
// Returns true if the database was modified.
func (rs *RangeSet) InsertRange(tr fdb.WritableTransaction, begin, end []byte, requireEmpty bool) (bool, error) {
	beginNonNull := begin
	if beginNonNull == nil {
		beginNonNull = rangeSetFirstKey
	}
	endNonNull := end
	if endNonNull == nil {
		endNonNull = rangeSetFinalKey
	}

	// Java validates begin with checkKey but NOT end (end can be FINAL_KEY).
	if err := rangeSetCheckKey(beginNonNull); err != nil {
		return false, err
	}
	if bytes.Compare(beginNonNull, endNonNull) > 0 {
		return false, &RangeSetInvertedRangeError{Begin: beginNonNull, End: endNonNull}
	}

	// Empty range: no-op.
	if bytes.Equal(beginNonNull, endNonNull) {
		return false, nil
	}

	frobnicatedBegin := rs.subspace.Pack(tuple.Tuple{beginNonNull})
	frobnicatedEnd := rs.subspace.Pack(tuple.Tuple{endNonNull})

	// Read conflict on the range being inserted.
	if err := tr.AddReadConflictRange(fdb.KeyRange{
		Begin: fdb.Key(frobnicatedBegin),
		End:   fdb.Key(frobnicatedEnd),
	}); err != nil {
		return false, err
	}

	snap := tr.Snapshot()
	keyAfterBegin := rangeSetKeyAfter(frobnicatedBegin)

	// "before" scan: last range entry that starts at or before our begin.
	ssBegin, _ := rs.subspace.FDBRangeKeys()
	beforeKVs, err := snap.GetRange(
		fdb.KeyRange{Begin: ssBegin, End: fdb.Key(keyAfterBegin)},
		fdb.RangeOptions{Limit: 1, Reverse: true},
	).GetSliceWithError()
	if err != nil {
		return false, err
	}

	// "after" scan: entries starting strictly after our begin, up to our end.
	afterLimit := 0 // unlimited
	if requireEmpty {
		afterLimit = 1
	}
	afterKVs, err := snap.GetRange(
		fdb.KeyRange{Begin: fdb.Key(keyAfterBegin), End: fdb.Key(frobnicatedEnd)},
		fdb.RangeOptions{Limit: afterLimit},
	).GetSliceWithError()
	if err != nil {
		return false, err
	}

	lastSeen := frobnicatedBegin
	hasBefore := len(beforeKVs) > 0
	var beforeKV fdb.KeyValue

	if hasBefore {
		beforeKV = beforeKVs[0]
		beforeEnd := beforeKV.Value // raw range end
		if bytes.Compare(beginNonNull, beforeEnd) < 0 {
			// Before range covers our begin.
			if requireEmpty {
				return false, nil
			}
			lastSeen = rs.subspace.Pack(tuple.Tuple{beforeEnd})
		}
	}

	if requireEmpty {
		// After iterator must be empty for the range to be empty.
		if len(afterKVs) > 0 {
			return false, nil
		}

		// Consolidation: if before ends exactly where we start, extend it.
		if hasBefore && bytes.Equal(beforeKV.Value, beginNonNull) {
			if err := tr.AddReadConflictKey(fdb.Key(beforeKV.Key)); err != nil {
				return false, err
			}
			tr.Set(fdb.Key(beforeKV.Key), endNonNull)
		} else {
			tr.Set(fdb.Key(frobnicatedBegin), endNonNull)
		}

		if err := tr.AddWriteConflictRange(fdb.KeyRange{
			Begin: fdb.Key(frobnicatedBegin),
			End:   fdb.Key(frobnicatedEnd),
		}); err != nil {
			return false, err
		}
		return true, nil
	}

	// Gap-filling mode (requireEmpty = false).
	changed := false
	for _, kv := range afterKVs {
		if bytes.Compare(lastSeen, kv.Key) < 0 {
			// Gap: fill from lastSeen to this entry's start.
			unpackedKey, unpackErr := fastSubspaceUnpack(kv.Key, len(rs.subspace.Bytes()))
			if unpackErr != nil {
				return false, unpackErr
			}
			keyBytes, ok := unpackedKey[0].([]byte)
			if !ok {
				return false, fmt.Errorf("rangeset: unexpected tuple element type %T, expected []byte", unpackedKey[0])
			}
			tr.Set(fdb.Key(lastSeen), keyBytes)
			if err := tr.AddWriteConflictRange(fdb.KeyRange{
				Begin: fdb.Key(lastSeen),
				End:   fdb.Key(kv.Key),
			}); err != nil {
				return false, err
			}
			changed = true
		}
		lastSeen = rs.subspace.Pack(tuple.Tuple{kv.Value})
	}

	// Final gap: from lastSeen to end.
	if bytes.Compare(lastSeen, frobnicatedEnd) < 0 {
		tr.Set(fdb.Key(lastSeen), endNonNull)
		if err := tr.AddWriteConflictRange(fdb.KeyRange{
			Begin: fdb.Key(lastSeen),
			End:   fdb.Key(frobnicatedEnd),
		}); err != nil {
			return false, err
		}
		changed = true
	}

	return changed, nil
}

// MissingRanges returns the gaps (ranges not in the set) within [begin, end).
// If begin is nil, uses FIRST_KEY. If end is nil, uses FINAL_KEY.
// If limit <= 0, returns all gaps.
func (rs *RangeSet) MissingRanges(tr fdb.WritableTransaction, begin, end []byte, limit int) ([]RangeSetRange, error) {
	beginNonNull := begin
	if beginNonNull == nil {
		beginNonNull = rangeSetFirstKey
	}
	endNonNull := end
	if endNonNull == nil {
		endNonNull = rangeSetFinalKey
	}

	if err := rangeSetCheckKey(beginNonNull); err != nil {
		return nil, err
	}
	if bytes.Compare(beginNonNull, endNonNull) > 0 {
		return nil, &RangeSetInvertedRangeError{Begin: beginNonNull, End: endNonNull}
	}

	if bytes.Equal(beginNonNull, endNonNull) {
		return nil, nil
	}

	frobnicatedBegin := rs.subspace.Pack(tuple.Tuple{beginNonNull})
	frobnicatedEnd := rs.subspace.Pack(tuple.Tuple{endNonNull})

	// Read conflict on the search range (matches Java's addReadConflictRangeIfNotSnapshot).
	if err := tr.AddReadConflictRange(fdb.KeyRange{
		Begin: fdb.Key(frobnicatedBegin),
		End:   fdb.Key(frobnicatedEnd),
	}); err != nil {
		return nil, err
	}

	snap := tr.Snapshot()
	keyAfterBegin := rangeSetKeyAfter(frobnicatedBegin)

	// "before" scan: last entry before our begin.
	ssBegin, _ := rs.subspace.FDBRangeKeys()
	beforeKVs, err := snap.GetRange(
		fdb.KeyRange{Begin: ssBegin, End: fdb.Key(keyAfterBegin)},
		fdb.RangeOptions{Limit: 1, Reverse: true},
	).GetSliceWithError()
	if err != nil {
		return nil, err
	}

	// "after" scan: entries from begin to end.
	afterKVs, err := snap.GetRange(
		fdb.KeyRange{Begin: fdb.Key(keyAfterBegin), End: fdb.Key(frobnicatedEnd)},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return nil, err
	}

	currBegin := beginNonNull

	// Check before entry.
	if len(beforeKVs) > 0 {
		beforeEnd := beforeKVs[0].Value
		if bytes.Compare(beginNonNull, beforeEnd) < 0 {
			currBegin = beforeEnd
		}
	}

	var results []RangeSetRange
	for _, kv := range afterKVs {
		unpackedKey, unpackErr := fastSubspaceUnpack(kv.Key, len(rs.subspace.Bytes()))
		if unpackErr != nil {
			return nil, unpackErr
		}
		nextBegin, ok := unpackedKey[0].([]byte)
		if !ok {
			return nil, fmt.Errorf("rangeset: unexpected tuple element type %T, expected []byte", unpackedKey[0])
		}

		if bytes.Compare(currBegin, nextBegin) < 0 {
			results = append(results, RangeSetRange{Begin: currBegin, End: nextBegin})
			if limit > 0 && len(results) >= limit {
				return results, nil
			}
		}
		currBegin = kv.Value // Move past this range.
	}

	// Final gap.
	if bytes.Compare(currBegin, endNonNull) < 0 {
		if limit <= 0 || len(results) < limit {
			results = append(results, RangeSetRange{Begin: currBegin, End: endNonNull})
		}
	}

	return results, nil
}

// IsEmpty returns true if no ranges have been inserted into this set.
func (rs *RangeSet) IsEmpty(tr fdb.WritableTransaction) (bool, error) {
	ranges, err := rs.MissingRanges(tr, nil, nil, 1)
	if err != nil {
		return false, err
	}
	if len(ranges) == 1 {
		return bytes.Equal(ranges[0].Begin, rangeSetFirstKey) &&
			bytes.Equal(ranges[0].End, rangeSetFinalKey), nil
	}
	// No missing ranges → set covers everything → not empty.
	return false, nil
}

// Clear removes all ranges from the set.
func (rs *RangeSet) Clear(tr fdb.WritableTransaction) {
	begin, end := rs.subspace.FDBRangeKeys()
	tr.ClearRange(fdb.KeyRange{Begin: begin, End: end})
}
