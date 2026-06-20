package recordlayer

import (
	"fmt"
	"maps"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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

// GetReadableUniversalIndexes returns universal indexes in READABLE state.
// Matches Java's FDBRecordStore.getReadableUniversalIndexes().
func (store *FDBRecordStore) GetReadableUniversalIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetUniversalIndexes() {
		if store.GetIndexState(idx.Name).IsScannable() {
			result = append(result, idx)
		}
	}
	return result
}

// GetEnabledUniversalIndexes returns universal indexes NOT in DISABLED state.
// Matches Java's FDBRecordStore.getEnabledUniversalIndexes().
func (store *FDBRecordStore) GetEnabledUniversalIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetUniversalIndexes() {
		if !store.GetIndexState(idx.Name).IsDisabled() {
			result = append(result, idx)
		}
	}
	return result
}

// SetFormatVersion updates the store's format version in the header.
// This is used during store migration to enable new features.
// Matches Java's FDBRecordStore.setFormatVersion().
func (store *FDBRecordStore) SetFormatVersion(version int32) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}
	store.storeHeader.FormatVersion = &version
	return store.writeStoreHeader(store.storeHeader)
}

// IndexStateSubspace returns the subspace where index states are stored.
// Matches Java's FDBRecordStore.indexStateSubspace().
func (store *FDBRecordStore) IndexStateSubspace() subspace.Subspace {
	return store.subspace.Sub(IndexStateSpaceKey)
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
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getRecordStoreState().getStoreHeader().getCacheable().
func (store *FDBRecordStore) IsCacheable() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader == nil {
		return false
	}
	return store.storeHeader.GetCacheable()
}

// GetStoreHeader returns a copy of the current store header proto.
// Goroutine-safe via stateMu (read lock).
// Matches Java's FDBRecordStore.getRecordStoreState().getStoreHeader().
func (store *FDBRecordStore) GetStoreHeader() *gen.DataStoreInfo {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader == nil {
		return nil
	}
	return proto.Clone(store.storeHeader).(*gen.DataStoreInfo)
}

// GetAllIndexStatesMap returns a copy of the raw index states map (non-READABLE only).
// Goroutine-safe via stateMu (read lock).
// For a complete map including defaulted READABLE states, use GetAllIndexStates().
func (store *FDBRecordStore) GetAllIndexStatesMap() map[string]IndexState {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.indexStates == nil {
		return make(map[string]IndexState)
	}
	return maps.Clone(store.indexStates)
}

// OverrideLockSaveRecord saves a record even when the store is locked for record updates
// (FORBID_RECORD_UPDATE). This is used by the OnlineIndexer to write index maintenance
// records while the store is locked.
// Goroutine-safe: uses parameter-based override instead of mutable field.
// Matches Java's FDBRecordStore.overrideLockSaveRecordAsync().
func (store *FDBRecordStore) OverrideLockSaveRecord(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	return store.saveRecordInternal(record, existenceCheck, true)
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
	if record == nil {
		return nil, fmt.Errorf("cannot save nil record")
	}
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
	var oldsizeInfo sizeInfo
	oldValue, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		store.omitUnsplitRecordSuffix(),
		&oldsizeInfo,
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

	// Serialize directly into union wire format (no UnionDescriptor allocation)
	data, err := serializeUnion(record, recordType)
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
	}

	keyCount := 1
	keySize := len(recordsSubspace.Pack(primaryKey))
	valueSize := len(data)
	isSplit := splitEnabled && len(data) > splitRecordSize

	// Include version key/value bytes in dry-run metrics.
	// Matches Java's dryRunWriteVersionSizeInfo().
	if store.metaData.IsStoreRecordVersions() {
		versionKey := store.versionKey(primaryKey)
		keyCount++
		keySize += len(versionKey)
		// Value is a tuple-packed Versionstamp: VersionBytes (12) + 1 tuple type byte = 13.
		// Matches Java's SizeInfo.add(keyBytes, version): VERSION_LENGTH + 1.
		valueSize += VersionBytes + 1
	}

	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		Record:     record,
		RecordType: recordType,
		KeyCount:   keyCount,
		KeySize:    keySize,
		ValueSize:  valueSize,
		Split:      isSplit,
	}, nil
}

// DryRunDeleteRecord checks whether a record with the given primary key exists
// and could be deleted, without actually deleting it.
// Returns true if the record exists (and would be deleted), false if not found.
// Note: does NOT check store lock state, matching Java's dryRunDeleteRecordAsync
// which only loads the record without calling validateRecordUpdateAllowed.
// Matches Java's FDBRecordStore.dryRunDeleteRecordAsync().
func (store *FDBRecordStore) DryRunDeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	recordsSubspace := store.subspace.Sub(RecordKey)
	splitEnabled := store.metaData.IsSplitLongRecords()
	var sizeInfo sizeInfo
	value, err := loadWithSplit(
		store.context.Transaction(),
		recordsSubspace,
		primaryKey,
		splitEnabled,
		store.omitUnsplitRecordSuffix(),
		&sizeInfo,
	)
	if err != nil {
		return false, fmt.Errorf("dry run delete record: %w", err)
	}
	if value == nil {
		return false, nil
	}
	return true, nil
}

// IsIndexReadableUniquePending returns true if the index is in READABLE_UNIQUE_PENDING state.
// This state means the unique index is fully indexed but may have duplicate entries.
// Matches Java's FDBRecordStoreBase.isIndexReadableUniquePending().
func (store *FDBRecordStore) IsIndexReadableUniquePending(indexName string) bool {
	return store.GetIndexState(indexName) == IndexStateReadableUniquePending
}

// GetWriteOnlyIndexes returns all indexes that are in WRITE_ONLY state.
// These are indexes currently being built by an OnlineIndexer.
// Matches Java's FDBRecordStoreBase.getWriteOnlyIndexes() (derived from getAllIndexStates).
func (store *FDBRecordStore) GetWriteOnlyIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetAllIndexes() {
		if store.GetIndexState(idx.Name) == IndexStateWriteOnly {
			result = append(result, idx)
		}
	}
	return result
}

// GetDisabledIndexes returns all indexes that are in DISABLED state.
// These indexes are not maintained or readable.
// Matches Java's FDBRecordStoreBase.getDisabledIndexes() (derived from getAllIndexStates).
func (store *FDBRecordStore) GetDisabledIndexes() []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetAllIndexes() {
		if store.GetIndexState(idx.Name) == IndexStateDisabled {
			result = append(result, idx)
		}
	}
	return result
}

// GetIndexesToBuildSince returns all indexes that need to be built because they were
// added or modified since the given metadata version.
// Matches Java's FDBRecordStore.getIndexesToBuildSince(version).
func (store *FDBRecordStore) GetIndexesToBuildSince(version int) []*Index {
	var result []*Index
	for _, idx := range store.metaData.GetAllIndexes() {
		if idx.LastModifiedVersion > version {
			result = append(result, idx)
		}
	}
	return result
}

// ResolveUniquenessViolationByDeletion resolves uniqueness violations for a specific
// index value key by deleting all records that violate uniqueness, except for the
// record with remainPrimaryKey (if non-nil). If remainPrimaryKey is nil, all violating
// records are deleted.
// Matches Java's FDBRecordStore.resolveUniquenessViolation(index, valueKey, remainPrimaryKey).
func (store *FDBRecordStore) ResolveUniquenessViolationByDeletion(
	index *Index,
	valueKey tuple.Tuple,
	remainPrimaryKey tuple.Tuple,
) error {
	violations, err := store.ScanUniquenessViolationsForValue(index, valueKey)
	if err != nil {
		return fmt.Errorf("resolve uniqueness violation: %w", err)
	}
	for _, v := range violations {
		if remainPrimaryKey != nil && tuplesEqual(v.PrimaryKey, remainPrimaryKey) {
			continue
		}
		if _, err := store.DeleteRecord(v.PrimaryKey); err != nil {
			return fmt.Errorf("resolve uniqueness violation: delete pk=%v: %w", v.PrimaryKey, err)
		}
	}
	return nil
}

// ScanUniquenessViolationsForValue returns uniqueness violations for a specific index
// value key. This filters violations to only those matching the given value key.
// Matches Java's StandardIndexMaintainer.scanUniquenessViolations(index, valueKey).
func (store *FDBRecordStore) ScanUniquenessViolationsForValue(
	index *Index,
	valueKey tuple.Tuple,
) ([]UniquenessViolation, error) {
	violationSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	// The key format is [valueKey..., primaryKey...]. To scan for a specific valueKey,
	// use a prefix range on the valueKey portion.
	prefixKey := violationSubspace.Pack(valueKey)
	pr, err := fdb.PrefixRange(prefixKey)
	if err != nil {
		return nil, fmt.Errorf("scan violations for value: prefix range: %w", err)
	}

	kvs, err := store.context.Transaction().GetRange(pr, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan violations for value on index %q: %w", index.Name, err)
	}

	colCount := index.RootExpression.ColumnSize()
	var violations []UniquenessViolation
	for _, kv := range kvs {
		t, err := fastSubspaceUnpack(kv.Key, len(violationSubspace.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("unpack violation key: %w", err)
		}
		if len(t) > colCount {
			v := UniquenessViolation{
				IndexName:  index.Name,
				IndexKey:   tuple.Tuple(t[:colCount]),
				PrimaryKey: tuple.Tuple(t[colCount:]),
			}
			if len(kv.Value) > 0 {
				existingKey, err := fastUnpack(kv.Value)
				if err == nil {
					v.ExistingKey = existingKey
				}
			}
			violations = append(violations, v)
		}
	}
	return violations, nil
}

// EstimateRecordsSizeInRange returns the estimated size in bytes of records within
// the given tuple range. Uses FDB's native GetEstimatedRangeSizeBytes.
// Matches Java's FDBRecordStore.estimateRecordsSizeAsync(TupleRange).
func (store *FDBRecordStore) EstimateRecordsSizeInRange(tupleRange TupleRange) (int64, error) {
	recordsSubspace := store.subspace.Sub(RecordKey)
	fdbRange := tupleRange.ToFDBRange(recordsSubspace)
	return store.context.Transaction().GetEstimatedRangeSizeBytes(fdbRange).Get()
}

// EstimateIndexSize returns the estimated size in bytes of a specific index.
// Uses FDB's native GetEstimatedRangeSizeBytes on the index's subspace.
func (store *FDBRecordStore) EstimateIndexSize(index *Index) (int64, error) {
	indexSub := store.indexSubspace(index)
	return store.context.Transaction().GetEstimatedRangeSizeBytes(indexSub).Get()
}

// GetRangeSplitPoints returns split points that divide the store's records subspace
// into approximately equal-sized chunks of the given byte size. Useful for
// parallelizing scans across multiple transactions.
// Uses FDB's native GetRangeSplitPoints which is efficient for large datasets.
func (store *FDBRecordStore) GetRangeSplitPoints(chunkSize int64) ([]fdb.Key, error) {
	recordsSub := store.subspace.Sub(RecordKey)
	return store.context.Transaction().GetRangeSplitPoints(recordsSub, chunkSize).Get()
}

// ScanRecordKeys scans only the primary keys of records without deserializing
// record data. This is significantly faster than ScanRecords when you only need
// the keys (avoids protobuf deserialization overhead).
// Split records (multiple KV entries per PK) are automatically deduplicated.
// Matches Java's FDBRecordStore.scanRecordKeys().
func (store *FDBRecordStore) ScanRecordKeys(
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[tuple.Tuple] {
	return &recordKeyCursor{
		store:                   store,
		continuation:            continuation,
		scanProperties:          scanProperties,
		startTime:               time.Now(),
		omitUnsplitRecordSuffix: store.omitUnsplitRecordSuffix(),
	}
}
