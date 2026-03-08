package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// SizeInfo tracks the storage metrics for a record.
// Matches Java's SplitHelper.SizeInfo / FDBStoredSizes.
type SizeInfo struct {
	KeyCount       int
	KeySize        int
	ValueSize      int
	IsSplit        bool
	VersionedInline bool
}

// saveWithSplit saves a serialized record, splitting it across multiple KV pairs
// if it exceeds SplitRecordSize and splitLongRecords is enabled.
// Does NOT handle version saves — the store layer manages versions separately
// because Go FDB bindings need context-level AddVersionMutation for versionstamps.
// Matches Java's SplitHelper.saveWithSplit() (data portion only).
func saveWithSplit(
	tx fdb.Transaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	serialized []byte,
	splitLongRecords bool,
	oldSizeInfo *SizeInfo,
	sizeInfo *SizeInfo,
) error {
	dataLen := len(serialized)

	// Clear previous record data
	clearPreviousRecord(tx, recordSubspace, primaryKey, splitLongRecords, oldSizeInfo)

	if dataLen > SplitRecordSize {
		if !splitLongRecords {
			return fmt.Errorf("record size %d exceeds limit %d and splitLongRecords is not enabled", dataLen, SplitRecordSize)
		}

		// Split the record into chunks
		splitIndex := StartSplitRecord
		offset := 0
		for offset < dataLen {
			end := offset + SplitRecordSize
			if end > dataLen {
				end = dataLen
			}
			chunk := serialized[offset:end]

			keyTuple := appendToTuple(primaryKey, splitIndex)
			key := recordSubspace.Pack(keyTuple)
			tx.Set(key, chunk)

			sizeInfo.KeyCount++
			sizeInfo.KeySize += len(key)
			sizeInfo.ValueSize += len(chunk)

			splitIndex++
			offset = end
		}
		sizeInfo.IsSplit = true
	} else {
		// Unsplit: single KV pair at suffix 0
		keyTuple := appendToTuple(primaryKey, UnsplitRecord)
		key := recordSubspace.Pack(keyTuple)
		tx.Set(key, serialized)

		sizeInfo.KeyCount = 1
		sizeInfo.KeySize = len(key)
		sizeInfo.ValueSize = dataLen
		sizeInfo.IsSplit = false
	}

	return nil
}

// clearPreviousRecord clears the old record's KV pairs before saving a new version.
// If oldSizeInfo indicates the previous record was split, clears the split range.
// Otherwise, clears just the unsplit key.
// Matches Java's SplitHelper behavior with clearBasedOnPreviousSizeInfo.
func clearPreviousRecord(
	tx fdb.Transaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	oldSizeInfo *SizeInfo,
) {
	if oldSizeInfo == nil {
		// No previous record — nothing to clear
		return
	}

	if oldSizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range (covers all suffixes: -1, 0, 1, 2, ...)
		// This is safe because the range only covers this primary key's data.
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Only unsplit key exists — clear just that
		keyTuple := appendToTuple(primaryKey, UnsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Also clear version key if it was stored inline
		if oldSizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, RecordVersionSuffix)
			tx.Clear(recordSubspace.Pack(versionKeyTuple))
		}
	}
}

// loadWithSplit loads a record that may be split across multiple KV pairs.
// Returns the reassembled record bytes and size info, or nil if not found.
// Matches Java's SplitHelper.loadWithSplit().
func loadWithSplit(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	sizeInfo *SizeInfo,
) ([]byte, error) {
	// Try unsplit first (most common case)
	unsplitKeyTuple := appendToTuple(primaryKey, UnsplitRecord)
	unsplitKey := recordSubspace.Pack(unsplitKeyTuple)

	value, err := tx.Get(fdb.Key(unsplitKey)).Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get unsplit record: %w", err)
	}

	if value != nil {
		sizeInfo.KeyCount = 1
		sizeInfo.KeySize = len(unsplitKey)
		sizeInfo.ValueSize = len(value)
		sizeInfo.IsSplit = false
		return value, nil
	}

	if !splitLongRecords {
		// Not found and splitting not enabled — record doesn't exist
		return nil, nil
	}

	// Check for split record: scan from suffix 1 onwards
	firstSplitKeyTuple := appendToTuple(primaryKey, StartSplitRecord)
	firstSplitKey := recordSubspace.Pack(firstSplitKeyTuple)

	firstValue, err := tx.Get(fdb.Key(firstSplitKey)).Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get first split chunk: %w", err)
	}

	if firstValue == nil {
		// Neither unsplit nor split — record doesn't exist
		return nil, nil
	}

	// Record is split — scan for remaining chunks
	// Range from suffix 2 to end of primary key subspace
	nextSplitKeyTuple := appendToTuple(primaryKey, StartSplitRecord+1)
	rangeBegin := recordSubspace.Pack(nextSplitKeyTuple)

	// End is exclusive: everything under this pk but beyond valid split suffixes.
	// Using the pk subspace end key covers all remaining split parts.
	pkSubspace := recordSubspace.Sub(primaryKey...)
	_, rangeEnd := pkSubspace.FDBRangeKeys()

	rangeResult := tx.GetRange(fdb.KeyRange{
		Begin: fdb.Key(rangeBegin),
		End:   rangeEnd,
	}, fdb.RangeOptions{
		Mode: fdb.StreamingModeWantAll,
	})

	kvs, err := rangeResult.GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("failed to read split record chunks: %w", err)
	}

	// Reassemble: first chunk + remaining chunks
	totalSize := len(firstValue)
	for _, kv := range kvs {
		totalSize += len(kv.Value)
	}

	result := make([]byte, 0, totalSize)
	result = append(result, firstValue...)

	sizeInfo.KeyCount = 1
	sizeInfo.KeySize = len(firstSplitKey)
	sizeInfo.ValueSize = len(firstValue)

	// Validate sequential indices and concatenate
	expectedIndex := StartSplitRecord + 1
	for _, kv := range kvs {
		keyTuple, unpackErr := recordSubspace.Unpack(kv.Key)
		if unpackErr != nil {
			return nil, fmt.Errorf("failed to unpack split key: %w", unpackErr)
		}
		if len(keyTuple) == 0 {
			return nil, fmt.Errorf("split record key has no suffix")
		}

		suffix, ok := keyTuple[len(keyTuple)-1].(int64)
		if !ok {
			return nil, fmt.Errorf("split record suffix is not int64: %T", keyTuple[len(keyTuple)-1])
		}

		// Skip version keys
		if suffix == RecordVersionSuffix {
			continue
		}

		if suffix != expectedIndex {
			return nil, fmt.Errorf("split record segments out of order: expected %d, got %d", expectedIndex, suffix)
		}

		result = append(result, kv.Value...)
		sizeInfo.KeyCount++
		sizeInfo.KeySize += len(kv.Key)
		sizeInfo.ValueSize += len(kv.Value)
		expectedIndex++
	}

	sizeInfo.IsSplit = true
	return result, nil
}

// deleteSplit deletes a record that may be split across multiple KV pairs.
// Returns true if a record was deleted.
// Matches Java's SplitHelper.deleteSplit().
func deleteSplit(
	tx fdb.Transaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	oldSizeInfo *SizeInfo,
) bool {
	if oldSizeInfo == nil {
		return false
	}

	if oldSizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Clear unsplit key only
		keyTuple := appendToTuple(primaryKey, UnsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Clear inline version if present
		if oldSizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, RecordVersionSuffix)
			tx.Clear(recordSubspace.Pack(versionKeyTuple))
		}
	}

	return true
}

// recordExistsWithSplit checks if a record exists, handling both split and unsplit formats.
func recordExistsWithSplit(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
) (bool, error) {
	// Check unsplit key
	unsplitKeyTuple := appendToTuple(primaryKey, UnsplitRecord)
	unsplitKey := recordSubspace.Pack(unsplitKeyTuple)

	value, err := tx.Get(fdb.Key(unsplitKey)).Get()
	if err != nil {
		return false, fmt.Errorf("failed to check record existence: %w", err)
	}
	if value != nil {
		return true, nil
	}

	if !splitLongRecords {
		return false, nil
	}

	// Check first split key
	firstSplitKeyTuple := appendToTuple(primaryKey, StartSplitRecord)
	firstSplitKey := recordSubspace.Pack(firstSplitKeyTuple)

	value, err = tx.Get(fdb.Key(firstSplitKey)).Get()
	if err != nil {
		return false, fmt.Errorf("failed to check split record existence: %w", err)
	}
	return value != nil, nil
}

// clearRecordKeyRange clears all keys for a primary key (version, unsplit, and all split chunks).
func clearRecordKeyRange(tx fdb.Transaction, recordSubspace subspace.Subspace, primaryKey tuple.Tuple) {
	pkSubspace := recordSubspace.Sub(primaryKey...)
	begin, end := pkSubspace.FDBRangeKeys()
	tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})
}

// appendToTuple creates a new tuple with the suffix appended (safe copy, no aliasing).
func appendToTuple(base tuple.Tuple, suffix int64) tuple.Tuple {
	result := make(tuple.Tuple, len(base)+1)
	copy(result, base)
	result[len(base)] = suffix
	return result
}
