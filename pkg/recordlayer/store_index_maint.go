// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/internal/recordlayer/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer/metadatapb"
)

// GetIndexMaintainer returns the index maintainer for the given index.
// Matches Java's FDBRecordStore.getIndexMaintainer().
func (store *FDBRecordStore) GetIndexMaintainer(index *Index) (IndexMaintainer, error) {
	if index == nil {
		return nil, nil
	}
	return store.getIndexMaintainer(index)
}

// updateSecondaryIndexes updates all relevant indexes for a record change.
// oldRecord is nil for inserts, newRecord is nil for deletes.
//
// Cross-type overwrites partition indexes into old-only deletes, new-only inserts and common updates.
// written optionally collects indexes whose own data changed (Java FDBRecordStore.updateSecondaryIndexes).
func (store *FDBRecordStore) updateSecondaryIndexes(oldRecord, newRecord *FDBStoredRecord[proto.Message], written indexWriteSet) error {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return fmt.Errorf("load store state: %w", err)
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	store.indexStateView.maintenance.RLock()
	defer store.indexStateView.maintenance.RUnlock()
	return store.updateSecondaryIndexesLocked(oldRecord, newRecord, written, nil)
}

// updateSecondaryIndexesLocked is the lock-free variant for use when the caller
// already holds stateMu.RLock and the shared maintenance read lock (e.g. SaveRecordBatch).
func (store *FDBRecordStore) updateSecondaryIndexesLocked(oldRecord, newRecord *FDBStoredRecord[proto.Message], written indexWriteSet, tokens textTokenCache) error {
	if oldRecord == nil && newRecord == nil {
		return nil
	}
	// Validate liveness once for this record update, including the all-disabled
	// case. Per-index decisions below use cached transaction-visible state and
	// explicit conflicts; they must not allocate a GRV future for each index.
	if _, err := store.context.Transaction().GetReadVersion().Get(); err != nil {
		return err
	}

	// Fast path: same type (or one side nil) — no three-way split needed.
	sameType := oldRecord == nil || newRecord == nil || oldRecord.RecordType.Name == newRecord.RecordType.Name
	if sameType {
		var recordType *RecordType
		if newRecord != nil {
			recordType = newRecord.RecordType
		} else {
			recordType = oldRecord.RecordType
		}

		for _, index := range store.metaData.GetIndexesForRecordType(recordType.Name) {
			maintain, err := store.shouldMaintainIndex(index.Name)
			if err != nil {
				return err
			}
			if !maintain {
				continue
			}
			if err := store.updateOneIndex(index, oldRecord, newRecord, written, tokens); err != nil {
				return err
			}
		}
		for _, index := range store.metaData.GetUniversalIndexes() {
			maintain, err := store.shouldMaintainIndex(index.Name)
			if err != nil {
				return err
			}
			if !maintain {
				continue
			}
			if err := store.updateOneIndex(index, oldRecord, newRecord, written, tokens); err != nil {
				return err
			}
		}
		return nil
	}

	// Slow path: cross-type overwrite. Partition indexes into old-only,
	// new-only, and common sets. Matches Java's three-way split.
	oldIndexes, err := store.enabledIndexesForRecord(oldRecord)
	if err != nil {
		return err
	}
	newIndexes, err := store.enabledIndexesForRecord(newRecord)
	if err != nil {
		return err
	}

	commonIndexes, oldOnly, newOnly := partitionIndexes(oldIndexes, newIndexes)

	// Delete entries from indexes that only the old type had.
	for _, index := range oldOnly {
		if err := store.updateOneIndex(index, oldRecord, nil, written, tokens); err != nil {
			return err
		}
	}
	// Insert entries into indexes that only the new type has.
	for _, index := range newOnly {
		if err := store.updateOneIndex(index, nil, newRecord, written, tokens); err != nil {
			return err
		}
	}
	// Update entries in indexes shared by both types.
	for _, index := range commonIndexes {
		if err := store.updateOneIndex(index, oldRecord, newRecord, written, tokens); err != nil {
			return err
		}
	}

	return nil
}

// enabledIndexesForRecord returns all enabled indexes (type-specific + universal)
// that apply to the given record.
func (store *FDBRecordStore) enabledIndexesForRecord(rec *FDBStoredRecord[proto.Message]) ([]*Index, error) {
	var result []*Index
	for _, index := range store.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		maintain, err := store.shouldMaintainIndex(index.Name)
		if err != nil {
			return nil, err
		}
		if maintain {
			result = append(result, index)
		}
	}
	for _, index := range store.metaData.GetUniversalIndexes() {
		maintain, err := store.shouldMaintainIndex(index.Name)
		if err != nil {
			return nil, err
		}
		if maintain {
			result = append(result, index)
		}
	}
	return result, nil
}

// partitionIndexes splits old and new index lists into three disjoint sets:
// common (in both), oldOnly (only in old), newOnly (only in new).
// Identity is by index name.
func partitionIndexes(oldIndexes, newIndexes []*Index) (common, oldOnly, newOnly []*Index) {
	newSet := make(map[string]*Index, len(newIndexes))
	for _, idx := range newIndexes {
		newSet[idx.Name] = idx
	}

	oldSet := make(map[string]struct{}, len(oldIndexes))
	for _, idx := range oldIndexes {
		oldSet[idx.Name] = struct{}{}
		if _, ok := newSet[idx.Name]; ok {
			common = append(common, idx)
		} else {
			oldOnly = append(oldOnly, idx)
		}
	}

	for _, idx := range newIndexes {
		if _, ok := oldSet[idx.Name]; !ok {
			newOnly = append(newOnly, idx)
		}
	}
	return
}

// updateOneIndex dispatches maintenance by state; caller must hold stateMu.
// written records maintainers that ran, excluding queued or disabled indexes.
func (store *FDBRecordStore) updateOneIndex(index *Index, oldRecord, newRecord *FDBStoredRecord[proto.Message], written indexWriteSet, tokens textTokenCache) error {
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return err
	}
	state, err := store.indexStateView.read(store.context.Transaction(), index.Name)
	if err != nil {
		return err
	}
	if state.IsDisabled() {
		return nil
	}
	// Each update is recorded in the context's index-update set for its state
	// (Java's FDBRecordStore.updateSecondaryIndexes, #4289).
	if state.IsWriteOnlyWithQueue() {
		data, err := maintainer.SerializePendingWriteQueue(oldRecord, newRecord)
		if err != nil {
			return err
		}
		if err := store.enqueuePendingIndexWrite(index, &metadatapb.PendingWritesQueueEntry{Operation: metadatapb.PendingWritesQueueEntry_UPDATE.Enum(), Data: data}); err != nil {
			return err
		}
		store.context.addToSessionSet(WriteOnlyWithQueueIndexesUpdated, index.Name)
		return nil
	}
	writeOnly := state.IsWriteOnlyNoQueue()
	written.add(index)
	if text, ok := maintainer.(*textIndexMaintainer); ok {
		err = text.update(oldRecord, newRecord, tokens) // its UpdateWhileWriteOnly is Update
	} else if writeOnly {
		err = maintainer.UpdateWhileWriteOnly(oldRecord, newRecord)
	} else {
		err = maintainer.Update(oldRecord, newRecord)
	}
	if err != nil {
		return err
	}
	if writeOnly {
		store.context.addToSessionSet(WriteOnlyIndexesUpdated, index.Name)
	} else {
		store.context.addToSessionSet(ReadableIndexesUpdated, index.Name)
	}
	return nil
}

// shouldIndexRecordForIndex checks if a record matches the given index's record types.
// Returns true if the record's type has this index defined (either per-type or universal).
func (store *FDBRecordStore) shouldIndexRecordForIndex(rec *FDBStoredRecord[proto.Message], index *Index) bool {
	for _, idx := range store.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		if idx.Name == index.Name {
			return true
		}
	}
	for _, idx := range store.metaData.GetUniversalIndexes() {
		if idx.Name == index.Name {
			return true
		}
	}
	return false
}

// indexSubspace returns the FDB subspace for a specific index.
// Layout: [storeSubspace][IndexKey=2][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSubspace(Index).
func (store *FDBRecordStore) indexSubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
}

// indexSecondarySubspace returns the FDB subspace for a RANK index's secondary data (ranked sets).
// Layout: [storeSubspace][IndexSecondarySpaceKey=3][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSecondarySubspace(Index).
func (store *FDBRecordStore) indexSecondarySubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexSecondarySpaceKey, index.SubspaceTupleKey())
}

// indexSlidingWindowSubspace returns the FDB subspace holding a sliding-window
// index's bookkeeping (the per-partition entry list and its count/boundary
// metadata).
// Layout: [storeSubspace][IndexSlidingWindowSpaceKey=10][indexSubspaceTupleKey]
// Matches Java's FDBRecordStore.indexSlidingWindowSubspace(Index).
func (store *FDBRecordStore) indexSlidingWindowSubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexSlidingWindowSpaceKey, index.SubspaceTupleKey())
}

// loadRecordForIndexMaintenance loads a record by primary key on behalf of an
// index maintainer. Matches the use Java's SlidingWindowIndexMaintainer makes of
// state.store.loadRecordAsync: a window that evicts or promotes an entry must
// hand the RECORD to the wrapped maintainer, and the entry list stores only a
// primary key.
//
// A missing record returns (nil, nil) — the caller decides what that means. For
// the sliding window it is a counted anomaly rather than an error, matching
// Java's SW_EVICTED_RECORD_MISSING / SW_PROMOTED_RECORD_MISSING arms.
func (store *FDBRecordStore) loadRecordForIndexMaintenance(primaryKey tuple.Tuple) (*FDBStoredRecord[proto.Message], error) {
	return store.LoadRecord(primaryKey)
}

// getIndexMaintainer returns the appropriate IndexMaintainer for the given index.
// Maintainers are cached for the lifetime of the store (= one transaction),
// avoiding repeated allocation of maintainer + mutation objects.
// Matches Java's FDBRecordStore.getIndexMaintainer() dispatch.
func (store *FDBRecordStore) getIndexMaintainer(index *Index) (IndexMaintainer, error) {
	if err := store.checkIndexCollation(index); err != nil {
		return nil, err
	}
	return store.getIndexMaintainerForRawAccess(index)
}

// Range deletion uses stored tuple bytes without evaluating provider-specific keys.
func (store *FDBRecordStore) getIndexMaintainerForRawAccess(index *Index) (IndexMaintainer, error) {
	if cached, ok := store.maintainerCache.Load(index.Name); ok {
		return cached.(IndexMaintainer), nil
	}

	m, err := store.createIndexMaintainer(index)
	if err != nil {
		return nil, err
	}

	store.maintainerCache.Store(index.Name, m)
	return m, nil
}

func (store *FDBRecordStore) createIndexMaintainer(index *Index) (IndexMaintainer, error) {
	idxSubspace := store.indexSubspace(index)
	tx := store.context.Transaction()
	switch canonicalIndexType(index.Type) {
	case IndexTypeValue:
		// Spelled out rather than left to the default arm. It used to BE the
		// default arm, which is what made an unknown type indistinguishable from
		// a value index — see UnknownIndexTypeError.
		return newStandardIndexMaintainer(index, idxSubspace, tx, store), nil
	case IndexTypeCount:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countMutation{index: index}), nil
	case IndexTypeCountNotNull:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countNotNullMutation{index: index}), nil
	case IndexTypeCountUpdates:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &countUpdatesMutation{index: index}), nil
	case IndexTypeSum:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &sumMutation{index: index}), nil
	case IndexTypeMaxEverLong:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverLongMutation{index: index, isMax: true}), nil
	case IndexTypeMinEverLong:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverLongMutation{index: index, isMax: false}), nil
	case IndexTypeMaxEverTuple:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverTupleMutation{index: index, isMax: true}), nil
	case IndexTypeMinEverTuple:
		return newAtomicMutationIndexMaintainer(index, idxSubspace, tx, store, &minMaxEverTupleMutation{index: index, isMax: false}), nil
	case IndexTypeRank:
		secSubspace := store.indexSecondarySubspace(index)
		return newRankIndexMaintainer(index, idxSubspace, secSubspace, tx, store)
	case IndexTypeVersion:
		return newVersionIndexMaintainer(index, idxSubspace, tx, store.context, store), nil
	case IndexTypeMaxEverVersion:
		return newMaxEverVersionIndexMaintainer(index, idxSubspace, tx, store.context, store), nil
	case IndexTypePermutedMin:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, false)
	case IndexTypePermutedMax:
		secSubspace := store.indexSecondarySubspace(index)
		return newPermutedMinMaxIndexMaintainer(index, idxSubspace, secSubspace, tx, store, true)
	case IndexTypeBitmapValue:
		return newBitmapValueIndexMaintainer(index, idxSubspace, tx, store)
	case IndexTypeText:
		secSubspace := store.indexSecondarySubspace(index)
		return newTextIndexMaintainerWithTimer(index, idxSubspace, secSubspace, tx, store, store.context.Timer(),
			store.GetPipelineSize(PipelineOperationTextIndexUpdate))
	case IndexTypeTimeWindowLeaderboard:
		secSubspace := store.indexSecondarySubspace(index)
		return newTimeWindowLeaderboardIndexMaintainer(index, idxSubspace, secSubspace, tx, store)
	case IndexTypeMultidimensional:
		numDims := 2 // default; extracted from DimensionsKeyExpression at runtime
		if d := extractDimensionsExpression(index.RootExpression); d != nil {
			numDims = d.DimensionsSize
		}
		return newMultidimensionalIndexMaintainer(index, idxSubspace, store.indexSecondarySubspace(index), tx, store, numDims)
	default:
		factory, err := lookupIndexMaintainerFactory(index)
		if err != nil {
			return nil, err
		}
		if factory != nil {
			return store.newRegisteredIndexMaintainer(factory, index, idxSubspace, tx)
		}
		// FAIL CLOSED. Java's registry lookup throws MetaDataException here
		// (IndexMaintainerFactoryRegistryImpl.java:78-82) and so does this.
		//
		// The arm this replaces returned the standard (value-index) maintainer,
		// which meant an index type this build does not implement was maintained
		// as a value index — the wrong key bytes written into that index's own
		// subspace, silently, with the write succeeding. Refusing costs an
		// unmaintainable index; guessing costs the index's contents.
		return nil, &UnknownIndexTypeError{IndexName: index.Name, IndexType: index.Type}
	}
}

// newRegisteredIndexMaintainer builds a registered factory's maintainer and
// applies the sliding-window DECORATION. Java's
// IndexMaintainerFactoryRegistryImpl detects the row-number window predicate
// and wraps the factory with SlidingWindowIndexMaintainerFactory; the wrapping
// is transparent to everything downstream. Same here: the decorator implements
// IndexMaintainer and forwards the read path, so only the WRITE path changes —
// which records reach the delegate.
//
// Only IndexTypeVector is eligible (isSlidingWindowIndex), matching Java's
// SlidingWindowIndexMaintainerFactory.isSlidingWindowIndex. Go's
// IndexTypeVectorSPFresh is a Go-only extension (RFC-094) and is deliberately
// NOT decorated: keyspace 10's layout is the wire contract for Java's HNSW
// vector index, so a Go-only pairing would write bytes under prefix 10 that no
// Java engine can interpret. SPFresh also runs its own background rebalancer
// over the postings it owns, which an external evictor would be racing rather
// than cooperating with.
func (store *FDBRecordStore) newRegisteredIndexMaintainer(factory IndexMaintainerFactory, index *Index, idxSubspace subspace.Subspace, tx fdb.WritableTransaction) (IndexMaintainer, error) {
	m, err := factory.NewIndexMaintainer(IndexMaintainerState{
		Store:         store,
		Context:       store.context,
		Index:         index,
		IndexSubspace: idxSubspace,
		Transaction:   tx,
		Timer:         store.context.Timer(),
	})
	if err != nil {
		return nil, err
	}
	if !isSlidingWindowIndex(index) {
		return m, nil
	}
	return newSlidingWindowIndexMaintainer(
		index, m, store.indexSlidingWindowSubspace(index), tx, store, store.context.Timer())
}

func (store *FDBRecordStore) addUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple, existingKey tuple.Tuple) error {
	return store.AddUniquenessViolationWithExisting(index, indexKey, primaryKey, existingKey)
}

func (store *FDBRecordStore) removeUniquenessViolations(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	valueSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey()).Sub(indexKey...)
	store.context.Transaction().Clear(valueSubspace.Pack(primaryKey))
	// Java removes the survivor's violation when the second-last duplicate is
	// deleted. Two rows suffice to distinguish unresolved groups from a single
	// survivor; this serializable read also protects concurrent resolution.
	rows, err := store.context.Transaction().GetRange(valueSubspace, fdb.RangeOptions{Limit: 2}).GetSliceWithError()
	if err != nil {
		return err
	}
	if len(rows) == 1 {
		keyRange, err := fdb.PrefixRange(valueSubspace.Bytes())
		if err != nil {
			return err
		}
		store.context.ClearRange(keyRange)
	}
	return nil
}

func (store *FDBRecordStore) isKeyInIndexBuildRange(index *Index, primaryKey tuple.Tuple) (bool, error) {
	irs := NewIndexingRangeSet(store.subspace, index)
	return irs.ContainsKey(store.context.Transaction(), primaryKey.Pack())
}

// UniquenessViolation represents a recorded uniqueness violation entry.
// Matches Java's RecordIndexUniquenessViolation.
type UniquenessViolation struct {
	IndexName   string
	IndexKey    tuple.Tuple
	PrimaryKey  tuple.Tuple
	ExistingKey tuple.Tuple // Conflicting PK stored by the other side (may be nil)
}

// ScanUniquenessViolations returns all uniqueness violations recorded for the given index.
// Violations are stored in the IndexUniquenessViolationsKey (7) subspace.
// Matches Java's StandardIndexMaintainer.scanUniquenessViolations().
func (store *FDBRecordStore) ScanUniquenessViolations(index *Index) ([]UniquenessViolation, error) {
	if index == nil {
		return nil, fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	begin, end := violationSubspace.FDBRangeKeys()
	kr := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(kr, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan uniqueness violations for index %q: %w", index.Name, err)
	}

	var violations []UniquenessViolation
	for _, kv := range kvs {
		t, err := tuplefast.SubspaceUnpack(kv.Key, len(violationSubspace.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("unpack violation key: %w", err)
		}
		// Key format: [indexKey..., primaryKey...]
		colCount := index.RootExpression.ColumnSize()
		if len(t) > colCount {
			v := UniquenessViolation{
				IndexName:  index.Name,
				IndexKey:   tuple.Tuple(t[:colCount]),
				PrimaryKey: tuple.Tuple(t[colCount:]),
			}
			// Value contains the conflicting PK (matching Java's wire format).
			// Empty value means no cross-reference was stored.
			if len(kv.Value) > 0 {
				existingKey, err := tuplefast.Unpack(kv.Value)
				if err == nil {
					v.ExistingKey = existingKey
				}
			}
			violations = append(violations, v)
		}
	}
	return violations, nil
}

// ResolveUniquenessViolation removes a single uniqueness violation entry.
// Call this after manually resolving the conflict (e.g., deleting the duplicate record).
// This low-level utility removes only the named entry. Ordinary record deletion
// uses Java's removeUniquenessViolationsAsync algorithm, including survivor cleanup.
func (store *FDBRecordStore) ResolveUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	store.context.Transaction().Clear(violationSubspace.Sub(indexKey...).Pack(primaryKey))
	return nil
}

// AddUniquenessViolation records a uniqueness violation for the given index.
// Used during WRITE_ONLY index builds when a uniqueness conflict is detected.
// Stores empty bytes as the value (no cross-reference to conflicting PK).
// Use AddUniquenessViolationWithExisting to store the conflicting PK in the value.
func (store *FDBRecordStore) AddUniquenessViolation(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	return store.AddUniquenessViolationWithExisting(index, indexKey, primaryKey, nil)
}

// AddUniquenessViolationWithExisting records a uniqueness violation with the conflicting PK.
// Matches Java's StandardIndexMaintainer.addUniquenessViolation(valueKey, primaryKey, existingKey).
// When existingKey is non-nil, its packed bytes are stored as the FDB value.
// When existingKey is nil, empty bytes are stored.
func (store *FDBRecordStore) AddUniquenessViolationWithExisting(index *Index, indexKey tuple.Tuple, primaryKey tuple.Tuple, existingKey tuple.Tuple) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	// Unlike ordinary index entries, Java's uniquenessViolationKey appends the
	// full primary key, including components already present in the value key.
	key := violationSubspace.Sub(indexKey...).Pack(primaryKey)
	var value []byte
	if existingKey != nil {
		value = existingKey.Pack()
	}
	store.context.Transaction().Set(key, value)
	return nil
}

// GetIndexMaintenanceFilter is Java's FDBRecordStore.getIndexMaintenanceFilter.
func (store *FDBRecordStore) GetIndexMaintenanceFilter() IndexMaintenanceFilter {
	return store.indexMaintenanceFilter()
}

func (store *FDBRecordStore) indexMaintenanceFilter() IndexMaintenanceFilter {
	if store == nil || store.maintenanceFilter == nil {
		return IndexMaintenanceFilterNormal
	}
	return store.maintenanceFilter
}
