package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// sizeInfo tracks the storage metrics for a record.
// Matches Java's SplitHelper.sizeInfo / FDBStoredSizes.
type sizeInfo struct {
	KeyCount        int
	KeySize         int
	ValueSize       int
	IsSplit         bool
	VersionedInline bool
}

// saveWithSplit saves a serialized record, splitting it across multiple KV pairs
// if it exceeds splitRecordSize and splitLongRecords is enabled.
// Does NOT handle version saves — the store layer manages versions separately
// because Go FDB bindings need context-level AddVersionMutation for versionstamps.
// Matches Java's SplitHelper.saveWithSplit() (data portion only).
func saveWithSplit(
	tx fdb.Transaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	serialized []byte,
	splitLongRecords bool,
	oldsizeInfo *sizeInfo,
	sizeInfo *sizeInfo,
) error {
	if len(primaryKey) == 0 {
		return fmt.Errorf("primary key must not be empty")
	}

	dataLen := len(serialized)

	// Clear previous record data
	clearPreviousRecord(tx, recordSubspace, primaryKey, splitLongRecords, oldsizeInfo)

	if dataLen > splitRecordSize {
		if !splitLongRecords {
			return fmt.Errorf("record size %d exceeds limit %d and splitLongRecords is not enabled", dataLen, splitRecordSize)
		}

		// Split the record into chunks
		splitIndex := startSplitRecord
		offset := 0
		for offset < dataLen {
			end := offset + splitRecordSize
			if end > dataLen {
				end = dataLen
			}
			chunk := serialized[offset:end]

			keyTuple := appendToTuple(primaryKey, splitIndex)
			key := recordSubspace.Pack(keyTuple)
			tx.SetBytes(key, chunk)

			sizeInfo.KeyCount++
			sizeInfo.KeySize += len(key)
			sizeInfo.ValueSize += len(chunk)

			splitIndex++
			offset = end
		}
		sizeInfo.IsSplit = true
	} else {
		// Unsplit: single KV pair at suffix 0
		key := tuple.PackConcatWithPrefix(recordSubspace.Bytes(), primaryKey, unsplitSuffix)
		tx.SetBytes(key, serialized)

		sizeInfo.KeyCount = 1
		sizeInfo.KeySize = len(key)
		sizeInfo.ValueSize = dataLen
		sizeInfo.IsSplit = false
	}

	return nil
}

// clearPreviousRecord clears the old record's KV pairs before saving a new version.
// If oldsizeInfo indicates the previous record was split, clears the split range.
// Otherwise, clears just the unsplit key.
// Matches Java's SplitHelper behavior with clearBasedOnPrevioussizeInfo.
func clearPreviousRecord(
	tx fdb.Transaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	oldsizeInfo *sizeInfo,
) {
	if oldsizeInfo == nil {
		// No previous record — nothing to clear
		return
	}

	if oldsizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range (covers all suffixes: -1, 0, 1, 2, ...)
		// This is safe because the range only covers this primary key's data.
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Only unsplit key exists — clear just that
		keyTuple := appendToTuple(primaryKey, unsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Also clear version key if it was stored inline
		if oldsizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, recordVersionSuffix)
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
	sizeInfo *sizeInfo,
) ([]byte, error) {
	// Try unsplit first (most common case).
	// Use PackConcatWithPrefix to avoid the intermediate tuple allocation
	// from appendToTuple(primaryKey, unsplitRecord).
	unsplitKey := tuple.PackConcatWithPrefix(recordSubspace.Bytes(), primaryKey, unsplitSuffix)

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
	firstSplitKeyTuple := appendToTuple(primaryKey, startSplitRecord)
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
	nextSplitKeyTuple := appendToTuple(primaryKey, startSplitRecord+1)
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
	expectedIndex := startSplitRecord + 1
	for _, kv := range kvs {
		keyTuple, unpackErr := fastSubspaceUnpack(kv.Key, len(recordSubspace.Bytes()))
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

		// Version keys are part of the record's storage footprint
		if suffix == recordVersionSuffix {
			sizeInfo.KeyCount++
			sizeInfo.KeySize += len(kv.Key)
			sizeInfo.ValueSize += len(kv.Value)
			sizeInfo.VersionedInline = true
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
	oldsizeInfo *sizeInfo,
) bool {
	if len(primaryKey) == 0 {
		return false
	}

	if oldsizeInfo == nil {
		return false
	}

	if oldsizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Clear unsplit key only
		keyTuple := appendToTuple(primaryKey, unsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Clear inline version if present
		if oldsizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, recordVersionSuffix)
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
	unsplitKeyTuple := appendToTuple(primaryKey, unsplitRecord)
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
	firstSplitKeyTuple := appendToTuple(primaryKey, startSplitRecord)
	firstSplitKey := recordSubspace.Pack(firstSplitKeyTuple)

	value, err = tx.Get(fdb.Key(firstSplitKey)).Get()
	if err != nil {
		return false, fmt.Errorf("failed to check split record existence: %w", err)
	}
	return value != nil, nil
}

// clearRecordKeyRange clears all keys for a primary key (version, unsplit, and all split chunks).
// Empty primaryKey is a no-op to prevent catastrophic data loss (would clear entire records subspace).
func clearRecordKeyRange(tx fdb.Transaction, recordSubspace subspace.Subspace, primaryKey tuple.Tuple) {
	if len(primaryKey) == 0 {
		return
	}
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
