package recordlayer

import (
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

	// The batch fast path precomputes pk+0 record keys; it does not implement the
	// legacy bare-key (omit_unsplit_record_suffix) layout. Fall back to per-record
	// SaveRecord — which handles every layout — for legacy stores. (Java has no
	// batch API, so this is a Go-only optimization and the fallback is semantically
	// identical to N SaveRecord calls.)
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return nil, fmt.Errorf("load store state: %w", err)
	}
	if store.omitUnsplitRecordSuffix() {
		results := make([]*FDBStoredRecord[proto.Message], len(records))
		for i, rec := range records {
			saved, err := store.SaveRecord(rec)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			results[i] = saved
		}
		return results, nil
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
		unsplitFut fdb.FutureByteSlice
		splitFut   fdb.FutureByteSlice // nil if !splitEnabled
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

		// Issue Get futures for both unsplit and first-split keys.
		// All futures are pipelined: N×2 frames queued, one TCP flush.
		unsplitKeyTuple := appendToTuple(primaryKey, unsplitRecord)
		unsplitKey := fdb.Key(recordsSubspace.Pack(unsplitKeyTuple))
		unsplitFut := tx.Get(unsplitKey)

		var splitFut fdb.FutureByteSlice
		if splitEnabled {
			firstSplitKeyTuple := appendToTuple(primaryKey, startSplitRecord)
			firstSplitKey := fdb.Key(recordsSubspace.Pack(firstSplitKeyTuple))
			splitFut = tx.Get(firstSplitKey)
		}

		pending[i] = pendingRecord{
			record:     record,
			recordType: recordType,
			primaryKey: primaryKey,
			unsplitKey: unsplitKey,
			unsplitFut: unsplitFut,
			splitFut:   splitFut,
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

	// Lazy-load store state before acquiring lock (Build() path, matches Java).
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return nil, fmt.Errorf("load store state: %w", err)
	}
	// Hold stateMu.RLock for the entire batch to avoid per-record lock/unlock.
	// Also validate update lock once (same for all records).
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.validateRecordUpdateAllowedLocked(); err != nil {
		return nil, err
	}

	for i := range pending {
		p := &pending[i]

		// Resolve pipelined existence checks (both unsplit + split).
		unsplitVal, err := p.unsplitFut.Get()
		if err != nil {
			return nil, fmt.Errorf("record %d: unsplit check: %w", i, err)
		}

		var oldValue []byte
		var oldsizeInfo sizeInfo
		if unsplitVal != nil {
			oldValue = unsplitVal
			oldsizeInfo.KeyCount = 1
			oldsizeInfo.KeySize = len(p.unsplitKey)
			oldsizeInfo.ValueSize = len(unsplitVal)
			oldsizeInfo.IsSplit = false
		} else if splitEnabled {
			splitVal, splitErr := p.splitFut.Get()
			if splitErr != nil {
				return nil, fmt.Errorf("record %d: split check: %w", i, splitErr)
			}
			if splitVal != nil {
				// Record exists as split — load split chunks only (unsplit
				// already checked via pipeline and returned nil).
				oldValue, err = loadSplitOnly(tx, recordsSubspace, p.primaryKey, &oldsizeInfo)
				if err != nil {
					return nil, fmt.Errorf("record %d: split load: %w", i, err)
				}
			}
		}
		oldRecordExists := oldValue != nil

		// Serialize
		data, err := serializeUnion(p.record, p.recordType)
		if err != nil {
			return nil, &RecordSerializationError{Cause: err}
		}

		// Save — fast path for unsplit new records (most common in batch ingest)
		var newsizeInfo sizeInfo
		if !oldRecordExists && len(data) <= splitRecordSize {
			// Direct Set using pre-computed key — avoids appendToTuple + Pack in saveWithSplit
			tx.SetBytes(p.unsplitKey, data)
			newsizeInfo.KeyCount = 1
			newsizeInfo.KeySize = len(p.unsplitKey)
			newsizeInfo.ValueSize = len(data)
		} else {
			var oldsizeInfoPtr *sizeInfo
			if oldRecordExists {
				oldsizeInfoPtr = &oldsizeInfo
			}
			if err := saveWithSplit(tx, recordsSubspace, p.primaryKey, data,
				splitEnabled, false, oldsizeInfoPtr, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save: %w", i, err)
			}
		}

		// Save version if versioning is enabled. One SET_VERSIONSTAMPED_VALUE
		// mutation per record — no round trip, just added to the write set.
		var savedVersion *FDBRecordVersion
		if store.metaData.IsStoreRecordVersions() {
			localVer := store.context.ClaimLocalVersion()
			version, verErr := IncompleteVersion(localVer)
			if verErr != nil {
				return nil, fmt.Errorf("record %d: create incomplete version: %w", i, verErr)
			}
			if err := store.saveRecordVersion(p.primaryKey, version, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save version: %w", i, err)
			}
			savedVersion = version
		}

		// Record count
		stored := &FDBStoredRecord[proto.Message]{
			PrimaryKey: p.primaryKey,
			RecordType: p.recordType,
			Record:     p.record,
			Version:    savedVersion,
			Store:      store,
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
		if oldRecordExists {
			oldRT, oldMsg, err := store.deserializeAndDiscover(oldValue)
			if err != nil {
				return nil, fmt.Errorf("record %d: deserialize old record: %w", i, err)
			}
			oldRecord = &FDBStoredRecord[proto.Message]{
				PrimaryKey: p.primaryKey,
				RecordType: oldRT,
				Record:     oldMsg,
				Store:      store,
			}
			if store.metaData.IsStoreRecordVersions() && store.hasVersionIndex() {
				if ver, verErr := store.LoadRecordVersion(p.primaryKey, false); verErr == nil {
					oldRecord.Version = ver
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
		tx.AddBytes(countFDBKey, buf[:])
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

	kvs, err := tx.GetRange(
		fdb.KeyRange{Begin: fdb.Key(firstSplitKey), End: fdb.Key(rangeEnd)},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("split record range read: %w", err)
	}

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
