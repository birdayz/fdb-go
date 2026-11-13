// Portions derived from FoundationDB Record Layer (BunchedMap.java,
// BunchedMapIterator.java, StoreTimer.java, KeyRange.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"sync"

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
)

// zeroArray is used to construct exclusive upper bounds for conflict ranges.
// Appending 0x00 to a key K gives the first key strictly greater than K.
// Matches Java's BunchedMap.ZERO_ARRAY.
var zeroArray = []byte{0x00}

// bunchedEntry is a convenience alias for BunchedEntry specialized for TEXT index use.
type bunchedEntry = BunchedEntry[tuple.Tuple, []int]

// BunchedMap is a FoundationDB-backed map that bunches close keys together.
// Wire-compatible with Java's com.apple.foundationdb.map.BunchedMap.
//
// Currently specialized for TEXT index use (K = tuple.Tuple, V = []int).
//
// When a StoreTimer is set, BunchedMap instruments all writes, deletes, and
// range reads with index-level counters matching Java's InstrumentedBunchedMap.
type BunchedMap struct {
	serializer *TextIndexBunchedSerializer
	bunchSize  int
	timer      *StoreTimer
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
func NewInstrumentedBunchedMap(bunchSize int, timer *StoreTimer) *BunchedMap {
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
	m.timer.Increment(CountSaveIndexKey)
	m.timer.IncrementBy(CountSaveIndexKeyBytes, int64(len(key)))
	m.timer.IncrementBy(CountSaveIndexValueBytes, int64(len(value)))
	if oldValue != nil {
		m.timer.IncrementBy(CountDeleteIndexValueBytes, int64(len(oldValue)))
	}
}

// instrumentDelete records a delete operation in the timer.
// Matches Java's InstrumentedBunchedMap.instrumentDelete().
func (m *BunchedMap) instrumentDelete(key, oldValue []byte) {
	if m.timer == nil {
		return
	}
	m.timer.Increment(CountDeleteIndexKey)
	m.timer.IncrementBy(CountDeleteIndexKeyBytes, int64(len(key)))
	if oldValue != nil {
		m.timer.IncrementBy(CountDeleteIndexValueBytes, int64(len(oldValue)))
	}
}

// instrumentRangeRead records a range read result in the timer.
// Matches Java's InstrumentedBunchedMap.instrumentRangeRead().
func (m *BunchedMap) instrumentRangeRead(kvs []fdb.KeyValue) {
	if m.timer == nil {
		return
	}
	for _, kv := range kvs {
		m.timer.Increment(CountLoadIndexKey)
		m.timer.IncrementBy(CountLoadIndexKeyBytes, int64(len(kv.Key)))
		m.timer.IncrementBy(CountLoadIndexValueBytes, int64(len(kv.Value)))
	}
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

// Put inserts or updates a key-value pair in the map.
// Returns (oldValue, true, nil) if the key already existed, (nil, false, nil) if new.
// Matches Java's BunchedMap.put().
func (m *BunchedMap) Put(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple, value []int) ([]int, bool, error) {
	p := m.startPut(tx, ss, key, value)
	return p.finish()
}

// pendingBunchedPut overlaps independent maps’ snapshot reads; finish applies writes
// and conflict ranges on the caller’s goroutine (BunchedMap.java:477-550).
type pendingBunchedPut struct {
	m             *BunchedMap
	tx            fdb.WritableTransaction
	subspaceKey   []byte
	keyBytes      []byte
	keyBytesAfter []byte
	value         []int
	read          pendingSnapshotRead
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

// startPut issues Put's read.
func (m *BunchedMap) startPut(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple, value []int) pendingBunchedPut {
	subspaceKey := ss.Bytes()
	// joinBytes(subspaceKey, SerializeKey(key)) in one allocation.
	keyBytes := key.PackWithPrefix(subspaceKey)
	return m.startPutPrepacked(tx, subspaceKey, keyBytes, nil, value)
}

func (m *BunchedMap) startPutPrepacked(tx fdb.WritableTransaction, subspaceKey, keyBytes, keyBytesAfter []byte, value []int) pendingBunchedPut {
	// Snapshot range read to find signpost before and after the key.
	// Java: tr.snapshot().getRange(lastLessOrEqual(keyBytes), firstGreaterThan(keyBytes).add(1), ...)
	endSel := fdb.FirstGreaterThan(fdb.Key(keyBytes))
	endSel.Offset++ // .add(1) in Java
	read := startSnapshotRead(tx, fdb.SelectorRange{
		Begin: fdb.LastLessOrEqual(fdb.Key(keyBytes)),
		End:   endSel,
	})
	return pendingBunchedPut{m: m, tx: tx, subspaceKey: subspaceKey, keyBytes: keyBytes, keyBytesAfter: keyBytesAfter, value: value, read: read}
}

// finish waits for the put's read and applies it.
func (p *pendingBunchedPut) finish() ([]int, bool, error) {
	m, tx, subspaceKey, keyBytes, value := p.m, p.tx, p.subspaceKey, p.keyBytes, p.value
	keyValues, err := p.read.wait()
	if err != nil {
		return nil, false, err
	}
	m.instrumentRangeRead(keyValues)

	var kvBefore, kvAfter *fdb.KeyValue
	for i := range keyValues {
		kv := &keyValues[i]
		if !bytes.HasPrefix(kv.Key, subspaceKey) {
			continue
		}
		if bytes.Compare(keyBytes, kv.Key) < 0 {
			kvAfter = kv
			break // no need to continue after kvAfter is set
		}
		if bytes.Compare(kv.Key, keyBytes) <= 0 {
			kvBefore = kv
		}
	}

	// Sanity checks matching Java.
	if kvBefore != nil && (bytes.Compare(keyBytes, kvBefore.Key) < 0 || !bytes.HasPrefix(kvBefore.Key, subspaceKey)) {
		return nil, false, &BunchedMapException{Message: "database key before map key compared incorrectly"}
	}
	if kvAfter != nil && (bytes.Compare(keyBytes, kvAfter.Key) >= 0 || !bytes.HasPrefix(kvAfter.Key, subspaceKey)) {
		return nil, false, &BunchedMapException{Message: "database key after map key compared incorrectly"}
	}

	// The new entry's key is its packed tuple, as Java's serializeKey caches
	// it in the key (Tuple.pack).
	newEntry := bunchItem{key: keyBytes[len(subspaceKey):], positions: value}
	oldValue, hadOld, err := m.insertEntry(tx, subspaceKey, keyBytes, p.keyBytesAfter, value, kvBefore, kvAfter, newEntry)
	if err != nil {
		return nil, false, err
	}
	return oldValue, hadOld, nil
}

// bunchItemPool reuses decoded lists after each operation; items reference only
// its read results and keys, never transaction-retained data.
var bunchItemPool = sync.Pool{New: func() any {
	items := make([]bunchItem, 0, textIndexBunchSize+1)
	return &items
}}

// decodeBunch returns pooled items and their pool handle; validation matches
// Java deserializeKey/deserializeEntries.
func decodeBunch(subspaceKey []byte, kv *fdb.KeyValue, what string) (items []bunchItem, pooled *[]bunchItem, err error) {
	pooled = bunchItemPool.Get().(*[]bunchItem)
	items, err = decodeBunchItems(kv.Key[len(subspaceKey):], kv.Value, (*pooled)[:0])
	if err != nil {
		releaseBunch(pooled, items)
		return nil, nil, fmt.Errorf("bunched map %s: deserialize entries: %w", what, err)
	}
	return items, pooled, nil
}

func releaseBunch(pooled *[]bunchItem, items []bunchItem) {
	clear(items[:cap(items)])
	*pooled = items[:0]
	bunchItemPool.Put(pooled)
}

// searchBunch binary-searches sorted entries for the first packed key >= key,
// equivalent to Java’s linear insertEntry search.
func searchBunch(items []bunchItem, key []byte) int {
	return sort.Search(len(items), func(i int) bool { return bytes.Compare(key, items[i].key) <= 0 })
}

// Remove removes a key from the map.
// Returns (oldValue, true, nil) if found and removed, (nil, false, nil) if not found.
// Matches Java's BunchedMap.remove().
func (m *BunchedMap) Remove(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple) ([]int, bool, error) {
	return m.startRemove(tx, ss, key).finish()
}

// pendingBunchedRemove overlaps entryForKey I/O; finish applies writes and conflict ranges
// (BunchedMap.java:617-640).
type pendingBunchedRemove struct {
	m           *BunchedMap
	tx          fdb.WritableTransaction
	subspaceKey []byte
	key         tuple.Tuple
	entry       *pendingEntryForKey
	err         error
}

// startRemove issues Remove's read.
func (m *BunchedMap) startRemove(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple) *pendingBunchedRemove {
	subspaceKey := ss.Bytes()
	entry, err := m.startEntryForKey(tx, subspaceKey, key)
	return &pendingBunchedRemove{m: m, tx: tx, subspaceKey: subspaceKey, key: key, entry: entry, err: err}
}

// finish waits for the remove's read and applies it.
func (p *pendingBunchedRemove) finish() ([]int, bool, error) {
	if p.err != nil {
		return nil, false, p.err
	}
	m, tx, subspaceKey, key := p.m, p.tx, p.subspaceKey, p.key
	kv, found, err := p.entry.finish()
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	entryList, pooled, err := decodeBunch(subspaceKey, &kv, "remove")
	if err != nil {
		return nil, false, err
	}
	defer releaseBunch(pooled, entryList)

	// Java finds the entry equal to key by a linear scan (BunchedMap.java:
	// 622-628); the bunch is in key order, so the search finds the same one.
	keyBytes := key.PackWithPrefix(subspaceKey)
	foundIndex := searchBunch(entryList, keyBytes[len(subspaceKey):])
	if foundIndex == len(entryList) || !bytes.Equal(entryList[foundIndex].key, keyBytes[len(subspaceKey):]) {
		return nil, false, nil
	}
	oldValue, err := itemPositions(&entryList[foundIndex])
	if err != nil {
		return nil, false, err
	}

	// Read conflict range over entries being rewritten.
	m.addEntryListReadConflictRange(tx, subspaceKey, kv.Key, entryList)

	// Write conflict key for the map key being modified.
	if err := tx.AddWriteConflictKeyBytes(keyBytes); err != nil {
		return nil, false, err
	}

	if len(entryList) == 1 {
		// Only entry — just delete the signpost. Java instruments the delete
		// without the old value here (BunchedMap.java:639-640).
		tx.ClearBytes(kv.Key)
		m.instrumentDelete(kv.Key, nil)
	} else {
		// Remove the entry and re-serialize.
		newEntryList := slices.Delete(entryList, foundIndex, foundIndex+1)

		var newKey, oldBytes []byte
		if foundIndex == 0 {
			// Removed first entry: signpost must change to the new first entry.
			tx.ClearBytes(kv.Key)
			m.instrumentDelete(kv.Key, kv.Value)
			newKey = joinBytes(subspaceKey, newEntryList[0].key)
		} else {
			newKey, oldBytes = kv.Key, kv.Value
		}

		newValue, err := serializeBunchItems(newEntryList)
		if err != nil {
			return nil, false, fmt.Errorf("bunched map remove: %w", err)
		}
		tx.SetBytes(newKey, newValue)
		m.instrumentWrite(newKey, newValue, oldBytes)
	}

	return oldValue, true, nil
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

// addEntryListReadConflictRange adds a read conflict range covering all entries
// in the entry list. Range is [signpostKey, lastEntryKey + 0x00).
// Matches Java's BunchedMap.addEntryListReadConflictRange().
func (m *BunchedMap) addEntryListReadConflictRange(tx fdb.WritableTransaction, subspaceKey, keyBytes []byte, entryList []bunchItem) {
	end := joinBytes(subspaceKey, entryList[len(entryList)-1].key, zeroArray)
	// Ignore errors — matches Java which doesn't check return value.
	_ = tx.AddReadConflictRangeBytes(keyBytes, end)
}

// insertAlone creates a new signpost with a single entry.
// Matches Java's BunchedMap.insertAlone().
func (m *BunchedMap) insertAlone(tx fdb.WritableTransaction, keyBytes []byte, entry bunchItem) error {
	_ = tx.AddReadConflictKeyBytes(keyBytes)
	valueBytes, err := serializeBunchItems([]bunchItem{entry})
	if err != nil {
		return err
	}
	tx.SetBytes(keyBytes, valueBytes)
	m.instrumentWrite(keyBytes, valueBytes, nil)
	return nil
}

// writeEntryListWithoutChecking writes an entry list to FDB with proper conflict ranges
// but without size checking.
// Matches Java's BunchedMap.writeEntryListWithoutChecking().
func (m *BunchedMap) writeEntryListWithoutChecking(tx fdb.WritableTransaction, subspaceKey, keyBytes []byte,
	oldKv *fdb.KeyValue, newKey []byte, entryList []bunchItem, serializedBytes []byte,
) {
	// Order matters: add read conflict range BEFORE writing (see Java comment about
	// explicit read conflict ranges skipping keys in write cache).
	m.addEntryListReadConflictRange(tx, subspaceKey, newKey, entryList)

	if oldKv != nil && !bytes.Equal(oldKv.Key, newKey) {
		tx.ClearBytes(oldKv.Key)
		m.instrumentDelete(oldKv.Key, oldKv.Value)
	}

	var oldValue []byte
	if oldKv != nil && bytes.Equal(oldKv.Key, newKey) {
		oldValue = oldKv.Value
	}
	tx.SetBytes(newKey, serializedBytes)
	m.instrumentWrite(newKey, serializedBytes, oldValue)

	if !bytes.Equal(keyBytes, newKey) {
		_ = tx.AddWriteConflictKeyBytes(keyBytes)
	}
}

// writeEntryList is Java's BunchedMap.writeEntryList: it writes entryList, splitting it if
// too large; isFirst/isLast place it at the insertion point, keyBytesAfter is keyBytes+\x00 or nil.
func (m *BunchedMap) writeEntryList(tx fdb.WritableTransaction, subspaceKey, keyBytes, keyBytesAfter []byte,
	oldKv *fdb.KeyValue, newKey []byte, entryList []bunchItem,
	kvAfter *fdb.KeyValue, isFirst, isLast bool,
) error {
	// Only size selects the branch; serialize a full list only when writing it, not for append.
	serializedSize, err := bunchItemsSize(entryList)
	if err != nil {
		return err
	}

	if serializedSize > bunchedMapMaxValueSize {
		if isFirst || len(entryList) == 1 {
			if err := m.insertAlone(tx, keyBytes, entryList[0]); err != nil {
				return err
			}
		} else if isLast {
			if err := m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entryList[len(entryList)-1]); err != nil {
				return err
			}
		} else {
			// Split down the middle.
			splitPoint := len(entryList) / 2
			firstEntries := entryList[:splitPoint]
			firstSerialized, err := serializeBunchItems(firstEntries)
			if err != nil {
				return err
			}
			secondEntries := entryList[splitPoint:]
			secondSerialized, err := serializeBunchItems(secondEntries)
			if err != nil {
				return err
			}
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, oldKv, newKey, firstEntries, firstSerialized)
			secondKey := joinBytes(subspaceKey, secondEntries[0].key)
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, nil, secondKey, secondEntries, secondSerialized)
		}
	} else {
		if m.serializer.CanAppend() && isLast && len(entryList) > 1 && oldKv != nil && bytes.Equal(oldKv.Key, newKey) {
			// APPEND_IF_FITS optimization. The appended entry is the put's own, so the
			// entry list's conflict end is keyBytes+\x00 (addEntryListReadConflictRange).
			if keyBytesAfter != nil && bytes.Equal(entryList[len(entryList)-1].key, keyBytes[len(subspaceKey):]) {
				_ = tx.AddReadConflictRangeBytes(newKey, keyBytesAfter)
			} else {
				m.addEntryListReadConflictRange(tx, subspaceKey, newKey, entryList)
			}
			appendBytes, err := serializeBunchItem(&entryList[len(entryList)-1])
			if err != nil {
				return err
			}
			tx.AppendIfFitsBytes(newKey, appendBytes)
			// Java instruments the appended bytes, without the old value as
			// the total size grows (BunchedMap.java:345).
			m.instrumentWrite(newKey, appendBytes, nil)
			_ = tx.AddWriteConflictKeyBytes(keyBytes)
		} else {
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, oldKv, newKey, entryList, encodeBunchItems(entryList, serializedSize))
		}

		// Write conflict ranges for "gaps" claimed during expansion.
		if isFirst && len(entryList) >= 2 {
			begin := keyBytes
			end := joinBytes(subspaceKey, entryList[1].key)
			_ = tx.AddWriteConflictRangeBytes(begin, end)
		}
		if isLast && len(entryList) >= 2 {
			begin := joinBytes(subspaceKey, entryList[len(entryList)-2].key)
			end := keyBytes
			_ = tx.AddWriteConflictRangeBytes(begin, end)
		}
	}
	return nil
}

// insertAfter tries to prepend the entry into kvAfter's bunch, or creates
// a standalone signpost if kvAfter doesn't exist or is full.
// Matches Java's BunchedMap.insertAfter().
func (m *BunchedMap) insertAfter(tx fdb.WritableTransaction, subspaceKey, keyBytes []byte,
	kvAfter *fdb.KeyValue, entry bunchItem,
) error {
	if kvAfter == nil {
		return m.insertAlone(tx, keyBytes, entry)
	}

	afterEntryList, pooled, err := decodeBunch(subspaceKey, kvAfter, "insert after")
	if err != nil {
		return err
	}
	defer releaseBunch(pooled, afterEntryList)

	if len(afterEntryList) >= m.bunchSize {
		return m.insertAlone(tx, keyBytes, entry)
	}

	// Prepend our entry to the next bunch's list.
	newEntryList := slices.Insert(afterEntryList, 0, entry)
	return m.writeEntryList(tx, subspaceKey, keyBytes, nil, kvAfter, keyBytes, newEntryList, nil, true, false)
}

// insertEntry handles the core insertion logic after signpost lookup.
// Returns (oldValue, true, nil) if the key already existed, (nil, false, nil) if new.
// Matches Java's BunchedMap.insertEntry().
func (m *BunchedMap) insertEntry(tx fdb.WritableTransaction, subspaceKey, keyBytes, keyBytesAfter []byte,
	value []int, kvBefore, kvAfter *fdb.KeyValue, entry bunchItem,
) ([]int, bool, error) {
	if kvBefore == nil {
		if err := m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entry); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}

	beforeEntryList, pooled, err := decodeBunch(subspaceKey, kvBefore, "insert")
	if err != nil {
		return nil, false, err
	}
	defer releaseBunch(pooled, beforeEntryList)

	// Find insertion point.
	insertIndex := searchBunch(beforeEntryList, entry.key)

	if insertIndex < len(beforeEntryList) && bytes.Equal(entry.key, beforeEntryList[insertIndex].key) {
		// Key already exists — update if value differs.
		oldValue, err := itemPositions(&beforeEntryList[insertIndex])
		if err != nil {
			return nil, false, err
		}
		if !positionListsEqual(oldValue, value) {
			beforeEntryList[insertIndex] = entry
			if err := m.writeEntryList(tx, subspaceKey, keyBytes, nil, kvBefore, kvBefore.Key,
				beforeEntryList, kvAfter, false, false); err != nil {
				return nil, false, err
			}
		} else {
			// Value unchanged — just add read conflict key for linearizability.
			_ = tx.AddReadConflictKeyBytes(keyBytes)
		}
		return oldValue, true, nil
	}

	if insertIndex < len(beforeEntryList) {
		// Inserting in the middle of the bunch.
		newEntryList := slices.Insert(beforeEntryList, insertIndex, entry)

		if len(newEntryList) <= m.bunchSize {
			if err := m.writeEntryList(tx, subspaceKey, keyBytes, nil, kvBefore, kvBefore.Key,
				newEntryList, kvAfter, false, false); err != nil {
				return nil, false, err
			}
		} else {
			// Split the bunch.
			splitPoint := len(newEntryList) / 2
			if err := m.writeEntryList(tx, subspaceKey, keyBytes, nil, kvBefore, kvBefore.Key,
				newEntryList[:splitPoint], nil, false, false); err != nil {
				return nil, false, err
			}
			secondEntries := newEntryList[splitPoint:]
			secondKey := joinBytes(subspaceKey, secondEntries[0].key)
			if err := m.writeEntryList(tx, subspaceKey, keyBytes, nil, nil, secondKey,
				secondEntries, kvAfter, false, false); err != nil {
				return nil, false, err
			}
		}
		return nil, false, nil
	}

	// Inserting after all entries in the before bunch.
	if len(beforeEntryList) < m.bunchSize {
		// Append to the end of the current bunch.
		newEntryList := append(beforeEntryList, entry)
		if err := m.writeEntryList(tx, subspaceKey, keyBytes, keyBytesAfter, kvBefore, kvBefore.Key,
			newEntryList, kvAfter, false, true); err != nil {
			return nil, false, err
		}
	} else {
		// Bunch is full — insert into the next bunch.
		if err := m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entry); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// ContainsKey checks if a key exists in the map.
// Matches Java's BunchedMap.containsKey().
func (m *BunchedMap) ContainsKey(tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple) (bool, error) {
	subspaceKey := ss.Bytes()
	kv, found, err := m.entryForKey(tx, subspaceKey, key)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	mapKey, err := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
	if err != nil {
		return false, fmt.Errorf("bunched map contains: deserialize key: %w", err)
	}
	keys, err := m.serializer.DeserializeKeys(mapKey, kv.Value)
	if err != nil {
		return false, fmt.Errorf("bunched map contains: deserialize keys: %w", err)
	}
	for _, k := range keys {
		if tupleEqual(k, key) {
			return true, nil
		}
	}
	return false, nil
}

// Compact repacks entries in the map to minimize the number of FDB keys used.
// Each call processes up to keyLimit FDB keys (0 = all keys) and returns a
// continuation token for multi-transaction compaction. Returns (nil, nil) when
// compaction is complete.
// Matches Java's BunchedMap.compact().
func (m *BunchedMap) Compact(tx fdb.WritableTransaction, ss subspace.Subspace, keyLimit int, continuation []byte) ([]byte, error) {
	subspaceKey := ss.Bytes()

	// Build range to read.
	var rr fdb.RangeResult
	if continuation == nil {
		rr = tx.GetRange(ss, fdb.RangeOptions{Limit: keyLimit})
	} else {
		contKey := append(append([]byte{}, subspaceKey...), continuation...)
		_, endKey := ss.FDBRangeKeys()
		rr = tx.GetRange(fdb.KeyRange{
			Begin: fdb.Key(contKey),
			End:   endKey,
		}, fdb.RangeOptions{Limit: keyLimit})
	}

	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return nil, err
	}
	if len(kvs) == 0 {
		return nil, nil
	}

	// Accumulate all entries from the read keys, then rewrite in optimal bunches.
	var allEntries []bunchedEntry
	for _, kv := range kvs {
		mapKey, err := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
		if err != nil {
			return nil, fmt.Errorf("bunched map compact: deserialize key: %w", err)
		}
		entries, err := m.serializer.DeserializeEntries(mapKey, kv.Value)
		if err != nil {
			return nil, fmt.Errorf("bunched map compact: deserialize entries: %w", err)
		}
		allEntries = append(allEntries, entries...)
		// Clear old key.
		tx.Clear(fdb.Key(kv.Key))
	}

	// Rewrite in bunchSize-aligned groups, respecting MAX_VALUE_SIZE.
	for len(allEntries) > 0 {
		end := m.bunchSize
		if end > len(allEntries) {
			end = len(allEntries)
		}

		bunch := allEntries[:end]
		serialized, err := m.serializer.SerializeEntries(bunch)
		if err != nil {
			return nil, fmt.Errorf("bunched map compact: %w", err)
		}

		// If serialized size exceeds limit, reduce bunch size until it fits.
		for len(serialized) > bunchedMapMaxValueSize && end > 1 {
			end--
			bunch = allEntries[:end]
			serialized, err = m.serializer.SerializeEntries(bunch)
			if err != nil {
				return nil, fmt.Errorf("bunched map compact: %w", err)
			}
		}

		newKey := joinBytes(subspaceKey, m.serializer.SerializeKey(bunch[0].Key))
		tx.Set(fdb.Key(newKey), serialized)
		allEntries = allEntries[end:]
	}

	// Return continuation if we might have more data.
	if keyLimit > 0 && len(kvs) == keyLimit {
		lastKV := kvs[len(kvs)-1]
		return lastKV.Key[len(subspaceKey):], nil
	}
	return nil, nil
}

// BunchedMapIterator iterates over a single BunchedMap's entries.
// Matches Java's BunchedMapIterator.
type BunchedMapIterator struct {
	serializer   *TextIndexBunchedSerializer
	subspaceKey  []byte
	rangeIter    rangeIterator
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

	it := &BunchedMapIterator{
		serializer:   m.serializer,
		subspaceKey:  subspaceKey,
		rangeIter:    rangeResult.Iterator(),
		reverse:      reverse,
		limit:        limit,
		continuation: continuation,
		entryIndex:   -1,
	}
	return it
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

		// Handle continuation: skip entries up to and including the continuation key.
		// Keep checking until we find a bunch with a valid entry past the
		// continuation point — don't clear the flag until we've found one.
		if it.continuation != nil {
			contKey, err := it.serializer.DeserializeKey(it.continuation, 0, len(it.continuation))
			if err != nil {
				it.iterErr = err
				it.done = true
				return
			}
			startIdx := -1
			if it.reverse {
				for i := len(entries) - 1; i >= 0; i-- {
					if compareTuples(contKey, entries[i].Key) > 0 {
						startIdx = i
						break
					}
				}
			} else {
				for i := 0; i < len(entries); i++ {
					if compareTuples(contKey, entries[i].Key) < 0 {
						startIdx = i
						break
					}
				}
			}
			if startIdx < 0 {
				// No entries past the continuation in this bunch — keep
				// continuation active and try the next bunch.
				continue
			}
			it.continuation = nil // Found a valid entry, stop skipping.
			it.currentEntries = entries
			it.entryIndex = startIdx
			e := entries[startIdx]
			it.nextEntry = &e
			return
		}

		it.currentEntries = entries
		startIdx := 0
		if it.reverse {
			startIdx = len(entries) - 1
		}
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
