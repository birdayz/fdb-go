package recordlayer

import (
	"fmt"
	"maps"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// RecordsSubspace returns the subspace where records are stored.
// Matches Java's FDBRecordStore.recordsSubspace().
func (store *FDBRecordStore) RecordsSubspace() subspace.Subspace {
	return store.subspace.Sub(RecordKey)
}

// IndexSubspace returns the subspace for a specific index's entries.
// Matches Java's FDBRecordStore.indexSubspace(index).
func (store *FDBRecordStore) IndexSubspace(index *Index) subspace.Subspace {
	return store.indexSubspace(index)
}

// IndexSecondarySubspace returns the secondary subspace for a specific index.
// Matches Java's FDBRecordStore.indexSecondarySubspace(index).
func (store *FDBRecordStore) IndexSecondarySubspace(index *Index) subspace.Subspace {
	return store.indexSecondarySubspace(index)
}

// GetReadableIndexes returns all indexes that are in READABLE or READABLE_UNIQUE_PENDING state.
// Matches Java's FDBRecordStoreBase.getReadableIndexes().
func (store *FDBRecordStore) GetReadableIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetAllIndexes() {
		if store.GetIndexState(idx.Name).IsScannable() {
			result = append(result, idx)
		}
	}
	return result
}

// GetEnabledIndexes returns all indexes that are NOT in DISABLED state.
// Matches Java's FDBRecordStoreBase.getEnabledIndexes().
func (store *FDBRecordStore) GetEnabledIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetAllIndexes() {
		if !store.GetIndexState(idx.Name).IsDisabled() {
			result = append(result, idx)
		}
	}
	return result
}

// GetAllIndexStates returns a map of all index names to their current states.
// Indexes without an explicit state entry default to READABLE.
// Matches Java's FDBRecordStore.getAllIndexStates().
func (store *FDBRecordStore) GetAllIndexStates() map[string]IndexState {
	result := make(map[string]IndexState)
	for name := range store.metaData.GetAllIndexes() {
		result[name] = store.GetIndexState(name)
	}
	return result
}

// RebuildAllIndexes rebuilds all indexes that are not in READABLE state.
// Matches Java's FDBRecordStore.rebuildAllIndexes().
func (store *FDBRecordStore) RebuildAllIndexes() error {
	for _, idx := range store.metaData.GetAllIndexes() {
		if store.GetIndexState(idx.Name) != IndexStateReadable {
			if err := store.RebuildIndex(idx); err != nil {
				return fmt.Errorf("rebuild all indexes: %w", err)
			}
		}
	}
	return nil
}

// VacuumReadableIndexesBuildData clears build artifacts (range sets, stamps,
// progress counters) for indexes that are already READABLE.
// Matches Java's FDBRecordStore.vacuumReadableIndexesBuildData().
func (store *FDBRecordStore) VacuumReadableIndexesBuildData() {
	tx := store.context.Transaction()
	for _, idx := range store.metaData.GetAllIndexes() {
		if store.GetIndexState(idx.Name) != IndexStateReadable {
			continue
		}
		// Clear build space [IndexBuildSpaceKey, indexSubspaceKey]
		buildSub := store.subspace.Sub(IndexBuildSpaceKey, idx.SubspaceTupleKey())
		tx.ClearRange(buildSub)

		// Clear range space [IndexRangeSpaceKey, indexSubspaceKey]
		rangeSub := store.subspace.Sub(IndexRangeSpaceKey, idx.SubspaceTupleKey())
		tx.ClearRange(rangeSub)
	}
}

// DeleteStore completely removes all data in a store subspace.
// Matches Java's FDBRecordStore.deleteStore(context, subspace).
func DeleteStore(ctx *FDBRecordContext, ss subspace.Subspace) error {
	pr, err := fdb.PrefixRange(ss.Bytes())
	if err != nil {
		return fmt.Errorf("delete store: prefix range: %w", err)
	}
	ctx.Transaction().ClearRange(pr)
	return nil
}

// FirstUnbuiltRange returns the first range of the index that hasn't been built yet.
// Returns nil, nil if the index is fully built.
// Matches Java's FDBRecordStore.firstUnbuiltRange(index).
func (store *FDBRecordStore) FirstUnbuiltRange(index *Index) (*RangeSetRange, error) {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	return rangeSet.FirstMissingRange(store.context.Transaction())
}

// IsCacheable returns whether the store state is marked as cacheable in the header.
// Matches Java's FDBRecordStore.getRecordStoreState().getStoreHeader().getCacheable().
func (store *FDBRecordStore) IsCacheable() bool {
	if store.storeHeader == nil {
		return false
	}
	return store.storeHeader.GetCacheable()
}

// GetStoreHeader returns a copy of the current store header proto.
// Matches Java's FDBRecordStore.getRecordStoreState().getStoreHeader().
func (store *FDBRecordStore) GetStoreHeader() *gen.DataStoreInfo {
	if store.storeHeader == nil {
		return nil
	}
	return proto.Clone(store.storeHeader).(*gen.DataStoreInfo)
}

// GetAllIndexStatesMap returns a copy of the raw index states map (non-READABLE only).
// For a complete map including defaulted READABLE states, use GetAllIndexStates().
func (store *FDBRecordStore) GetAllIndexStatesMap() map[string]IndexState {
	if store.indexStates == nil {
		return make(map[string]IndexState)
	}
	return maps.Clone(store.indexStates)
}

// OverrideLockSaveRecord saves a record even when the store is locked for record updates
// (FORBID_RECORD_UPDATE). This is used by the OnlineIndexer to write index maintenance
// records while the store is locked.
// Matches Java's FDBRecordStore.overrideLockSaveRecordAsync().
func (store *FDBRecordStore) OverrideLockSaveRecord(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	store.overrideLock = true
	defer func() { store.overrideLock = false }()
	return store.SaveRecordWithOptions(record, existenceCheck)
}

// GetRecordMetaData returns the metadata associated with this store.
// Matches Java's FDBRecordStore.getRecordMetaData().
func (store *FDBRecordStore) GetRecordMetaData() *RecordMetaData {
	return store.metaData
}

// GetContext returns the record context (transaction wrapper) for this store.
// Matches Java's FDBRecordStore.getRecordContext().
func (store *FDBRecordStore) GetContext() *FDBRecordContext {
	return store.context
}

// GetSubspace returns the FDB subspace for this store.
// Matches Java's FDBRecordStore.getSubspace().
func (store *FDBRecordStore) GetSubspace() subspace.Subspace {
	return store.subspace
}

// DryRunSaveRecord performs all save validation (existence checks, type checks,
// lock state) without actually writing data. Returns what the stored record
// would look like if saved, or an error if validation fails.
// Matches Java's FDBRecordStore.dryRunSaveRecordAsync().
func (store *FDBRecordStore) DryRunSaveRecord(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type: %s", recordTypeName)}
	}

	if recordType.PrimaryKey == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("no primary key defined for record type: %s", recordTypeName)}
	}

	pkTuples, err := recordType.PrimaryKey.Evaluate(nil, record)
	if err != nil {
		return nil, fmt.Errorf("evaluate primary key: %w", err)
	}
	if len(pkTuples) != 1 {
		return nil, &MetaDataError{Message: fmt.Sprintf("primary key must evaluate to exactly one tuple, got %d", len(pkTuples))}
	}
	keyValues := pkTuples[0]
	primaryKey := make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		primaryKey[i] = v
	}

	// Load existing record for existence/type validation.
	recordsSubspace := store.subspace.Sub(RecordKey)
	splitEnabled := store.metaData.IsSplitLongRecords()
	var oldSizeInfo SizeInfo
	oldValue, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		&oldSizeInfo,
	)
	if err != nil {
		return nil, fmt.Errorf("load existing record: %w", err)
	}
	oldRecordExists := oldValue != nil

	if existenceCheck.ErrorIfExists() && oldRecordExists {
		return nil, &RecordAlreadyExistsError{
			Message:    "record already exists",
			PrimaryKey: primaryKey,
		}
	}

	if existenceCheck.ErrorIfNotExists() && !oldRecordExists {
		return nil, &RecordDoesNotExistError{
			Message:    "record does not exist",
			PrimaryKey: primaryKey,
		}
	}

	if existenceCheck.ErrorIfTypeChanged() && oldRecordExists {
		_, oldMsg, deserErr := store.deserializeAndDiscover(oldValue)
		if deserErr != nil {
			return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
		}
		existingTypeName := string(oldMsg.ProtoReflect().Descriptor().Name())
		if existingTypeName != recordTypeName {
			return nil, &RecordTypeChangedError{
				Message:      "record type changed",
				PrimaryKey:   primaryKey,
				ActualType:   existingTypeName,
				ExpectedType: recordTypeName,
			}
		}
	}

	if err := store.validateRecordUpdateAllowed(); err != nil {
		return nil, err
	}

	// Compute what the record would look like.
	unionRecord, err := store.wrapInUnion(record, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap record in union: %w", err)
	}

	data, err := proto.Marshal(unionRecord)
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
	}

	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		Record:     record,
		RecordType: recordType,
		KeyCount:   1,
		KeySize:    len(store.subspace.Sub(RecordKey).Pack(primaryKey)),
		ValueSize:  len(data),
		Split:      splitEnabled && len(data) > SplitRecordSize,
	}, nil
}
