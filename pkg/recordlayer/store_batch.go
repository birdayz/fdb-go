// Portions derived from FoundationDB Record Layer (FDBStoredRecord.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"encoding/binary"
	"fmt"
	"maps"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// unsplitSuffix is the pre-computed tuple for the unsplit record suffix (0).
// Used by PackConcatWithPrefix to avoid appendToTuple allocation.
var unsplitSuffix = tuple.Tuple{unsplitRecord}

// BatchSaveOptions controls transaction-local batch execution, not stored layout.
type BatchSaveOptions struct {
	// CoalesceIndexWriteConflicts adds, once the whole batch has saved, a write
	// conflict over the primary and the secondary subspace of each index whose
	// maintainer this batch ran: an index that applies to a saved record's type
	// or to the type of a record it overwrote (a multi-type index on either,
	// and every universal index such as COUNT or VERSION), and that is not
	// disabled or write-only with a pending-write queue. Indexes of other
	// record types get no added range, so readers of only those indexes see no
	// conflict from the batch, as without the option.
	//
	// The ranges are added on top of the batch's normal conflicts and suppress
	// none of them: each write keeps the conflict it has without the option (a
	// versionstamped VERSION entry gets its own from the commit proxy). Every
	// conflict of the batch therefore remains, and a client that coalesces overlapping
	// ranges can ship fewer, wider ones; the live approximate transaction size
	// need not decrease. Readers that also write and read anywhere in a covered
	// index (not only the keys the batch changed) may see additional conflicts.
	// No record, header, record-count, index-state, uniqueness-violation,
	// sliding-window, or pending-queue range is widened, and a failed batch adds
	// none.
	CoalesceIndexWriteConflicts bool

	// skipTextPrefetch turns off the TEXT bunch prefetch (tests compare
	// against it).
	skipTextPrefetch bool
}

// SaveRecordBatch saves multiple records with pipelined existence checks.
//
// Instead of N sequential blocking FDB reads (one per record in SaveRecord),
// this method issues all existing-record ranges up front as futures, then
// collects them in one batch. This turns N round trips into ~1 round trip
// for the existence checks, significantly improving throughput for batch inserts.
//
// Semantically equivalent to calling SaveRecord N times. All records are
// saved in the current transaction.
//
// TEXT indexes: before the first record is applied, the batch reads, all
// together, the BunchedMap bunches its records' TEXT index updates will read
// (the new entries' and an update's removed ones, one snapshot read per token
// map, plus the tokenizer version of each updated record), into the
// transaction's read-your-writes cache. The records are then applied one
// after another as SaveRecord applies them, and their bunch reads, served
// from that cache merged with the earlier records' writes, return what they
// return without it. The commit is the commit of N SaveRecord calls, its
// mutations and read and write conflict ranges included; the bunch reads cost
// one round trip instead of one per record (store_batch_text_prefetch.go).
func (store *FDBRecordStore) SaveRecordBatch(
	records []proto.Message,
) ([]*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordBatchWithOptions(records, BatchSaveOptions{})
}

// SaveRecordBatchWithOptions saves as SaveRecordBatch does, then applies the
// requested conflict widening only after every normal save has succeeded.
func (store *FDBRecordStore) SaveRecordBatchWithOptions(
	records []proto.Message,
	options BatchSaveOptions,
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
	// Every record is checked before the first is written, so a bad one late
	// in the batch leaves none of it in the transaction.
	for i, record := range records {
		if record == nil {
			continue
		}
		// The type by descriptor first, as each save resolves it ahead of the record's contents.
		rt, err := store.metaData.GetRecordTypeForDescriptor(record.ProtoReflect().Descriptor())
		if err != nil {
			return nil, err
		}
		if err := rt.checkUTF8Strings(record); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, &RecordSerializationError{Cause: err})
		}
	}
	if store.omitUnsplitRecordSuffix() {
		if err := store.checkLegacyBatchCollation(records); err != nil {
			return nil, err
		}
		results := make([]*FDBStoredRecord[proto.Message], len(records))
		written := options.indexWriteSet()
		for i, rec := range records {
			saved, err := store.saveRecordInternal(rec, RecordExistenceCheckNone, nil, VersionstampSaveBehaviorDefault, false, nil, nil, written)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			results[i] = saved
		}
		if err := store.addBatchIndexWriteConflicts(written); err != nil {
			return nil, err
		}
		return results, nil
	}

	tx := store.context.Transaction()
	recordsSubspace := store.recordsSubspace
	splitEnabled := store.metaData.IsSplitLongRecords()

	// --- Phase 1: Extract PKs and issue all record ranges (non-blocking) ---
	type pendingRecord struct {
		record     proto.Message
		write      proto.Message // what the save serializes (asJavaForSave)
		recordType *RecordType
		primaryKey tuple.Tuple
		unsplitKey fdb.Key // pre-computed, reused for save
		load       pendingStoreRecordLoad
		oldExists  bool
		oldSize    sizeInfo
		oldType    *RecordType
		oldRecord  proto.Message
		oldWire    *recordWire
		oldVersion *FDBRecordVersion
		version    *FDBRecordVersion
		previous   int // previous occurrence of this primary key, or -1
	}

	pending := make([]pendingRecord, len(records))
	lastForKey := make(map[string]int, len(records))

	for i, record := range records {
		if record == nil {
			return nil, fmt.Errorf("record %d is nil", i)
		}

		descriptor := record.ProtoReflect().Descriptor()
		recordTypeName := string(descriptor.Name())
		recordType, err := store.metaData.GetRecordTypeForDescriptor(descriptor)
		if err != nil {
			return nil, err
		}
		savedVersion, err := store.recordVersionForSave(nil, VersionstampSaveBehaviorDefault)
		if err != nil {
			return nil, fmt.Errorf("record %d: choose version: %w", i, err)
		}
		if recordType.PrimaryKey == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", recordTypeName)}
		}
		if err := store.checkPrimaryKeyCollation(recordType); err != nil {
			return nil, err
		}
		// As saveRecordInternal: the record as every later load reads it, and
		// the message the save serializes.
		var write proto.Message
		record, write = recordType.asJavaForSave(record)

		// Record type supplied so a record-type-prefixed primary key can read
		// its leading component off the type, as Java's saveTypedRecord does.
		keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, &FDBStoredRecord[proto.Message]{
			RecordType: recordType,
			Record:     record,
			Version:    savedVersion,
		}, record)
		if err != nil {
			return nil, fmt.Errorf("record %d: extract primary key: %w", i, err)
		}

		primaryKey := make(tuple.Tuple, len(keyValues))
		for j, v := range keyValues {
			primaryKey[j] = v
		}
		if incomplete, _ := primaryKey.HasIncompleteVersionstamp(); incomplete {
			return nil, &IncompleteVersionstampError{Context: fmt.Sprintf("record %d primary key", i)}
		}

		// Issue the whole record range before waiting on any batch member.
		unsplitKeyTuple := appendToTuple(primaryKey, unsplitRecord)
		unsplitKey := fdb.Key(recordsSubspace.Pack(unsplitKeyTuple))
		previous := -1
		if last, ok := lastForKey[string(unsplitKey)]; ok {
			previous = last
		}
		lastForKey[string(unsplitKey)] = i
		// A repeated key replaces the earlier batch entry, so it reads nothing.
		var load pendingStoreRecordLoad
		if previous < 0 {
			load = store.startRecordLoad(tx, primaryKey)
		}

		pending[i] = pendingRecord{
			record:     record,
			write:      write,
			recordType: recordType,
			primaryKey: primaryKey,
			version:    savedVersion,
			unsplitKey: unsplitKey,
			load:       load,
			previous:   previous,
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
	// By now FDB has pipelined all N ranges over the wire. Collecting them
	// costs ~1 round trip total instead of N sequential round trips.

	results := make([]*FDBStoredRecord[proto.Message], len(records))
	var insertCount int64

	// Lazy-load store state before acquiring lock (Build() path, matches Java).
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return nil, fmt.Errorf("load store state: %w", err)
	}
	// Hold stateMu.RLock for the entire batch to avoid per-record lock/unlock.
	// The update lock is the same for every record, so it is validated once, in
	// the first record's turn, after that record's existing value is decoded:
	// each record saves as SaveRecord saves it (Java's saveTypedRecord loads the
	// old record, then validates the lock), so a batch whose first record the
	// store cannot read fails that decode, not the lock.
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	store.indexStateView.maintenance.RLock()
	defer store.indexStateView.maintenance.RUnlock()
	lockChecked := false
	written := options.indexWriteSet()

	for i := range pending {
		p := &pending[i]

		// A repeated key replaces the earlier batch entry, not the initial image.
		if p.previous >= 0 {
			p.oldExists, p.oldType = true, pending[p.previous].recordType
			if err := store.checkRecordWriteCollationLocked(false, p.recordType, p.oldType); err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			continue
		}
		var oldsizeInfo sizeInfo
		oldValue, oldVersion, err := p.load.raw.finish(&oldsizeInfo)
		if err != nil {
			return nil, fmt.Errorf("record %d: existing record load: %w", i, err)
		}
		oldRecordExists := oldValue != nil
		// Each record saves as SaveRecord saves it: the existing record is
		// read through the serializer first (Java's loadExistingRecord), so
		// one the store cannot read fails the batch and is never overwritten.
		var oldRT *RecordType
		var oldMsg proto.Message
		var oldWire *recordWire
		if oldRecordExists {
			oldRT, oldMsg, oldWire, err = store.deserializeAndDiscover(oldValue)
			if err != nil {
				return nil, &RecordDeserializationError{PrimaryKey: p.primaryKey, Cause: err}
			}
		}
		if oldRecordExists {
			p.oldVersion, err = p.load.finishVersion(oldVersion)
			if err != nil {
				return nil, fmt.Errorf("record %d: load old record version: %w", i, err)
			}
		}
		if !lockChecked {
			if err := store.validateRecordUpdateAllowedLocked(); err != nil {
				return nil, err
			}
			lockChecked = true
		}

		if err := store.checkRecordWriteCollationLocked(!oldRecordExists, p.recordType, oldRT); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		p.oldExists, p.oldSize = oldRecordExists, oldsizeInfo
		p.oldType, p.oldRecord, p.oldWire = oldRT, oldMsg, oldWire
	}

	// The bunch reads of the batch's TEXT index updates, issued together and
	// drained into the transaction's cache before the first record is applied
	// (store_batch_text_prefetch.go).
	if !options.skipTextPrefetch {
		news := make([]*FDBStoredRecord[proto.Message], len(pending))
		olds := make([]*FDBStoredRecord[proto.Message], len(pending))
		for i := range pending {
			p := &pending[i]
			news[i] = &FDBStoredRecord[proto.Message]{PrimaryKey: p.primaryKey, RecordType: p.recordType, Record: p.record, Store: store}
			if p.previous < 0 && p.oldExists {
				olds[i] = &FDBStoredRecord[proto.Message]{PrimaryKey: p.primaryKey, RecordType: p.oldType, Record: p.oldRecord, wire: p.oldWire, Store: store}
			}
		}
		store.prefetchBatchTextBunches(news, olds)
	}

	// No record mutation precedes the whole batch's collation preflight.
	for i := range pending {
		p := &pending[i]
		oldRecordExists, oldsizeInfo := p.oldExists, p.oldSize
		oldRT, oldMsg, oldWire := p.oldType, p.oldRecord, p.oldWire
		oldVersion := p.oldVersion
		if p.previous >= 0 {
			old := results[p.previous]
			oldRT, oldMsg, oldWire, oldVersion = old.RecordType, old.Record, old.wire, old.Version
			oldsizeInfo = sizeInfo{
				KeyCount: old.KeyCount, KeySize: old.KeySize, ValueSize: old.ValueSize, IsSplit: old.Split,
				VersionedInline: old.VersionedInline,
			}
		}

		// Serialize
		data, err := serializeUnionOver(p.write, p.recordType, priorRecordInner(oldRT, oldWire, p.recordType))
		if err != nil {
			return nil, &RecordSerializationError{Cause: err}
		}
		storedBytes, err := store.writeStoredRecord(data, p.recordType, p.primaryKey)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}

		// Save — fast path for unsplit new records (most common in batch ingest)
		var newsizeInfo sizeInfo
		if !oldRecordExists && len(storedBytes) <= splitRecordSize {
			// Direct Set using pre-computed key — avoids appendToTuple + Pack in saveWithSplit
			tx.SetBytes(p.unsplitKey, storedBytes)
			newsizeInfo.KeyCount = 1
			newsizeInfo.KeySize = len(p.unsplitKey)
			newsizeInfo.ValueSize = len(storedBytes)
		} else {
			var oldsizeInfoPtr *sizeInfo
			if oldRecordExists {
				oldsizeInfoPtr = &oldsizeInfo
			}
			if err := saveWithSplit(store.context, tx, recordsSubspace, p.primaryKey, storedBytes,
				splitEnabled, false, oldsizeInfoPtr, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save: %w", i, err)
			}
		}

		// Save version if versioning is enabled. One SET_VERSIONSTAMPED_VALUE
		// mutation per record — no round trip, just added to the write set.
		savedVersion := p.version
		if savedVersion != nil {
			if err := store.saveRecordVersion(p.primaryKey, savedVersion, &newsizeInfo); err != nil {
				return nil, fmt.Errorf("record %d: save version: %w", i, err)
			}
		}

		// Record count
		stored := &FDBStoredRecord[proto.Message]{
			PrimaryKey: p.primaryKey,
			RecordType: p.recordType,
			Record:     p.record,
			// Its map entries are indexed in the order it was written in.
			wire:            newRecordWire(p.recordType, unionInner(data, p.recordType.unionFieldNumber)),
			Version:         savedVersion,
			Store:           store,
			KeyCount:        newsizeInfo.KeyCount,
			KeySize:         newsizeInfo.KeySize,
			ValueSize:       newsizeInfo.ValueSize,
			Split:           newsizeInfo.IsSplit,
			VersionedInline: newsizeInfo.VersionedInline,
		}
		if !oldRecordExists {
			if countFDBKey != nil {
				// Batched: just count inserts, single ADD at end
				insertCount++
			} else if countKey != nil {
				// Grouped count or non-empty key: per-record ADD
				if err := store.addRecordCount(stored, littleEndianInt64One); err != nil {
					return nil, fmt.Errorf("record %d: record count: %w", i, err)
				}
			}
		}

		// Secondary indexes
		var oldRecord *FDBStoredRecord[proto.Message]
		if oldRecordExists {
			oldRecord = &FDBStoredRecord[proto.Message]{
				PrimaryKey: p.primaryKey,
				RecordType: oldRT,
				Record:     oldMsg,
				wire:       oldWire,
				Store:      store,
				Version:    oldVersion,
			}
		}
		if err := store.updateSecondaryIndexesLocked(oldRecord, stored, written); err != nil {
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

	if err := store.addBatchIndexWriteConflicts(written); err != nil {
		return nil, err
	}
	return results, nil
}

// indexWriteSet collects, by name, the indexes whose maintainer a save ran
// (readable or write-only without a queue). A nil set collects nothing.
type indexWriteSet map[string]*Index

func (s indexWriteSet) add(index *Index) {
	if s != nil {
		s[index.Name] = index
	}
}

// indexWriteSet returns the set a batch with these options collects into: nil
// unless index write conflicts are coalesced.
func (options BatchSaveOptions) indexWriteSet() indexWriteSet {
	if !options.CoalesceIndexWriteConflicts {
		return nil
	}
	return indexWriteSet{}
}

// addBatchIndexWriteConflicts adds the coalesced conflicts of
// CoalesceIndexWriteConflicts over the indexes the batch updated, in name order.
// Every write keeps its own normal conflict range; these prefixes only add to
// those conflicts, never replace one.
func (store *FDBRecordStore) addBatchIndexWriteConflicts(written indexWriteSet) error {
	var ranges []fdb.KeyRange
	for _, name := range slices.Sorted(maps.Keys(written)) {
		index := written[name]
		for _, prefix := range [][]byte{store.indexSubspace(index).Bytes(), store.indexSecondarySubspace(index).Bytes()} {
			// PrefixRange also covers an aggregate entry at the subspace key itself.
			r, err := fdb.PrefixRange(prefix)
			if err != nil {
				return fmt.Errorf("batch index %q write-conflict range: %w", name, err)
			}
			ranges = append(ranges, r)
		}
	}
	for _, r := range ranges {
		if err := store.context.Transaction().AddWriteConflictRange(r); err != nil {
			return fmt.Errorf("batch index write conflict: %w", err)
		}
	}
	return nil
}

// The sequential legacy fallback must refuse a later collated record before
// staging earlier saves, just as the pipelined batch preflights all its records.
func (store *FDBRecordStore) checkLegacyBatchCollation(records []proto.Message) error {
	if store.goOnlyCollation {
		return nil
	}
	previous := make(map[string]*RecordType, len(records))
	for i, record := range records {
		if record == nil {
			return fmt.Errorf("record %d is nil", i)
		}
		name := string(record.ProtoReflect().Descriptor().Name())
		rt := store.metaData.GetRecordType(name)
		if rt == nil {
			return unknownRecordTypeError(name)
		}
		if rt.PrimaryKey == nil {
			return &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", name)}
		}
		if err := store.checkPrimaryKeyCollation(rt); err != nil {
			return err
		}
		record, _ = rt.asJavaForSave(record)
		values, err := evaluateKeyFlat(rt.PrimaryKey, &FDBStoredRecord[proto.Message]{RecordType: rt, Record: record}, record)
		if err != nil {
			return err
		}
		key := make(tuple.Tuple, len(values))
		for j, value := range values {
			key[j] = value
		}
		packed := string(key.Pack())
		oldType := previous[packed]
		if oldType == nil {
			old, err := store.LoadRecord(key)
			if err != nil {
				return err
			}
			if old != nil {
				oldType = old.RecordType
			}
		}
		if err := store.checkRecordWriteCollation(oldType == nil, rt, oldType); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
		previous[packed] = rt
	}
	return nil
}
