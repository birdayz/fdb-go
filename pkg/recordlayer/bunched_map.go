package recordlayer

import (
	"bytes"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
type BunchedMap struct {
	serializer *TextIndexBunchedSerializer
	bunchSize  int
}

// NewBunchedMap creates a new BunchedMap with the given bunch size.
func NewBunchedMap(bunchSize int) *BunchedMap {
	return &BunchedMap{
		serializer: TextIndexBunchedSerializerInstance(),
		bunchSize:  bunchSize,
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
// Matches Java's BunchedMap.get().
func (m *BunchedMap) Get(tx fdb.ReadTransaction, ss subspace.Subspace, key tuple.Tuple) ([]int, bool, error) {
	subspaceKey := ss.Bytes()
	kv, found, err := m.entryForKey(tx, subspaceKey, key)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	mapKey := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
	entries := m.serializer.DeserializeEntries(mapKey, kv.Value)
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
func (m *BunchedMap) Put(tx fdb.Transaction, ss subspace.Subspace, key tuple.Tuple, value []int) ([]int, bool, error) {
	subspaceKey := ss.Bytes()
	keyBytes := joinBytes(subspaceKey, m.serializer.SerializeKey(key))

	// Snapshot range read to find signpost before and after the key.
	// Java: tr.snapshot().getRange(lastLessOrEqual(keyBytes), firstGreaterThan(keyBytes).add(1), ...)
	snap := tx.Snapshot()
	endSel := fdb.FirstGreaterThan(fdb.Key(keyBytes))
	endSel.Offset++ // .add(1) in Java
	rr := snap.GetRange(
		fdb.SelectorRange{
			Begin: fdb.LastLessOrEqual(fdb.Key(keyBytes)),
			End:   endSel,
		},
		fdb.RangeOptions{},
	)
	keyValues, err := rr.GetSliceWithError()
	if err != nil {
		return nil, false, err
	}

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

	newEntry := bunchedEntry{Key: key, Value: value}
	oldValue, hadOld := m.insertEntry(tx, subspaceKey, keyBytes, key, value, kvBefore, kvAfter, newEntry)
	return oldValue, hadOld, nil
}

// Remove removes a key from the map.
// Returns (oldValue, true, nil) if found and removed, (nil, false, nil) if not found.
// Matches Java's BunchedMap.remove().
func (m *BunchedMap) Remove(tx fdb.Transaction, ss subspace.Subspace, key tuple.Tuple) ([]int, bool, error) {
	subspaceKey := ss.Bytes()

	kv, found, err := m.entryForKey(tx, subspaceKey, key)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	mapKey := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
	entryList := m.serializer.DeserializeEntries(mapKey, kv.Value)

	foundIndex := -1
	for i, entry := range entryList {
		if tupleEqual(entry.Key, key) {
			foundIndex = i
			break
		}
	}

	if foundIndex == -1 {
		return nil, false, nil
	}

	oldEntry := entryList[foundIndex]

	// Read conflict range over entries being rewritten.
	m.addEntryListReadConflictRange(tx, subspaceKey, kv.Key, entryList)

	// Write conflict key for the map key being modified.
	keyBytes := joinBytes(subspaceKey, m.serializer.SerializeKey(key))
	if err := tx.AddWriteConflictKey(fdb.Key(keyBytes)); err != nil {
		return nil, false, err
	}

	if len(entryList) == 1 {
		// Only entry — just delete the signpost.
		tx.Clear(fdb.Key(kv.Key))
	} else {
		// Remove the entry and re-serialize.
		newEntryList := make([]bunchedEntry, 0, len(entryList)-1)
		newEntryList = append(newEntryList, entryList[:foundIndex]...)
		newEntryList = append(newEntryList, entryList[foundIndex+1:]...)

		var newKey []byte
		if foundIndex == 0 {
			// Removed first entry: signpost must change to the new first entry.
			tx.Clear(fdb.Key(kv.Key))
			newKey = joinBytes(subspaceKey, m.serializer.SerializeKey(newEntryList[0].Key))
		} else {
			newKey = kv.Key
		}

		newValue := m.serializer.SerializeEntries(newEntryList)
		tx.Set(fdb.Key(newKey), newValue)
	}

	return oldEntry.Value, true, nil
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

	var lastKey []byte // packed key for comparison
	for _, kv := range kvs {
		boundaryKey := m.serializer.DeserializeKey(kv.Key, len(subspaceKey), len(kv.Key)-len(subspaceKey))
		boundaryKeyPacked := boundaryKey.Pack()
		if lastKey != nil && bytes.Compare(boundaryKeyPacked, lastKey) < 0 {
			return &BunchedMapException{Message: "boundary key out of order"}
		}
		lastKey = boundaryKeyPacked

		keys := m.serializer.DeserializeKeys(boundaryKey, kv.Value)
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
func (m *BunchedMap) entryForKey(tx fdb.ReadTransaction, subspaceKey []byte, key tuple.Tuple) (fdb.KeyValue, bool, error) {
	keyBytes := joinBytes(subspaceKey, m.serializer.SerializeKey(key))

	// Add a read conflict key for the map key being accessed.
	// ReadTransaction may be a Snapshot which doesn't support AddReadConflictKey,
	// but the callers (Get uses ReadTransaction, Put/Remove use Transaction) handle this.
	// For Get, we skip the conflict key since it's a read-only operation using the
	// snapshot directly from entryForKey. For Put/Remove, the Transaction is used.
	//
	// Java always adds the read conflict key via tr (the Transaction).
	// For Get, Java passes the transaction into entryForKey (which adds the conflict key),
	// but Get is wrapped in runAsync which takes a TransactionContext.
	// Since Go's Get takes a ReadTransaction, we conditionally add the conflict key.
	if txn, ok := tx.(fdb.Transaction); ok {
		if err := txn.AddReadConflictKey(fdb.Key(keyBytes)); err != nil {
			return fdb.KeyValue{}, false, err
		}
	}

	// Snapshot range read: lastLessOrEqual(keyBytes) to firstGreaterThan(keyBytes).
	snap := tx.Snapshot()
	rr := snap.GetRange(
		fdb.SelectorRange{
			Begin: fdb.LastLessOrEqual(fdb.Key(keyBytes)),
			End:   fdb.FirstGreaterThan(fdb.Key(keyBytes)),
		},
		fdb.RangeOptions{},
	)
	keyValues, err := rr.GetSliceWithError()
	if err != nil {
		return fdb.KeyValue{}, false, err
	}

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
func (m *BunchedMap) addEntryListReadConflictRange(tx fdb.Transaction, subspaceKey, keyBytes []byte, entryList []bunchedEntry) {
	end := joinBytes(subspaceKey, m.serializer.SerializeKey(entryList[len(entryList)-1].Key), zeroArray)
	// Ignore errors — matches Java which doesn't check return value.
	_ = tx.AddReadConflictRange(fdb.KeyRange{
		Begin: fdb.Key(keyBytes),
		End:   fdb.Key(end),
	})
}

// insertAlone creates a new signpost with a single entry.
// Matches Java's BunchedMap.insertAlone().
func (m *BunchedMap) insertAlone(tx fdb.Transaction, keyBytes []byte, entry bunchedEntry) {
	_ = tx.AddReadConflictKey(fdb.Key(keyBytes))
	valueBytes := m.serializer.SerializeEntries([]bunchedEntry{entry})
	tx.Set(fdb.Key(keyBytes), valueBytes)
}

// writeEntryListWithoutChecking writes an entry list to FDB with proper conflict ranges
// but without size checking.
// Matches Java's BunchedMap.writeEntryListWithoutChecking().
func (m *BunchedMap) writeEntryListWithoutChecking(tx fdb.Transaction, subspaceKey, keyBytes []byte,
	oldKv *fdb.KeyValue, newKey []byte, entryList []bunchedEntry, serializedBytes []byte) {

	// Order matters: add read conflict range BEFORE writing (see Java comment about
	// explicit read conflict ranges skipping keys in write cache).
	m.addEntryListReadConflictRange(tx, subspaceKey, newKey, entryList)

	if oldKv != nil && !bytes.Equal(oldKv.Key, newKey) {
		tx.Clear(fdb.Key(oldKv.Key))
	}

	tx.Set(fdb.Key(newKey), serializedBytes)

	if !bytes.Equal(keyBytes, newKey) {
		_ = tx.AddWriteConflictKey(fdb.Key(keyBytes))
	}
}

// writeEntryList serializes and writes an entry list, handling size overflow by splitting.
// isFirst/isLast indicate position relative to the insertion point.
// Matches Java's BunchedMap.writeEntryList().
func (m *BunchedMap) writeEntryList(tx fdb.Transaction, subspaceKey, keyBytes []byte,
	oldKv *fdb.KeyValue, newKey []byte, entryList []bunchedEntry,
	kvAfter *fdb.KeyValue, isFirst, isLast bool) {

	serializedBytes := m.serializer.SerializeEntries(entryList)

	if len(serializedBytes) > bunchedMapMaxValueSize {
		if isFirst || len(entryList) == 1 {
			m.insertAlone(tx, keyBytes, entryList[0])
		} else if isLast {
			m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entryList[len(entryList)-1])
		} else {
			// Split down the middle.
			splitPoint := len(entryList) / 2
			firstEntries := entryList[:splitPoint]
			firstSerialized := m.serializer.SerializeEntries(firstEntries)
			secondEntries := entryList[splitPoint:]
			secondSerialized := m.serializer.SerializeEntries(secondEntries)
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, oldKv, newKey, firstEntries, firstSerialized)
			secondKey := joinBytes(subspaceKey, m.serializer.SerializeKey(secondEntries[0].Key))
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, nil, secondKey, secondEntries, secondSerialized)
		}
	} else {
		if m.serializer.CanAppend() && isLast && len(entryList) > 1 && oldKv != nil && bytes.Equal(oldKv.Key, newKey) {
			// APPEND_IF_FITS optimization.
			m.addEntryListReadConflictRange(tx, subspaceKey, newKey, entryList)
			appendBytes := m.serializer.SerializeEntry(entryList[len(entryList)-1].Key, entryList[len(entryList)-1].Value)
			tx.AppendIfFits(fdb.Key(newKey), appendBytes)
			_ = tx.AddWriteConflictKey(fdb.Key(keyBytes))
		} else {
			m.writeEntryListWithoutChecking(tx, subspaceKey, keyBytes, oldKv, newKey, entryList, serializedBytes)
		}

		// Write conflict ranges for "gaps" claimed during expansion.
		if isFirst && len(entryList) >= 2 {
			begin := keyBytes
			end := joinBytes(subspaceKey, m.serializer.SerializeKey(entryList[1].Key))
			_ = tx.AddWriteConflictRange(fdb.KeyRange{
				Begin: fdb.Key(begin),
				End:   fdb.Key(end),
			})
		}
		if isLast && len(entryList) >= 2 {
			begin := joinBytes(subspaceKey, m.serializer.SerializeKey(entryList[len(entryList)-2].Key))
			end := keyBytes
			_ = tx.AddWriteConflictRange(fdb.KeyRange{
				Begin: fdb.Key(begin),
				End:   fdb.Key(end),
			})
		}
	}
}

// insertAfter tries to prepend the entry into kvAfter's bunch, or creates
// a standalone signpost if kvAfter doesn't exist or is full.
// Matches Java's BunchedMap.insertAfter().
func (m *BunchedMap) insertAfter(tx fdb.Transaction, subspaceKey, keyBytes []byte,
	kvAfter *fdb.KeyValue, entry bunchedEntry) {

	if kvAfter == nil {
		m.insertAlone(tx, keyBytes, entry)
		return
	}

	afterKey := m.serializer.DeserializeKey(kvAfter.Key, len(subspaceKey), len(kvAfter.Key)-len(subspaceKey))
	afterEntryList := m.serializer.DeserializeEntries(afterKey, kvAfter.Value)

	if len(afterEntryList) >= m.bunchSize {
		m.insertAlone(tx, keyBytes, entry)
		return
	}

	// Prepend our entry to the next bunch's list.
	newEntryList := make([]bunchedEntry, 0, len(afterEntryList)+1)
	newEntryList = append(newEntryList, entry)
	newEntryList = append(newEntryList, afterEntryList...)
	m.writeEntryList(tx, subspaceKey, keyBytes, kvAfter, keyBytes, newEntryList, nil, true, false)
}

// insertEntry handles the core insertion logic after signpost lookup.
// Returns (oldValue, true) if the key already existed, (nil, false) if new.
// Matches Java's BunchedMap.insertEntry().
func (m *BunchedMap) insertEntry(tx fdb.Transaction, subspaceKey, keyBytes []byte,
	key tuple.Tuple, value []int, kvBefore, kvAfter *fdb.KeyValue, entry bunchedEntry) ([]int, bool) {

	if kvBefore == nil {
		m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entry)
		return nil, false
	}

	beforeKey := m.serializer.DeserializeKey(kvBefore.Key, len(subspaceKey), len(kvBefore.Key)-len(subspaceKey))
	beforeEntryList := m.serializer.DeserializeEntries(beforeKey, kvBefore.Value)

	// Find insertion point.
	insertIndex := 0
	for insertIndex < len(beforeEntryList) && compareTuples(key, beforeEntryList[insertIndex].Key) > 0 {
		insertIndex++
	}

	if insertIndex < len(beforeEntryList) && compareTuples(key, beforeEntryList[insertIndex].Key) == 0 {
		// Key already exists — update if value differs.
		oldEntry := beforeEntryList[insertIndex]
		if !positionListsEqual(oldEntry.Value, value) {
			newEntryList := make([]bunchedEntry, len(beforeEntryList))
			copy(newEntryList, beforeEntryList)
			newEntryList[insertIndex] = entry
			m.writeEntryList(tx, subspaceKey, keyBytes, kvBefore, kvBefore.Key,
				newEntryList, kvAfter, false, false)
		} else {
			// Value unchanged — just add read conflict key for linearizability.
			_ = tx.AddReadConflictKey(fdb.Key(keyBytes))
		}
		return oldEntry.Value, true
	}

	if insertIndex < len(beforeEntryList) {
		// Inserting in the middle of the bunch.
		newEntryList := make([]bunchedEntry, 0, len(beforeEntryList)+1)
		newEntryList = append(newEntryList, beforeEntryList[:insertIndex]...)
		newEntryList = append(newEntryList, entry)
		newEntryList = append(newEntryList, beforeEntryList[insertIndex:]...)

		if len(newEntryList) <= m.bunchSize {
			m.writeEntryList(tx, subspaceKey, keyBytes, kvBefore, kvBefore.Key,
				newEntryList, kvAfter, false, false)
		} else {
			// Split the bunch.
			splitPoint := len(newEntryList) / 2
			m.writeEntryList(tx, subspaceKey, keyBytes, kvBefore, kvBefore.Key,
				newEntryList[:splitPoint], nil, false, false)
			secondEntries := newEntryList[splitPoint:]
			secondKey := joinBytes(subspaceKey, m.serializer.SerializeKey(secondEntries[0].Key))
			m.writeEntryList(tx, subspaceKey, keyBytes, nil, secondKey,
				secondEntries, kvAfter, false, false)
		}
		return nil, false
	}

	// Inserting after all entries in the before bunch.
	if len(beforeEntryList) < m.bunchSize {
		// Append to the end of the current bunch.
		newEntryList := make([]bunchedEntry, 0, len(beforeEntryList)+1)
		newEntryList = append(newEntryList, beforeEntryList...)
		newEntryList = append(newEntryList, entry)
		m.writeEntryList(tx, subspaceKey, keyBytes, kvBefore, kvBefore.Key,
			newEntryList, kvAfter, false, true)
	} else {
		// Bunch is full — insert into the next bunch.
		m.insertAfter(tx, subspaceKey, keyBytes, kvAfter, entry)
	}
	return nil, false
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
