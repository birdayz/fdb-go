package recordlayer

import (
	"encoding/binary"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// unsplitSuffix is the pre-computed tuple for the unsplit record suffix (0).
// Used by PackConcatWithPrefix to avoid appendToTuple allocation.
var unsplitSuffix = tuple.Tuple{unsplitRecord}

// SaveRecordBatch saves multiple records with pipelined existence checks.
//
// Instead of N sequential blocking FDB reads (one per record in SaveRecord),
// this method issues all existence-check Gets up front as futures, then
// collects them in one batch. This turns N round trips into ~1 round trip
// for the existence checks, significantly improving throughput for batch inserts.
//
// Semantically equivalent to calling SaveRecord N times. All records are
// saved in the current transaction.
func (store *FDBRecordStore) SaveRecordBatch(
	records []proto.Message,
) ([]*FDBStoredRecord[proto.Message], error) {
	if len(records) == 0 {
		return nil, nil
	}

	tx := store.context.Transaction()
	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()

	// --- Phase 1: Extract PKs and issue all Get futures (non-blocking) ---
	type pendingRecord struct {
		record     proto.Message
		recordType *RecordType
		primaryKey tuple.Tuple
		unsplitKey fdb.Key // pre-computed, reused for save
		future     fdb.FutureByteSlice
	}

	pending := make([]pendingRecord, len(records))

	for i, record := range records {
		if record == nil {
			return nil, fmt.Errorf("record %d is nil", i)
		}

		recordTypeName := string(record.ProtoReflect().Descriptor().Name())
		recordType := store.metaData.GetRecordType(recordTypeName)
		if recordType == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type: %s", recordTypeName)}
		}
		if recordType.PrimaryKey == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", recordTypeName)}
		}

		keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, nil, record)
		if err != nil {
			return nil, fmt.Errorf("record %d: extract primary key: %w", i, err)
		}

		primaryKey := make(tuple.Tuple, len(keyValues))
		for j, v := range keyValues {
			primaryKey[j] = v
		}

		// Issue the Get future — this is the key optimization.
		// tx.Get returns immediately; the read is async over the wire.
		unsplitKeyTuple := appendToTuple(primaryKey, unsplitRecord)
		unsplitKey := fdb.Key(recordsSubspace.Pack(unsplitKeyTuple))
		future := tx.Get(unsplitKey)

		pending[i] = pendingRecord{
			record:     record,
			recordType: recordType,
			primaryKey: primaryKey,
			unsplitKey: unsplitKey,
			future:     future,
		}
	}

	// Pre-compute the record count FDB key (same for all records in batch).
	var countFDBKey []byte
	countKey := store.metaData.GetRecordCountKey()
	if countKey != nil && !store.isRecordCountDisabled() {
		// For EmptyKey (ungrouped count), the key is always the same.
		// For grouped counts, we'd need per-record evaluation — fall back to per-record.
		if _, ok := countKey.(*EmptyKeyExpression); ok {
			countSubspace := store.subspace.Sub(RecordCountKey)
			countFDBKey = countSubspace.Pack(tuple.Tuple{})
		}
	}

	// --- Phase 2: Collect futures + save each record ---
	// By now FDB has pipelined all N Gets over the wire. Collecting them
	// costs ~1 round trip total instead of N sequential round trips.

	results := make([]*FDBStoredRecord[proto.Message], len(records))
	var insertCount int64

	// Hold stateMu.RLock for the entire batch to avoid per-record lock/unlock.
	// Also validate update lock once (same for all records).
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.validateRecordUpdateAllowedLocked(); err != nil {
		return nil, err
	}

	for i := range pending {
		p := &pending[i]

		// Collect the existence check
		oldValue, err := p.future.Get()
		if err != nil {
			return nil, fmt.Errorf("record %d: existence check: %w", i, err)
		}
		oldRecordExists := oldValue != nil

		// Handle split records (rare for batch inserts, but correct)
		var oldsizeInfo sizeInfo
		if !oldRecordExists && splitEnabled {
			var err error
			oldValue, err = loadSplitOnly(tx, recordsSubspace, p.primaryKey, &oldsizeInfo)
			if err != nil {
				return nil, fmt.Errorf("record %d: split check: %w", i, err)
			}
			oldRecordExists = oldValue != nil
		} else if oldRecordExists {
			oldsizeInfo.KeyCount = 1
			oldsizeInfo.KeySize = len(p.unsplitKey)
			oldsizeInfo.ValueSize = len(oldValue)
			oldsizeInfo.IsSplit = false
		}

		// Serialize
		data, err := serializeUnion(p.record, p.recordType)
		if err != nil {
			return nil, &RecordSerializationError{Cause: err}
		}

		// Save — fast path for unsplit new records (most common in batch ingest)
		var newsizeInfo sizeInfo
		if !oldRecordExists && len(data) <= splitRecordSize {
			// Direct Set using pre-computed key — avoids appendToTuple + Pack in saveWithSplit
			tx.Set(p.unsplitKey, data)
			newsizeInfo.KeyCount = 1
			newsizeInfo.KeySize = len(p.unsplitKey)
			newsizeInfo.ValueSize = len(data)
		} else {
			var oldsizeInfoPtr *sizeInfo
			if oldRecordExists {
				oldsizeInfoPtr = &oldsizeInfo
			}
			if err := saveWithSplit(tx, recordsSubspace, p.primaryKey, data,
				splitEnabled, oldsizeInfoPtr, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save: %w", i, err)
			}
		}

		// Record count
		stored := &FDBStoredRecord[proto.Message]{
			PrimaryKey: p.primaryKey,
			RecordType: p.recordType,
			Record:     p.record,
			KeyCount:   newsizeInfo.KeyCount,
			KeySize:    newsizeInfo.KeySize,
			ValueSize:  newsizeInfo.ValueSize,
			Split:      newsizeInfo.IsSplit,
		}
		if !oldRecordExists {
			if countFDBKey != nil {
				// Batched: just count inserts, single ADD at end
				insertCount++
			} else if countKey != nil {
				// Grouped count or non-empty key: per-record ADD
				if err := store.addRecordCount(p.record, littleEndianInt64One); err != nil {
					return nil, fmt.Errorf("record %d: record count: %w", i, err)
				}
			}
		}

		// Secondary indexes
		var oldRecord *FDBStoredRecord[proto.Message]
		if oldRecordExists && oldValue != nil {
			oldMsg, err := store.deserializeRecord(oldValue, p.recordType)
			if err == nil {
				oldRecord = &FDBStoredRecord[proto.Message]{
					PrimaryKey: p.primaryKey,
					RecordType: p.recordType,
					Record:     oldMsg,
				}
			}
		}
		if err := store.updateSecondaryIndexesLocked(oldRecord, stored); err != nil {
			return nil, fmt.Errorf("record %d: index update: %w", i, err)
		}

		results[i] = stored
	}

	// Batch record count: single atomic ADD for all inserts
	if countFDBKey != nil && insertCount > 0 {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(insertCount))
		tx.Add(fdb.Key(countFDBKey), buf[:])
	}

	return results, nil
}

// SaveRecordBatchInsertOnly saves multiple NEW records without existence checks.
// Skips the pipelined Get phase entirely — assumes all records are fresh inserts.
// This eliminates one FDB round trip per batch and all existence-check overhead.
//
// WARNING: If a record with the same primary key already exists, it will be
// silently overwritten. Record count will be incorrectly incremented. Index
// entries from the old record will NOT be cleaned up. Only use this when you
// can guarantee unique primary keys (e.g. UUID/random IDs, monotonic sequences).
//
// For the metrognome usage event ingest path, every event has a unique event_id
// PK, so this is safe and provides maximum throughput.
func (store *FDBRecordStore) SaveRecordBatchInsertOnly(
	records []proto.Message,
) ([]*FDBStoredRecord[proto.Message], error) {
	if len(records) == 0 {
		return nil, nil
	}

	tx := store.context.Transaction()
	// Disable RYW cache for insert-only path — we never read back any written
	// keys within this transaction. Matches C++ READ_YOUR_WRITES_DISABLE which
	// skips both read and write caching. Saves ~400 allocs per batch (string
	// key conversions + value copies eliminated).
	tx.Options().SetReadYourWritesDisable()
	// Pre-size mutation and conflict range slices to avoid growth allocs.
	// Each record produces ~4 FDB ops (record Set + VALUE Set + COUNT Atomic + SUM Atomic).
	numIndexes := len(store.metaData.GetAllIndexes())
	tx.Options().EnsureMutationCapacity(len(records) * (1 + numIndexes))
	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()

	// Pre-compute count key
	var countFDBKey []byte
	countKey := store.metaData.GetRecordCountKey()
	if countKey != nil && !store.isRecordCountDisabled() {
		if _, ok := countKey.(*EmptyKeyExpression); ok {
			countSubspace := store.subspace.Sub(RecordCountKey)
			countFDBKey = countSubspace.Pack(tuple.Tuple{})
		}
	}

	results := make([]*FDBStoredRecord[proto.Message], len(records))

	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.validateRecordUpdateAllowedLocked(); err != nil {
		return nil, err
	}

	// Try to compile the PK expression for the batch.
	// Compiled evaluator reuses a single tupleAppender across all records,
	// eliminating the EvaluateFlat []any allocation per record.
	var compiledPK *compiledKeyEvaluator
	var pkAppender tupleAppender
	if len(records) > 0 {
		typeName := string(records[0].ProtoReflect().Descriptor().Name())
		rt := store.metaData.GetRecordType(typeName)
		if rt != nil && rt.PrimaryKey != nil {
			compiledPK = compileKeyExpression(rt.PrimaryKey)
			if compiledPK != nil {
				pkAppender.elements = make([]tuple.TupleElement, 0, 8)
			}
		}
	}

	for i, record := range records {
		if record == nil {
			return nil, fmt.Errorf("record %d is nil", i)
		}

		recordTypeName := string(record.ProtoReflect().Descriptor().Name())
		recordType := store.metaData.GetRecordType(recordTypeName)
		if recordType == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type: %s", recordTypeName)}
		}
		if recordType.PrimaryKey == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", recordTypeName)}
		}

		// Fast path: compiled PK evaluator (reuses tupleAppender — zero []any alloc
		// for EvaluateFlat. Still copies into tuple.Tuple for the result).
		var primaryKey tuple.Tuple
		if compiledPK != nil {
			if err := compiledPK.evaluate(&pkAppender, nil, record); err == nil {
				primaryKey = make(tuple.Tuple, len(pkAppender.elements))
				copy(primaryKey, pkAppender.elements)
			}
		}
		if primaryKey == nil {
			keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, nil, record)
			if err != nil {
				return nil, fmt.Errorf("record %d: extract primary key: %w", i, err)
			}
			primaryKey = make(tuple.Tuple, len(keyValues))
			for j, v := range keyValues {
				primaryKey[j] = v
			}
		}

		// Serialize
		data, err := serializeUnion(record, recordType)
		if err != nil {
			return nil, &RecordSerializationError{Cause: err}
		}

		// Direct write — no existence check, no split check.
		if !splitEnabled || len(data) <= splitRecordSize {
			// Pack PK + unsplit suffix directly, avoiding appendToTuple alloc.
			unsplitKey := fdb.Key(tuple.PackConcatWithPrefix(
				recordsSubspace.Bytes(), primaryKey, unsplitSuffix))
			tx.Set(unsplitKey, data)
		} else {
			var newsizeInfo sizeInfo
			if err := saveWithSplit(tx, recordsSubspace, primaryKey, data,
				splitEnabled, nil, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save: %w", i, err)
			}
		}

		stored := &FDBStoredRecord[proto.Message]{
			PrimaryKey: primaryKey,
			RecordType: recordType,
			Record:     record,
		}

		if err := store.updateSecondaryIndexesLocked(nil, stored); err != nil {
			return nil, fmt.Errorf("record %d: index update: %w", i, err)
		}

		results[i] = stored
	}

	// Batch record count
	if countFDBKey != nil {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(records)))
		tx.Add(fdb.Key(countFDBKey), buf[:])
	}

	return results, nil
}

// loadSplitOnly checks for a split record without checking unsplit first.
// Used by SaveRecordBatch when the unsplit check was already done via pipeline.
func loadSplitOnly(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	si *sizeInfo,
) ([]byte, error) {
	firstSplitKeyTuple := appendToTuple(primaryKey, startSplitRecord)
	firstSplitKey := recordSubspace.Pack(firstSplitKeyTuple)

	rangeEnd := recordSubspace.Pack(appendToTuple(primaryKey, 256))

	kvs := tx.GetRange(
		fdb.KeyRange{Begin: fdb.Key(firstSplitKey), End: fdb.Key(rangeEnd)},
		fdb.RangeOptions{},
	).GetSliceOrPanic()

	if len(kvs) == 0 {
		return nil, nil
	}

	var totalData []byte
	for _, kv := range kvs {
		totalData = append(totalData, kv.Value...)
	}
	si.KeyCount = len(kvs)
	si.IsSplit = true
	si.ValueSize = len(totalData)
	if len(kvs) > 0 {
		si.KeySize = len(kvs[0].Key)
	}
	return totalData, nil
}
