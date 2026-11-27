// Portions derived from FoundationDB Record Layer (BunchedMap.java,
// BunchedMapIterator.java, StoreTimer.java, KeyRange.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rlcollections

import (
	"bytes"
	"fmt"

	"fdb.dev/internal/recordlayer/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Wire-compatible Go implementation of Java's com.apple.foundationdb.map.BunchedMap.
//
// A BunchedMap stores multiple logical map entries under "signpost" FDB keys
// to amortize the overhead of a shared subspace prefix. Each signpost key is
// subspacePrefix + serializer.SerializeKey(firstKeyInBunch), and the FDB value
// is the serialized bunch of all entries for that signpost's range.
//
// Specialized for TEXT index use: K = tuple.Tuple, V = []int.

const (
	// bunchedMapMaxValueSize is a conservative limit on FDB value size to avoid
	// hitting the actual 100KB limit. Matches Java's BunchedMap.MAX_VALUE_SIZE.
	bunchedMapMaxValueSize = 10_000

	// defaultBunchPoolCap is the initial slice capacity for pooled bunch items,
	// matching textIndexBunchSize + 1 (20 + 1) in pkg/recordlayer/text_index_maintainer.go.
	defaultBunchPoolCap = 21
)

// zeroArray is used to construct exclusive upper bounds for conflict ranges.
// Appending 0x00 to a key K gives the first key strictly greater than K.
// Matches Java's BunchedMap.ZERO_ARRAY.
var zeroArray = []byte{0x00}

// bunchedEntry is a convenience alias for BunchedEntry specialized for TEXT index use.
type bunchedEntry = BunchedEntry[tuple.Tuple, []int]

// BunchedMapTimer receives instrumentation hooks from BunchedMap operations.
// oldValLen is -1 when no previous value existed.
type BunchedMapTimer interface {
	OnWrite(keyLen, valLen, oldValLen int)
	OnDelete(keyLen, oldValLen int)
	OnRangeRead(kvs []fdb.KeyValue)
}

// RangeIterator is the minimal FDB range-scan iterator BunchedMap iterators depend on.
type RangeIterator interface {
	Advance() bool
	Get() (fdb.KeyValue, error)
}

// BunchedMap is a FoundationDB-backed map that bunches close keys together.
// Wire-compatible with Java's com.apple.foundationdb.map.BunchedMap.
//
// Currently specialized for TEXT index use (K = tuple.Tuple, V = []int).
//
// When a BunchedMapTimer is set, BunchedMap instruments all writes, deletes, and
// range reads matching Java's InstrumentedBunchedMap.
type BunchedMap struct {
	serializer *TextIndexBunchedSerializer
	bunchSize  int
	timer      BunchedMapTimer
}

// NewBunchedMap creates a new BunchedMap with the given bunch size.
func NewBunchedMap(bunchSize int) *BunchedMap {
	return &BunchedMap{
		serializer: TextIndexBunchedSerializerInstance(),
		bunchSize:  bunchSize,
	}
}

// NewInstrumentedBunchedMap creates a BunchedMap with instrumentation enabled.
// Matches Java's InstrumentedBunchedMap which wraps BunchedMap with timer hooks.
func NewInstrumentedBunchedMap(bunchSize int, timer BunchedMapTimer) *BunchedMap {
	return &BunchedMap{
		serializer: TextIndexBunchedSerializerInstance(),
		bunchSize:  bunchSize,
		timer:      timer,
	}
}

// instrumentWrite records a write operation in the timer.
// Matches Java's InstrumentedBunchedMap.instrumentWrite().
func (m *BunchedMap) instrumentWrite(key, value, oldValue []byte) {
	if m.timer == nil {
		return
	}
	oldValLen := -1
	if oldValue != nil {
		oldValLen = len(oldValue)
	}
	m.timer.OnWrite(len(key), len(value), oldValLen)
}

// instrumentDelete records a delete operation in the timer.
// Matches Java's InstrumentedBunchedMap.instrumentDelete().
func (m *BunchedMap) instrumentDelete(key, oldValue []byte) {
	if m.timer == nil {
		return
	}
	oldValLen := -1
	if oldValue != nil {
		oldValLen = len(oldValue)
	}
	m.timer.OnDelete(len(key), oldValLen)
}

// instrumentRangeRead records a range read result in the timer.
// Matches Java's InstrumentedBunchedMap.instrumentRangeRead().
func (m *BunchedMap) instrumentRangeRead(kvs []fdb.KeyValue) {
	if m.timer == nil {
		return
	}
	m.timer.OnRangeRead(kvs)
}

// joinBytes concatenates multiple byte slices. Equivalent to Java's ByteArrayUtil.join.
func joinBytes(slices ...[]byte) []byte {
	n := 0
	for _, s := range slices {
		n += len(s)
	}
	out := make([]byte, 0, n)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}

// Get retrieves the value associated with a key from the map.
// Returns (value, true, nil) if found, (nil, false, nil) if not found.
// Always adds a read conflict key on the map key for serializability,
// matching Java's BunchedMap.get() which takes TransactionContext.
func (m *BunchedMap) Get(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple) ([]int, bool, error) {
	subspaceKey := ss.Bytes()
	kv, found, err := m.entryForKey(tx, subspaceKey, key)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	mapKey, err := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
	if err != nil {
		return nil, false, fmt.Errorf("bunched map get: deserialize key: %w", err)
	}
	entries, err := m.serializer.DeserializeEntries(mapKey, kv.Value)
	if err != nil {
		return nil, false, fmt.Errorf("bunched map get: deserialize entries: %w", err)
	}
	for _, entry := range entries {
		if tupleEqual(entry.Key, key) {
			return entry.Value, true, nil
		}
	}
	return nil, false, nil
}

// VerifyIntegrity checks that all signpost keys and entries within bunches
// are in sorted order. Returns nil if the map is consistent.
// Matches Java's BunchedMap.verifyIntegrity().
func (m *BunchedMap) VerifyIntegrity(tx fdb.ReadTransaction, ss subspace.Subspace) error {
	subspaceKey := ss.Bytes()
	rr := tx.GetRange(ss, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return err
	}
	m.instrumentRangeRead(kvs)

	var lastKey []byte // packed key for comparison
	for _, kv := range kvs {
		boundaryKey, err := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
		if err != nil {
			return fmt.Errorf("bunched map verify: deserialize key: %w", err)
		}
		boundaryKeyPacked := boundaryKey.Pack()
		if lastKey != nil && bytes.Compare(boundaryKeyPacked, lastKey) < 0 {
			return &BunchedMapException{Message: "boundary key out of order"}
		}
		lastKey = boundaryKeyPacked

		keys, err := m.serializer.DeserializeKeys(boundaryKey, kv.Value)
		if err != nil {
			return fmt.Errorf("bunched map verify: deserialize keys: %w", err)
		}
		for _, k := range keys {
			kPacked := k.Pack()
			if bytes.Compare(kPacked, lastKey) < 0 {
				return &BunchedMapException{Message: "keys within bunch out of order"}
			}
			lastKey = kPacked
		}
	}
	return nil
}

// pendingSnapshotRead issues GetRange immediately and completes on the caller’s goroutine.
// Like Java’s CompletableFuture pipeline, an in-flight read needs no goroutine of its own.
type pendingSnapshotRead struct {
	result fdb.RangeResult
}

func startSnapshotRead(tx fdb.WritableTransaction, r fdb.SelectorRange) pendingSnapshotRead {
	// Java reads with ROW_LIMIT_UNLIMITED and StreamingMode.WANT_ALL.
	return pendingSnapshotRead{result: tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})}
}

func (p pendingSnapshotRead) wait() ([]fdb.KeyValue, error) {
	return p.result.GetSliceWithError()
}

// entryForKey finds the signpost (FDB key-value pair) that should contain
// the given map key. Uses snapshot read + explicit read conflict key.
// Matches Java's BunchedMap.entryForKey().
//
// Always adds a read conflict key on the map key, matching Java's
// "Grand Theory of Conflict Ranges" (BunchedMap.java lines 234-270):
// "When reading a map key, add a read conflict key to the corresponding
// key in the DB regardless of DB keys actually read."
func (m *BunchedMap) entryForKey(tx fdb.WritableTransaction, subspaceKey []byte, key tuple.Tuple) (fdb.KeyValue, bool, error) {
	p, err := m.startEntryForKey(tx, subspaceKey, key)
	if err != nil {
		return fdb.KeyValue{}, false, err
	}
	return p.finish()
}

// pendingEntryForKey is entryForKey with its read in flight (BunchedMap.java:
// 195-230 returns the read's future).
type pendingEntryForKey struct {
	m           *BunchedMap
	subspaceKey []byte
	keyBytes    []byte
	read        pendingSnapshotRead
}

// startEntryForKey adds entryForKey's read conflict key and issues its read.
func (m *BunchedMap) startEntryForKey(tx fdb.WritableTransaction, subspaceKey []byte, key tuple.Tuple) (*pendingEntryForKey, error) {
	keyBytes := joinBytes(subspaceKey, m.serializer.SerializeKey(key))

	// Add a read conflict key for the map key being accessed.
	// This is unconditional, matching Java's entryForKey() which always
	// calls tr.addReadConflictKey(keyBytes).
	if err := tx.AddReadConflictKeyBytes(keyBytes); err != nil {
		return nil, err
	}

	// Snapshot range read: lastLessOrEqual(keyBytes) to firstGreaterThan(keyBytes).
	read := startSnapshotRead(tx, fdb.SelectorRange{
		Begin: fdb.LastLessOrEqual(fdb.Key(keyBytes)),
		End:   fdb.FirstGreaterThan(fdb.Key(keyBytes)),
	})
	return &pendingEntryForKey{m: m, subspaceKey: subspaceKey, keyBytes: keyBytes, read: read}, nil
}

// finish waits for the read and picks the signpost that holds the key.
func (p *pendingEntryForKey) finish() (fdb.KeyValue, bool, error) {
	m, subspaceKey, keyBytes := p.m, p.subspaceKey, p.keyBytes
	keyValues, err := p.read.wait()
	if err != nil {
		return fdb.KeyValue{}, false, err
	}
	m.instrumentRangeRead(keyValues)

	if len(keyValues) == 0 {
		return fdb.KeyValue{}, false, nil
	}

	// The last result should be the greatest key <= keyBytes.
	kv := keyValues[len(keyValues)-1]
	if bytes.Compare(kv.Key, keyBytes) > 0 {
		return fdb.KeyValue{}, false, &BunchedMapException{
			Message: "signpost key found for key is greater than original key",
		}
	}
	if bytes.HasPrefix(kv.Key, subspaceKey) {
		return kv, true, nil
	}
	// Candidate key not in our subspace — key is before all entries in the map.
	return fdb.KeyValue{}, false, nil
}

// BunchedMapIterator iterates over a single BunchedMap's entries.
// Matches Java's BunchedMapIterator.
type BunchedMapIterator struct {
	serializer   *TextIndexBunchedSerializer
	subspaceKey  []byte
	rangeIter    RangeIterator
	reverse      bool
	limit        int
	continuation []byte

	currentEntries []bunchedEntry
	entryIndex     int
	lastKey        tuple.Tuple
	returned       int
	done           bool
	nextEntry      *bunchedEntry
	iterErr        error // sticky error from deserialization
}

// Scan iterates over all entries in a single BunchedMap within the given subspace.
// Supports continuation, limit, and reverse scan.
// Matches Java's BunchedMap.scan().
func (m *BunchedMap) Scan(tx fdb.ReadTransaction, ss subspace.Subspace, continuation []byte, limit int, reverse bool) *BunchedMapIterator {
	subspaceKey := ss.Bytes()

	var rangeResult fdb.RangeResult
	if continuation == nil {
		rangeResult = tx.GetRange(ss, fdb.RangeOptions{Reverse: reverse})
	} else {
		contKey := joinBytes(subspaceKey, continuation)
		if reverse {
			rangeResult = tx.GetRange(fdb.KeyRange{
				Begin: fdb.Key(subspaceKey),
				End:   fdb.Key(contKey),
			}, fdb.RangeOptions{Reverse: true})
		} else {
			rangeResult = tx.GetRange(fdb.SelectorRange{
				Begin: fdb.LastLessThan(fdb.Key(contKey)),
				End:   fdb.FirstGreaterOrEqual(fdb.Key(append(subspaceKey, 0xff))),
			}, fdb.RangeOptions{Reverse: false})
		}
	}

	return &BunchedMapIterator{
		serializer:   m.serializer,
		subspaceKey:  subspaceKey,
		rangeIter:    rangeResult.Iterator(),
		reverse:      reverse,
		limit:        limit,
		continuation: continuation,
		entryIndex:   -1,
	}
}

// HasNext returns true if there are more entries.
func (it *BunchedMapIterator) HasNext() bool {
	if it.done {
		return false
	}
	if it.nextEntry != nil {
		return true
	}
	it.advance()
	return it.nextEntry != nil
}

// Next returns the next entry.
func (it *BunchedMapIterator) Next() *bunchedEntry {
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

// skipToContinuation skips entries up to and including the continuation key
// and returns the index of the first entry strictly past it in scan order,
// or -1 if no entry in entries is past it.
func (it *BunchedMapIterator) skipToContinuation(entries []bunchedEntry) (int, error) {
	contKey, err := it.serializer.DeserializeKey(it.continuation, 0, len(it.continuation))
	if err != nil {
		return -1, err
	}
	if it.reverse {
		for i := len(entries) - 1; i >= 0; i-- {
			if compareTuples(contKey, entries[i].Key) > 0 {
				return i, nil
			}
		}
		return -1, nil
	}
	for i := 0; i < len(entries); i++ {
		if compareTuples(contKey, entries[i].Key) < 0 {
			return i, nil
		}
	}
	return -1, nil
}

// advance finds the next valid entry.
func (it *BunchedMapIterator) advance() {
	for {
		// Try next entry from current bunch.
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
				it.nextEntry = &e
				return
			}
			it.currentEntries = nil
		}

		// Stream next KV from FDB.
		if !it.rangeIter.Advance() {
			// Advance()==false on exhaustion OR a transient FDB error (1007, timeout);
			// capture the stored Get() error so Err() surfaces it instead of looking
			// like clean end-of-data (silent scan truncation).
			if _, err := it.rangeIter.Get(); err != nil {
				it.iterErr = err
			}
			it.done = true
			return
		}
		kv, err := it.rangeIter.Get()
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		if !bytes.HasPrefix(kv.Key, it.subspaceKey) {
			it.done = true
			return
		}

		boundaryKey, err := it.serializer.DeserializeKey(kv.Key, len(it.subspaceKey), len(kv.Key)-len(it.subspaceKey))
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
		// Handle continuation: skip entries up to and including the continuation key.
		// Keep checking until we find a bunch with a valid entry past the
		// continuation point — don't clear the flag until we've found one.
		if it.continuation != nil {
			startIdx, err = it.skipToContinuation(entries)
			if err != nil {
				it.iterErr = err
				it.done = true
				return
			}
			if startIdx < 0 {
				// No entries past the continuation in this bunch — keep
				// continuation active and try the next bunch.
				continue
			}
			it.continuation = nil // Found a valid entry, stop skipping.
		}

		it.currentEntries = entries
		it.entryIndex = startIdx
		e := entries[startIdx]
		it.nextEntry = &e
		return
	}
}

// GetContinuation returns a continuation token for resuming the scan.
func (it *BunchedMapIterator) GetContinuation() []byte {
	if it.lastKey == nil {
		return nil
	}
	if it.done && (it.limit <= 0 || it.returned < it.limit) {
		return nil
	}
	return it.serializer.SerializeKey(it.lastKey)
}

// Err returns the first error encountered during iteration, if any.
func (it *BunchedMapIterator) Err() error {
	return it.iterErr
}

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
	groupingColumns int  // textPosition + 1 (grouping columns + token column)
	logicalBounds   bool // the TupleRange restricts entries within one token map
}

// NewTextSubspaceSplitter creates a splitter for TEXT index scans.
func NewTextSubspaceSplitter(indexSubspace subspace.Subspace, groupingColumns int) *TextSubspaceSplitter {
	return &TextSubspaceSplitter{
		indexSubspace:   indexSubspace,
		groupingColumns: groupingColumns,
	}
}

// SetTextSubspaceSplitterLogicalBounds marks whether the scan's range restricts
// entries within one token map.
func SetTextSubspaceSplitterLogicalBounds(s *TextSubspaceSplitter, logicalBounds bool) {
	s.logicalBounds = logicalBounds
}

// TextSubspaceSplitterLogicalBounds reports whether the splitter enforces logical bounds.
func TextSubspaceSplitterLogicalBounds(s *TextSubspaceSplitter) bool {
	return s.logicalBounds
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
	return s.indexSubspace.Sub(TupleToElements(prefix)...), nil
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

// TupleToElements converts a Tuple to []tuple.TupleElement for subspace.Sub().
func TupleToElements(t tuple.Tuple) []tuple.TupleElement {
	result := make([]tuple.TupleElement, len(t))
	for i, v := range t {
		result[i] = v
	}
	return result
}

// tupleEqual compares two tuples for equality by comparing their packed representations.
func tupleEqual(a, b tuple.Tuple) bool {
	return bytes.Equal(a.Pack(), b.Pack())
}

// positionListsEqual checks if two position lists ([]int) are equal.
func positionListsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
