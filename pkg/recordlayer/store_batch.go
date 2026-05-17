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
				// Record exists as split — full load needed for index maintenance.
				oldValue, err = loadWithSplit(tx, recordsSubspace, p.primaryKey, splitEnabled, &oldsizeInfo)
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
				splitEnabled, oldsizeInfoPtr, &newsizeInfo); err != nil {
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
			oldMsg, err := store.deserializeRecord(oldValue, p.recordType)
			if err == nil {
				oldRecord = &FDBStoredRecord[proto.Message]{
					PrimaryKey: p.primaryKey,
					RecordType: p.recordType,
					Record:     oldMsg,
					Store:      store,
				}
				// Load old version for VERSION index cleanup on update.
				// One FDB read per updated record — only when version indexes exist.
				if store.metaData.IsStoreRecordVersions() && store.hasVersionIndex() {
					if ver, verErr := store.LoadRecordVersion(p.primaryKey, false); verErr == nil {
						oldRecord.Version = ver
					}
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

// SaveRecordBatchInsertOnly saves multiple NEW records without existence checks.
// Skips the pipelined Get phase entirely — assumes all records are fresh inserts.
// This eliminates one FDB round trip per batch and all existence-check overhead.
//
// WARNING: If a record with the same primary key already exists, it will be
// silently overwritten. Record count will be incorrectly incremented. Index
// entries from the old record will NOT be cleaned up. Only use this when you
// can guarantee unique primary keys (e.g. UUID/random IDs, monotonic sequences).
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
	// Disable write conflict ranges: all record keys are unique (guaranteed by
	// InsertOnly contract) and all atomic index mutations commute (ADD, MAX, MIN).
	// Eliminates ~200 conflict range entries per batch from the commit request.
	tx.Options().SetWriteConflictsDisabled()
	// Pre-size mutation slices to avoid growth allocs.
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

	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return nil, fmt.Errorf("load store state: %w", err)
	}
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
	if len(records) > 0 && records[0] != nil {
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

		// Save version if versioning is enabled.
		var savedVersion *FDBRecordVersion
		if store.metaData.IsStoreRecordVersions() {
			localVer := store.context.ClaimLocalVersion()
			version, verErr := IncompleteVersion(localVer)
			if verErr != nil {
				return nil, fmt.Errorf("record %d: create incomplete version: %w", i, verErr)
			}
			if err := store.saveRecordVersion(primaryKey, version, nil); err != nil {
				return nil, fmt.Errorf("record %d: save version: %w", i, err)
			}
			savedVersion = version
		}

		stored := &FDBStoredRecord[proto.Message]{
			PrimaryKey: primaryKey,
			RecordType: recordType,
			Record:     record,
			Version:    savedVersion,
			Store:      store,
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
		tx.AddBytes(countFDBKey, buf[:])
	}

	return results, nil
}

// InsertBatch is the maximum-throughput insert path. Like SaveRecordBatchInsertOnly
// but returns no results — the caller only gets an error. This eliminates per-record
// allocations for result structs, primary key tuples, and sizeInfo tracking.
//
// PRECONDITIONS:
//   - All records MUST be the same proto message type. Mixed types are rejected with an error.
//   - All records must have unique primary keys. Existing records silently overwritten.
//   - If the schema has UNIQUE indexes, all records must have unique values for those
//     indexes. Uniqueness is not enforced — duplicates silently corrupt the index.
//   - This is a Go-only API, not present in Java Record Layer.
func (store *FDBRecordStore) InsertBatch(records []proto.Message) error {
	if len(records) == 0 {
		return nil
	}
	if records[0] == nil {
		return fmt.Errorf("record %d is nil", 0)
	}

	tx := store.context.Transaction()
	tx.Options().SetReadYourWritesDisable()
	tx.Options().SetWriteConflictsDisabled()
	numIndexes := len(store.metaData.GetAllIndexes())
	tx.Options().EnsureMutationCapacity(len(records) * (1 + numIndexes))
	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()
	// Skip uniqueness checks — InsertBatch caller guarantees unique keys.
	// Eliminates FDB GetRange per entry for UNIQUE indexes (~15% CPU saving).
	store.skipUniquenessChecks = true
	defer func() { store.skipUniquenessChecks = false }()

	// Pre-compute count key
	var countFDBKey []byte
	countKey := store.metaData.GetRecordCountKey()
	if countKey != nil && !store.isRecordCountDisabled() {
		if _, ok := countKey.(*EmptyKeyExpression); ok {
			countSubspace := store.subspace.Sub(RecordCountKey)
			countFDBKey = countSubspace.Pack(tuple.Tuple{})
		}
	}

	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return fmt.Errorf("load store state: %w", err)
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if err := store.validateRecordUpdateAllowedLocked(); err != nil {
		return err
	}

	// Cache record type + indexes for the batch — all records are the same type.
	typeName := string(records[0].ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(typeName)
	if recordType == nil {
		return &MetaDataError{Message: fmt.Sprintf("unknown record type: %s", typeName)}
	}
	if recordType.PrimaryKey == nil {
		return &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", typeName)}
	}

	// Pre-resolve index maintainers to avoid per-record map lookups.
	typeIndexes := store.metaData.GetIndexesForRecordType(recordType.Name)
	universalIndexes := store.metaData.GetUniversalIndexes()
	type cachedMaintainer struct {
		index      *Index
		maintainer IndexMaintainer
	}
	maintainers := make([]cachedMaintainer, 0, len(typeIndexes)+len(universalIndexes))
	for _, idx := range typeIndexes {
		if store.shouldMaintainIndex(idx.Name) {
			m, err := store.getIndexMaintainer(idx)
			if err != nil {
				return err
			}
			maintainers = append(maintainers, cachedMaintainer{index: idx, maintainer: m})
		}
	}
	for _, idx := range universalIndexes {
		if store.shouldMaintainIndex(idx.Name) {
			m, err := store.getIndexMaintainer(idx)
			if err != nil {
				return err
			}
			maintainers = append(maintainers, cachedMaintainer{index: idx, maintainer: m})
		}
	}

	// Compiled PK evaluator + reusable appender
	var compiledPK *compiledKeyEvaluator
	var pkAppender tupleAppender
	if recordType.PrimaryKey != nil {
		compiledPK = compileKeyExpression(recordType.PrimaryKey)
		if compiledPK != nil {
			pkAppender.elements = make([]tuple.TupleElement, 0, 8)
		}
	}

	// Reusable stored record — populated per-record, not returned to caller.
	var stored FDBStoredRecord[proto.Message]

	// Shared serialization buffer — all records' serialized bytes are sub-slices
	// of this buffer. Avoids per-record make([]byte, totalSize) in serializeUnion.
	serBuf := make([]byte, 0, len(records)*128)

	// Shared key buffer — all packed FDB keys are sub-slices of this buffer.
	// Avoids per-key make([]byte) in Pack functions. Estimated 80 bytes per key,
	// ~4 keys per record (record + VALUE + COUNT + SUM).
	keyBuf := make([]byte, 0, len(records)*320)
	store.batchKeyBuf = &keyBuf
	// Shared packer for DirectPacker — avoids GetPacker/PutPacker pool churn per index entry.
	batchPk := tuple.GetPacker()
	store.batchPacker = batchPk
	defer func() {
		store.batchKeyBuf = nil
		store.batchPacker = nil
		tuple.PutPacker(batchPk)
	}()

	for i, record := range records {
		if record == nil {
			return fmt.Errorf("record %d is nil", i)
		}

		// Validate all records are the same type (InsertBatch precondition).
		// Mixed types cause silent corruption: wrong union field number in
		// serialization and wrong index maintainers applied.
		if rtn := string(record.ProtoReflect().Descriptor().Name()); rtn != typeName {
			return fmt.Errorf("record %d: type %q does not match batch type %q (InsertBatch requires all same type)", i, rtn, typeName)
		}

		// Evaluate PK — reuse pkAppender across records (no alloc per record).
		var primaryKey tuple.Tuple
		if compiledPK != nil {
			if err := compiledPK.evaluate(&pkAppender, nil, record); err == nil {
				// Use pkAppender.elements directly for packing (consumed immediately).
				// For index maintainers, create a sub-slice reference (no copy).
				primaryKey = tuple.Tuple(pkAppender.elements)
			}
		}
		if primaryKey == nil {
			keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, nil, record)
			if err != nil {
				return fmt.Errorf("record %d: extract primary key: %w", i, err)
			}
			primaryKey = make(tuple.Tuple, len(keyValues))
			for j, v := range keyValues {
				primaryKey[j] = v
			}
		}

		// Serialize into shared buffer (1 alloc for all records instead of 50).
		data, err := serializeUnionInto(record, recordType, &serBuf)
		if err != nil {
			return &RecordSerializationError{Cause: err}
		}

		// Direct write
		if !splitEnabled || len(data) <= splitRecordSize {
			unsplitKey := tuple.PackConcatInto(&keyBuf,
				recordsSubspace.Bytes(), primaryKey, unsplitSuffix)
			tx.SetBytes(unsplitKey, data)
		} else {
			var newsizeInfo sizeInfo
			if err := saveWithSplit(tx, recordsSubspace, primaryKey, data,
				splitEnabled, nil, &newsizeInfo); err != nil {
				return fmt.Errorf("record %d: save: %w", i, err)
			}
		}

		// Save version if versioning is enabled.
		if store.metaData.IsStoreRecordVersions() {
			localVer := store.context.ClaimLocalVersion()
			version, verErr := IncompleteVersion(localVer)
			if verErr != nil {
				return fmt.Errorf("record %d: create incomplete version: %w", i, verErr)
			}
			if err := store.saveRecordVersion(primaryKey, version, nil); err != nil {
				return fmt.Errorf("record %d: save version: %w", i, err)
			}
			stored.Version = version
		}

		// Index updates via cached maintainers — no per-record map lookups.
		stored.PrimaryKey = primaryKey
		stored.RecordType = recordType
		stored.Record = record
		stored.Store = store
		for _, cm := range maintainers {
			if err := cm.maintainer.Update(nil, &stored); err != nil {
				return fmt.Errorf("record %d: index %q: %w", i, cm.index.Name, err)
			}
		}
	}

	// Batch record count
	if countFDBKey != nil {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(records)))
		tx.AddBytes(countFDBKey, buf[:])
	}

	return nil
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
