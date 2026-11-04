// Portions derived from FoundationDB Record Layer (
// BunchedMapMultiIterator.java, SubspaceSplitter.java,
// TextSubspaceSplitter.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// SubspaceSplitter determines which sub-map a given FDB key belongs to.
// Used by BunchedMapMultiIterator to partition scans across multiple BunchedMaps.
// Matches Java's com.apple.foundationdb.map.SubspaceSplitter.
type SubspaceSplitter interface {
	SubspaceOf(keyBytes []byte) (subspace.Subspace, error)
	SubspaceTag(ss subspace.Subspace) (tuple.Tuple, error)
}

// TextSubspaceSplitter splits TEXT index keys by grouping columns.
// Given an FDB key in the index subspace, extracts the first groupingColumns
// elements as the subspace prefix for that token's BunchedMap.
// Matches Java's TextSubspaceSplitter.
type TextSubspaceSplitter struct {
	indexSubspace   subspace.Subspace
	groupingColumns int // textPosition + 1 (grouping columns + token column)
}

// NewTextSubspaceSplitter creates a splitter for TEXT index scans.
func NewTextSubspaceSplitter(indexSubspace subspace.Subspace, groupingColumns int) *TextSubspaceSplitter {
	return &TextSubspaceSplitter{
		indexSubspace:   indexSubspace,
		groupingColumns: groupingColumns,
	}
}

// SubspaceOf extracts the subspace for a given FDB key by taking the first
// groupingColumns elements of the unpacked tuple.
func (s *TextSubspaceSplitter) SubspaceOf(keyBytes []byte) (subspace.Subspace, error) {
	t, err := tuplefast.SubspaceUnpack(keyBytes, len(s.indexSubspace.Bytes()))
	if err != nil {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: unable to unpack key: %v", err),
			Data:    keyBytes,
		}
	}
	if len(t) < s.groupingColumns {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: key has %d elements, need at least %d", len(t), s.groupingColumns),
			Data:    keyBytes,
		}
	}
	prefix := make(tuple.Tuple, s.groupingColumns)
	copy(prefix, t[:s.groupingColumns])
	return s.indexSubspace.Sub(tupleToTupleElements(prefix)...), nil
}

// SubspaceTag returns the grouping key tuple for a subspace.
func (s *TextSubspaceSplitter) SubspaceTag(ss subspace.Subspace) (tuple.Tuple, error) {
	t, err := tuplefast.SubspaceUnpack(ss.Bytes(), len(s.indexSubspace.Bytes()))
	if err != nil {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: unable to unpack subspace tag: %v", err),
			Data:    ss.Bytes(),
		}
	}
	return t, nil
}

// tupleToTupleElements converts a Tuple to []tuple.TupleElement for subspace.Sub().
func tupleToTupleElements(t tuple.Tuple) []tuple.TupleElement {
	result := make([]tuple.TupleElement, len(t))
	for i, v := range t {
		result[i] = v
	}
	return result
}

// BunchedMapScanEntry is a single entry from a multi-map scan.
// Contains the entry data plus the subspace it belongs to.
type BunchedMapScanEntry struct {
	Subspace    subspace.Subspace
	SubspaceTag tuple.Tuple
	Key         tuple.Tuple
	Value       []int
}

// KVCallback is called for each raw FDB key-value pair read during iteration.
// Used for byte scan limiting — the callback receives the raw key and value
// lengths before deserialization. Matches Java's Consumer<KeyValue> callback
// passed to BunchedMapMultiIterator.
type KVCallback func(keyLen, valueLen int)

// BunchedMapMultiIterator iterates over multiple BunchedMaps within a parent subspace.
// Entries are returned sorted first by subspace, then by key within subspace.
// Streams from FDB lazily — only one bunch is in memory at a time.
// Matches Java's BunchedMapMultiIterator.
type BunchedMapMultiIterator struct {
	serializer *TextIndexBunchedSerializer
	splitter   SubspaceSplitter
	parentKey  []byte // parent subspace bytes
	reverse    bool
	limit      int
	callback   KVCallback // per-KV callback for byte tracking

	// FDB streaming iterator
	rangeIter rangeIterator
	iterErr   error // sticky error from Advance/Get

	// tx and the range bounds let seekTo reopen the range read at a later key.
	tx            fdb.ReadTransaction
	logicalBounds bool
	beginBytes    []byte
	endBytes      []byte

	// seekTarget, while non-nil, makes advance skip the entries of
	// seekSubspaceKey that sort before it in scan order (see seekTo).
	seekTarget      tuple.Tuple
	seekSubspaceKey []byte

	// Continuation state
	continuation          []byte
	continuationSatisfied bool
	// continuationKey, while non-nil, skips the entries of the map
	// continuationKeySubspace at or before it in scan order.
	continuationKey         tuple.Tuple
	continuationKeySubspace []byte

	// Current subspace state
	currentSubspace       subspace.Subspace
	currentSubspaceKey    []byte
	currentSubspaceSuffix []byte
	currentSubspaceTag    tuple.Tuple

	// Current bunch state
	currentEntries []BunchedEntry[tuple.Tuple, []int]
	entryIndex     int

	// Iteration state
	lastKey   tuple.Tuple
	returned  int
	done      bool
	nextEntry *BunchedMapScanEntry
}

// NewBunchedMapMultiIterator creates a multi-map iterator.
// Streams from FDB lazily — only one bunch in memory at a time.
func NewBunchedMapMultiIterator(
	tx fdb.ReadTransaction,
	parentSubspace subspace.Subspace,
	splitter SubspaceSplitter,
	beginBytes, endBytes []byte,
	continuation []byte,
	limit int,
	reverse bool,
	serializer *TextIndexBunchedSerializer,
) *BunchedMapMultiIterator {
	return NewBunchedMapMultiIteratorWithCallback(
		tx, parentSubspace, splitter,
		beginBytes, endBytes, continuation,
		limit, nil, reverse, serializer,
	)
}

// NewBunchedMapMultiIteratorWithCallback creates a multi-map iterator with a
// per-KV callback for byte tracking. The callback fires once per raw FDB
// key-value pair read, before deserialization. Matches Java's scanMulti()
// with Consumer<KeyValue> callback parameter.
func NewBunchedMapMultiIteratorWithCallback(
	tx fdb.ReadTransaction,
	parentSubspace subspace.Subspace,
	splitter SubspaceSplitter,
	beginBytes, endBytes []byte,
	continuation []byte,
	limit int,
	callback KVCallback,
	reverse bool,
	serializer *TextIndexBunchedSerializer,
) *BunchedMapMultiIterator {
	it := &BunchedMapMultiIterator{
		serializer:            serializer,
		splitter:              splitter,
		parentKey:             parentSubspace.Bytes(),
		reverse:               reverse,
		limit:                 limit,
		callback:              callback,
		tx:                    tx,
		beginBytes:            beginBytes,
		endBytes:              endBytes,
		continuation:          continuation,
		continuationSatisfied: continuation == nil,
		entryIndex:            -1,
	}

	// A TEXT suffix bounds logical entries, not the physical bunch signpost.
	// Include the predecessor bunch and filter its decoded entries below.
	if text, ok := splitter.(*TextSubspaceSplitter); ok {
		if low, err := text.SubspaceOf(beginBytes); err == nil {
			it.logicalBounds = len(beginBytes) > len(low.Bytes())+1
		}
	}
	// A resumed range needs the same predecessor-inclusive lower bound.
	beginSelector := fdb.FirstGreaterOrEqual(fdb.Key(beginBytes))
	if it.logicalBounds {
		beginSelector = fdb.LastLessOrEqual(fdb.Key(beginBytes))
	}
	// Build the FDB range read based on continuation.
	var rangeResult fdb.RangeResult
	if continuation == nil && it.logicalBounds {
		rangeResult = tx.GetRange(fdb.SelectorRange{Begin: fdb.LastLessOrEqual(fdb.Key(beginBytes)), End: fdb.FirstGreaterOrEqual(fdb.Key(endBytes))},
			fdb.RangeOptions{Reverse: reverse})
	} else if continuation == nil {
		rangeResult = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(beginBytes), End: fdb.Key(endBytes)},
			fdb.RangeOptions{Reverse: reverse})
	} else {
		continuationEndpoint := append(append([]byte{}, it.parentKey...), continuation...)
		if reverse {
			if bytes.Compare(continuationEndpoint, endBytes) < 0 {
				rangeResult = tx.GetRange(fdb.SelectorRange{Begin: beginSelector, End: fdb.FirstGreaterOrEqual(fdb.Key(continuationEndpoint))},
					fdb.RangeOptions{Reverse: true})
			} else {
				rangeResult = tx.GetRange(fdb.SelectorRange{Begin: beginSelector, End: fdb.FirstGreaterOrEqual(fdb.Key(endBytes))},
					fdb.RangeOptions{Reverse: true})
			}
		} else {
			if bytes.Compare(continuationEndpoint, beginBytes) < 0 {
				rangeResult = tx.GetRange(fdb.SelectorRange{Begin: beginSelector, End: fdb.FirstGreaterOrEqual(fdb.Key(endBytes))},
					fdb.RangeOptions{Reverse: false})
			} else {
				// lastLessThan(continuationEndpoint) to firstGreaterOrEqual(endBytes)
				rangeResult = tx.GetRange(fdb.SelectorRange{
					Begin: fdb.LastLessThan(fdb.Key(continuationEndpoint)),
					End:   fdb.FirstGreaterOrEqual(fdb.Key(endBytes)),
				}, fdb.RangeOptions{Reverse: false})
			}
		}
	}

	it.rangeIter = rangeResult.Iterator()
	return it
}

// HasNext returns true if there are more entries to return.
func (it *BunchedMapMultiIterator) HasNext() bool {
	if it.done {
		return false
	}
	if it.nextEntry != nil {
		return true
	}
	it.advance()
	return it.nextEntry != nil
}

// Next returns the next entry and advances the iterator.
func (it *BunchedMapMultiIterator) Next() *BunchedMapScanEntry {
	if !it.HasNext() {
		return nil
	}
	entry := it.nextEntry
	it.lastKey = entry.Key
	it.nextEntry = nil
	it.returned++
	if it.limit > 0 && it.returned >= it.limit {
		it.done = true
	}
	return entry
}

// nextKV reads the next key-value from the FDB range iterator.
// Returns (kv, true) on success, (zero, false) when exhausted or on error
// (the error is recorded in iterErr and surfaced via Err()).
// Fires the KV callback if set.
func (it *BunchedMapMultiIterator) nextKV() (fdb.KeyValue, bool) {
	if !it.rangeIter.Advance() {
		// Advance()==false on exhaustion OR a transient FDB error (1007, timeout);
		// capture the stored Get() error so Err() surfaces it instead of looking like
		// clean end-of-data. This backs the live text-index scan (textCursor.Err()),
		// where swallowing it would silently truncate the result set.
		if _, err := it.rangeIter.Get(); err != nil {
			it.iterErr = err
		}
		return fdb.KeyValue{}, false
	}
	kv, err := it.rangeIter.Get()
	if err != nil {
		it.iterErr = err
		it.done = true
		return fdb.KeyValue{}, false
	}
	if it.callback != nil {
		it.callback(len(kv.Key), len(kv.Value))
	}
	return kv, true
}

// advance finds the next valid entry.
func (it *BunchedMapMultiIterator) advance() {
	for {
		// Try to get next entry from current bunch.
		if it.currentEntries != nil {
			idx := it.entryIndex
			if it.reverse {
				idx--
			} else {
				idx++
			}

			if idx >= 0 && idx < len(it.currentEntries) {
				it.entryIndex = idx
				e := it.currentEntries[idx]
				if !it.withinLogicalBounds(it.currentSubspaceKey, e.Key) {
					continue
				}
				it.nextEntry = &BunchedMapScanEntry{
					Subspace:    it.currentSubspace,
					SubspaceTag: it.currentSubspaceTag,
					Key:         e.Key,
					Value:       e.Value,
				}
				return
			}
			// Exhausted this bunch — clear so we don't re-enter it.
			it.currentEntries = nil
		}

		// Need next KV from FDB range iterator (streaming).
		kv, ok := it.nextKV()
		if !ok {
			it.done = true
			return
		}

		// Check if this key is in the parent subspace.
		if !bytes.HasPrefix(kv.Key, it.parentKey) {
			cmp := bytes.Compare(kv.Key, it.parentKey)
			if (!it.reverse && cmp > 0) || (it.reverse && cmp < 0) {
				// Past the parent subspace — done.
				it.done = true
				return
			}
			// Haven't reached the subspace yet, skip.
			continue
		}

		// Determine which sub-subspace this key belongs to.
		nextSubspace, err := it.splitter.SubspaceOf(kv.Key)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		nextSubspaceKey := nextSubspace.Bytes()
		nextSubspaceSuffix := make([]byte, len(nextSubspaceKey)-len(it.parentKey))
		copy(nextSubspaceSuffix, nextSubspaceKey[len(it.parentKey):])

		// Handle continuation.
		if !it.continuationSatisfied {
			if bytes.HasPrefix(it.continuation, nextSubspaceSuffix) {
				// This is the subspace we need to resume in.
				continuationKey, err := it.serializer.DeserializeKey(it.continuation, len(nextSubspaceSuffix), len(it.continuation)-len(nextSubspaceSuffix))
				if err != nil {
					it.iterErr = err
					it.done = true
					return
				}
				it.continuationSatisfied = true
				// The entries of this map up to the continuation key are skipped
				// below, bunch after bunch, until one is past it: Java hands the
				// key to the map's BunchedMapIterator (BunchedMapMultiIterator.java:
				// 136-139), whose filter stays on across bunches
				// (BunchedMapIterator.java:126-138). The key's own bunch can sort
				// after the bunch this read starts at.
				it.continuationKey = continuationKey
				it.continuationKeySubspace = nextSubspaceKey
			} else if bytes.Compare(nextSubspaceSuffix, it.continuation)*(boolToInt(!it.reverse)-boolToInt(it.reverse)) > 0 {
				// Past the continuation subspace, satisfied.
				it.continuationSatisfied = true
			} else {
				// Before the continuation subspace, skip.
				continue
			}
		}

		// Deserialize the bunch.
		boundaryKey, err := it.serializer.DeserializeKey(kv.Key, len(nextSubspaceKey), len(kv.Key)-len(nextSubspaceKey))
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		entries, err := it.serializer.DeserializeEntries(boundaryKey, kv.Value)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		if len(entries) == 0 {
			continue
		}

		startIdx := 0
		if it.reverse {
			startIdx = len(entries) - 1
		}
		if it.continuationKey != nil {
			if !bytes.Equal(nextSubspaceKey, it.continuationKeySubspace) {
				// The continuation's map ended without an entry past the key;
				// Java's next map iterator starts with no continuation key.
				it.continuationKey = nil
			} else {
				startIdx = -1
				for i := range entries {
					j := i
					if it.reverse {
						j = len(entries) - 1 - i
					}
					c := compareTuples(it.continuationKey, entries[j].Key)
					if (!it.reverse && c < 0) || (it.reverse && c > 0) {
						startIdx = j
						break
					}
				}
				if startIdx < 0 {
					continue // every entry of this bunch is at or before the key
				}
				it.continuationKey = nil
			}
		}
		if it.seekTarget != nil {
			var skip bool
			startIdx, skip, err = it.seekWithin(nextSubspaceKey, entries)
			if err != nil {
				it.iterErr = err
				it.done = true
				return
			}
			if skip {
				continue
			}
		}

		it.currentSubspace = nextSubspace
		it.currentSubspaceKey = nextSubspaceKey
		it.currentSubspaceSuffix = nextSubspaceSuffix
		tag, err := it.splitter.SubspaceTag(nextSubspace)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		it.currentSubspaceTag = tag
		it.currentEntries = entries
		it.entryIndex = startIdx
		e := entries[startIdx]
		if !it.withinLogicalBounds(it.currentSubspaceKey, e.Key) {
			continue
		}
		it.nextEntry = &BunchedMapScanEntry{
			Subspace:    it.currentSubspace,
			SubspaceTag: it.currentSubspaceTag,
			Key:         e.Key,
			Value:       e.Value,
		}
		return
	}
}

func (it *BunchedMapMultiIterator) withinLogicalBounds(prefix []byte, key tuple.Tuple) bool {
	if !it.logicalBounds {
		return true
	}
	packed := append(append([]byte(nil), prefix...), key.Pack()...)
	return bytes.Compare(packed, it.beginBytes) >= 0 && bytes.Compare(packed, it.endBytes) < 0
}

// GetContinuation returns a continuation token for resuming the scan.
// Returns nil if the scan is exhausted.
func (it *BunchedMapMultiIterator) GetContinuation() []byte {
	if it.lastKey == nil || it.currentSubspaceKey == nil {
		return nil
	}
	if it.done && (it.limit <= 0 || it.returned < it.limit) {
		// Exhausted the scan (not stopped by limit).
		return nil
	}
	// Continuation = subspaceSuffix + serializedKey
	return append(append([]byte{}, it.currentSubspaceSuffix...), it.serializer.SerializeKey(it.lastKey)...)
}

// seekTo repositions the iterator so that the next entry it returns is the
// first entry of the current map at or after target in scan order (at or
// below it for a reverse scan), as if every entry before it had been returned.
// GetContinuation afterwards is the one Java's streaming iteration holds
// after returning the last skipped entry, so a resume is unchanged.
//
// Precondition: the scanned range lies within one map, an entry has been
// returned (with no entry buffered by HasNext) and target sorts strictly after
// it in scan order. Every entry a seek reads or passes over is then in the
// current map, so GetContinuation keeps Java's form. Entries of the current
// bunch are searched in memory; otherwise the range read is reopened at the
// bunch that can contain target: a bunch is keyed by its first entry, so the
// target's bunch is the last one whose key is below the target key.
//
// Not part of Java's BunchedMapMultiIterator: it is the seek the leapfrog text
// intersection uses (see DIVERGENCES.md, "TEXT intersection seeks").
func (it *BunchedMapMultiIterator) seekTo(target tuple.Tuple) {
	if it.done || it.nextEntry != nil || it.lastKey == nil || it.currentSubspaceKey == nil {
		return
	}
	if it.currentEntries != nil {
		if it.reverse {
			for j := it.entryIndex - 1; j >= 0; j-- {
				if compareTuples(it.currentEntries[j].Key, target) <= 0 {
					it.entryIndex = j + 1
					it.lastKey = it.currentEntries[j+1].Key
					return
				}
			}
		} else {
			for j := it.entryIndex + 1; j < len(it.currentEntries); j++ {
				if compareTuples(it.currentEntries[j].Key, target) >= 0 {
					it.entryIndex = j - 1
					it.lastKey = it.currentEntries[j-1].Key
					return
				}
			}
		}
	}

	// The target lies beyond the in-memory bunch. Its bunch is in the current
	// map and no earlier than the current bunch, so both selectors resolve
	// inside the scanned range.
	endpoint := append(append([]byte{}, it.currentSubspaceKey...), it.serializer.SerializeKey(target)...)
	var rangeResult fdb.RangeResult
	if it.reverse {
		// The bunch after the target's one holds the entry that precedes the
		// target in a reverse scan, so the read includes it: end past the first
		// key greater than the endpoint.
		begin := fdb.FirstGreaterOrEqual(fdb.Key(it.beginBytes))
		if it.logicalBounds {
			begin = fdb.LastLessOrEqual(fdb.Key(it.beginBytes))
		}
		rangeResult = it.tx.GetRange(fdb.SelectorRange{
			Begin: begin,
			End:   fdb.KeySelector{Key: fdb.Key(endpoint), OrEqual: true, Offset: 2},
		}, fdb.RangeOptions{Reverse: true})
	} else {
		rangeResult = it.tx.GetRange(fdb.SelectorRange{
			Begin: fdb.LastLessThan(fdb.Key(endpoint)),
			End:   fdb.FirstGreaterOrEqual(fdb.Key(it.endBytes)),
		}, fdb.RangeOptions{Reverse: false})
	}
	it.rangeIter = rangeResult.Iterator()
	it.currentEntries = nil
	it.entryIndex = -1
	it.seekTarget = target
	it.seekSubspaceKey = it.currentSubspaceKey
}

// seekWithin applies a pending seek to a freshly read bunch of the map
// subspaceKey. It returns the index of the first entry at or past the target
// in scan order, or skip=true when the whole bunch sorts before the target.
// Entries passed over become the iterator's last key.
func (it *BunchedMapMultiIterator) seekWithin(subspaceKey []byte, entries []BunchedEntry[tuple.Tuple, []int]) (int, bool, error) {
	if !bytes.Equal(subspaceKey, it.seekSubspaceKey) {
		// The reopened read starts in the target's map and ends with the scanned
		// range, which lies within it (seekTo's precondition).
		return 0, false, &BunchedMapException{Message: "seek read a bunch outside the map it seeks in"}
	}
	if it.reverse {
		for j := len(entries) - 1; j >= 0; j-- {
			if compareTuples(entries[j].Key, it.seekTarget) <= 0 {
				if j < len(entries)-1 {
					it.lastKey = entries[j+1].Key
				}
				it.seekTarget = nil
				return j, false, nil
			}
		}
		it.lastKey = entries[0].Key
		return 0, true, nil
	}
	for j := range entries {
		if compareTuples(entries[j].Key, it.seekTarget) >= 0 {
			if j > 0 {
				it.lastKey = entries[j-1].Key
			}
			it.seekTarget = nil
			return j, false, nil
		}
	}
	it.lastKey = entries[len(entries)-1].Key
	return 0, true, nil
}

// Cancel stops the iterator.
func (it *BunchedMapMultiIterator) Cancel() {
	it.done = true
}

// Err returns the first error encountered during iteration, if any.
func (it *BunchedMapMultiIterator) Err() error {
	return it.iterErr
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
