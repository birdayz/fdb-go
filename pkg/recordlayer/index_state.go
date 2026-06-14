package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// IndexState represents the state of a secondary index.
// Matches Java's com.apple.foundationdb.record.IndexState.
type IndexState int

const (
	// IndexStateReadable means the index is fully built and can be read and written.
	IndexStateReadable IndexState = 0
	// IndexStateWriteOnly means the index is being built. Written to on record changes
	// but not yet safe for queries.
	IndexStateWriteOnly IndexState = 1
	// IndexStateDisabled means the index is not maintained or readable.
	IndexStateDisabled IndexState = 2
	// IndexStateReadableUniquePending means the unique index is fully indexed but may
	// have duplicate entries. Safe to query if uniqueness is not assumed.
	IndexStateReadableUniquePending IndexState = 3
)

// IsScannable returns true if this state allows index scans.
// Matches Java's IndexState.isScannable() — READABLE or READABLE_UNIQUE_PENDING.
func (s IndexState) IsScannable() bool {
	return s == IndexStateReadable || s == IndexStateReadableUniquePending
}

// IsWriteOnly returns true if this index is in WRITE_ONLY state.
func (s IndexState) IsWriteOnly() bool {
	return s == IndexStateWriteOnly
}

// IsDisabled returns true if this index is in DISABLED state.
func (s IndexState) IsDisabled() bool {
	return s == IndexStateDisabled
}

func (s IndexState) String() string {
	switch s {
	case IndexStateReadable:
		return "READABLE"
	case IndexStateWriteOnly:
		return "WRITE_ONLY"
	case IndexStateDisabled:
		return "DISABLED"
	case IndexStateReadableUniquePending:
		return "READABLE_UNIQUE_PENDING"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

// indexStateFromCode converts a numeric code to IndexState.
// Matches Java's IndexState.fromCode().
func indexStateFromCode(code int64) (IndexState, error) {
	switch IndexState(code) {
	case IndexStateReadable, IndexStateWriteOnly, IndexStateDisabled, IndexStateReadableUniquePending:
		return IndexState(code), nil
	default:
		return IndexStateReadable, fmt.Errorf("unknown index state code: %d", code)
	}
}

// GetIndexState returns the state of the given index. Returns READABLE if no
// explicit state is stored (matching Java's default behavior).
// Goroutine-safe via stateMu (read lock).
func (store *FDBRecordStore) GetIndexState(indexName string) IndexState {
	store.ensureStoreStateLoaded()
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.getIndexStateLocked(indexName)
}

// getIndexStateLocked returns index state without acquiring stateMu.
// Caller must hold stateMu (read or write).
// Caller must call ensureStoreStateLoaded() before acquiring stateMu
// to guarantee indexStates is populated.
func (store *FDBRecordStore) getIndexStateLocked(indexName string) IndexState {
	if store.indexStates == nil {
		// This should not happen if callers properly call ensureStoreStateLoaded().
		// Defensive fallback: assume all indexes readable.
		return IndexStateReadable
	}
	state, ok := store.indexStates[indexName]
	if !ok {
		return IndexStateReadable
	}
	return state
}

// IsIndexReadable returns true if the index is in READABLE state.
func (store *FDBRecordStore) IsIndexReadable(indexName string) bool {
	return store.GetIndexState(indexName) == IndexStateReadable
}

// IsIndexWriteOnly returns true if the index is in WRITE_ONLY state.
func (store *FDBRecordStore) IsIndexWriteOnly(indexName string) bool {
	return store.GetIndexState(indexName) == IndexStateWriteOnly
}

// IsIndexDisabled returns true if the index is in DISABLED state.
func (store *FDBRecordStore) IsIndexDisabled(indexName string) bool {
	return store.GetIndexState(indexName) == IndexStateDisabled
}

// IsIndexScannable returns true if the index can be scanned (READABLE or READABLE_UNIQUE_PENDING).
func (store *FDBRecordStore) IsIndexScannable(indexName string) bool {
	return store.GetIndexState(indexName).IsScannable()
}

// MarkIndexReadable transitions an index to READABLE state.
// Returns true if the state was changed, false if already READABLE.
// Returns an error if the index is not fully built or if a unique index has violations.
// Matches Java's FDBRecordStore.markIndexReadable(index, allowUniquePending=false).
func (store *FDBRecordStore) MarkIndexReadable(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	current := store.GetIndexState(indexName)
	if current == IndexStateReadable {
		return false, nil
	}

	// Verify the index is fully built before marking readable.
	// Matches Java's checkAndUpdateBuiltIndexState -> firstUnbuiltRange check.
	if err := store.checkIndexBuilt(idx); err != nil {
		return false, err
	}

	// For unique indexes, check for uniqueness violations.
	// Matches Java's markIndexReadable(index, allowUniquePending=false) which throws
	// RecordIndexUniquenessViolation if violations exist.
	if idx.IsUnique() {
		violations, err := store.ScanUniquenessViolations(idx)
		if err != nil {
			return false, fmt.Errorf("check uniqueness violations for index %q: %w", indexName, err)
		}
		if len(violations) > 0 {
			return false, &RecordIndexUniquenessViolationError{
				IndexName:  indexName,
				IndexKey:   violations[0].IndexKey,
				PrimaryKey: violations[0].PrimaryKey,
			}
		}
	}

	store.setIndexState(indexName, IndexStateReadable)
	store.clearReadableIndexBuildData(idx)
	return true, nil
}

// MarkIndexReadableOrUniquePending transitions a unique index to READABLE if it has
// no uniqueness violations, or to READABLE_UNIQUE_PENDING if violations exist.
// For non-unique indexes, always transitions to READABLE.
// Returns true if the state was changed.
// Returns an error if the index is not fully built.
// Matches Java's FDBRecordStore.markIndexReadableOrUniquePending().
func (store *FDBRecordStore) MarkIndexReadableOrUniquePending(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}

	current := store.GetIndexState(indexName)
	if current == IndexStateReadable {
		return false, nil
	}

	// Verify the index is fully built before marking readable.
	// Matches Java's checkAndUpdateBuiltIndexState -> firstUnbuiltRange check.
	if err := store.checkIndexBuilt(idx); err != nil {
		return false, err
	}

	targetState := IndexStateReadable
	if idx.IsUnique() {
		violations, err := store.ScanUniquenessViolations(idx)
		if err != nil {
			return false, fmt.Errorf("check uniqueness violations for index %q: %w", indexName, err)
		}
		if len(violations) > 0 {
			targetState = IndexStateReadableUniquePending
		}
	}

	if current == targetState {
		return false, nil
	}

	store.setIndexState(indexName, targetState)
	if targetState == IndexStateReadable {
		// Clear build data only when transitioning to READABLE.
		// READABLE_UNIQUE_PENDING keeps build data until violations are resolved.
		// Matches Java's clearReadableIndexBuildData().
		store.clearReadableIndexBuildData(idx)
	}
	return true, nil
}

// MarkIndexWriteOnly transitions an index to WRITE_ONLY state.
// Returns true if the state was changed.
// Matches Java's FDBRecordStore.markIndexWriteOnly().
func (store *FDBRecordStore) MarkIndexWriteOnly(indexName string) (bool, error) {
	if store.metaData.GetIndex(indexName) == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	current := store.GetIndexState(indexName)
	if current == IndexStateWriteOnly {
		return false, nil
	}
	store.setIndexState(indexName, IndexStateWriteOnly)
	return true, nil
}

// MarkIndexDisabled transitions an index to DISABLED state and clears all index data.
// Returns true if the state was changed.
// Matches Java's FDBRecordStore.markIndexDisabled().
func (store *FDBRecordStore) MarkIndexDisabled(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	current := store.GetIndexState(indexName)
	if current == IndexStateDisabled {
		return false, nil
	}
	store.setIndexState(indexName, IndexStateDisabled)
	if err := store.clearIndexData(idx); err != nil {
		return false, err
	}
	return true, nil
}

// ClearAndMarkIndexWriteOnly clears all index data and sets state to WRITE_ONLY.
// Used to start a fresh index rebuild.
// Matches Java's FDBRecordStore.clearAndMarkIndexWriteOnly().
func (store *FDBRecordStore) ClearAndMarkIndexWriteOnly(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	if err := store.clearIndexData(idx); err != nil {
		return false, err
	}
	current := store.GetIndexState(indexName)
	changed := current != IndexStateWriteOnly
	store.setIndexState(indexName, IndexStateWriteOnly)
	return changed, nil
}

// setIndexState persists an index state to FDB and updates the in-memory cache.
// Key format: subspace[IndexStateSpaceKey][indexName]
// Value format: tuple.Tuple{int64(state)}.Pack() — matches Java's Tuple.from(state.code()).pack()
// Also handles cache invalidation: sets dirty store state and bumps metadata version
// stamp when the store is cacheable.
// Goroutine-safe via stateMu (write lock) — matches Java's beginRecordStoreStateWrite().
// Matches Java's FDBRecordStore.updateIndexState().
func (store *FDBRecordStore) setIndexState(indexName string, state IndexState) {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	key := store.indexStateSubspace().Pack(tuple.Tuple{indexName})

	if state == IndexStateReadable {
		// READABLE is the default — remove the key to save space (matches Java behavior)
		store.context.Transaction().Clear(key)
	} else {
		value := tuple.Tuple{int64(state)}.Pack()
		store.context.Transaction().Set(key, value)
	}

	if store.indexStates == nil {
		store.indexStates = make(map[string]IndexState)
	}
	if state == IndexStateReadable {
		delete(store.indexStates, indexName)
	} else {
		store.indexStates[indexName] = state
	}

	// Cache invalidation: mark dirty and bump version stamp if cacheable.
	// Matches Java's updateIndexState() which calls setDirtyStoreState(true)
	// and setMetaDataVersionStamp() when the store header is cacheable.
	store.context.SetDirtyStoreState(true)
	if store.storeHeader != nil && store.storeHeader.GetCacheable() {
		store.context.SetMetaDataVersionStamp()
	}
}

// readIndexStates reads all index states from the IndexStateSpaceKey subspace.
// Only non-READABLE states are stored; absent entries default to READABLE.
// This is a standalone function that does not mutate any store fields.
// Matches Java's FDBRecordStore.loadIndexStatesAsync().
func readIndexStates(tx fdb.WritableTransaction, ss subspace.Subspace) (map[string]IndexState, error) {
	isSubspace := ss.Sub(IndexStateSpaceKey)
	begin, end := isSubspace.FDBRangeKeys()

	kvs, err := tx.Snapshot().GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("failed to load index states: %w", err)
	}

	prefixLen := len(isSubspace.Bytes())
	states := make(map[string]IndexState)
	for _, kv := range kvs {
		// Unpack key to get index name using fastUnpack.
		if len(kv.Key) < prefixLen {
			continue
		}
		t, err := fastUnpack(kv.Key[prefixLen:])
		if err != nil {
			return nil, fmt.Errorf("failed to unpack index state key: %w", err)
		}
		if len(t) == 0 {
			continue
		}
		indexName, ok := t[0].(string)
		if !ok {
			continue
		}

		// Unpack value to get state code.
		valueTuple, err := fastUnpack(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to unpack index state value for %q: %w", indexName, err)
		}
		if len(valueTuple) == 0 {
			continue
		}
		code, ok := valueTuple[0].(int64)
		if !ok {
			continue
		}

		state, err := indexStateFromCode(code)
		if err != nil {
			return nil, fmt.Errorf("invalid index state for %q: %w", indexName, err)
		}
		states[indexName] = state
	}

	return states, nil
}

// loadIndexStates reads all index states and sets them on the store.
// Convenience wrapper around readIndexStates.
func (store *FDBRecordStore) loadIndexStates() error {
	states, err := readIndexStates(store.context.Transaction(), store.subspace)
	if err != nil {
		return err
	}
	store.stateMu.Lock()
	store.indexStates = states
	store.stateMu.Unlock()
	return nil
}

// indexStateSubspace returns the FDB subspace for index state storage.
func (store *FDBRecordStore) indexStateSubspace() subspace.Subspace {
	return store.subspace.Sub(IndexStateSpaceKey)
}

// Index build subspace sub-keys matching Java's IndexingSubspaces.
const (
	indexBuildScannedRecordsSubKey = int64(1) // atomic ADD counter for records scanned
	indexBuildTypeVersionSubKey    = int64(2) // IndexBuildIndexingStamp proto
)

// indexBuildTypeSubspace returns the subspace for the index build type stamp.
// Matches Java's IndexingSubspaces.indexBuildTypeSubspace().
func (store *FDBRecordStore) indexBuildTypeSubspace(index *Index) subspace.Subspace {
	return store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), indexBuildTypeVersionSubKey)
}

// SaveIndexingTypeStamp persists the indexing method stamp for an index.
// Matches Java's FDBRecordStore.saveIndexingTypeStamp().
func (store *FDBRecordStore) SaveIndexingTypeStamp(index *Index, stamp *gen.IndexBuildIndexingStamp) error {
	data, err := stamp.MarshalVT()
	if err != nil {
		return fmt.Errorf("marshal indexing type stamp: %w", err)
	}
	stampKey := store.indexBuildTypeSubspace(index).Bytes()
	store.context.Transaction().Set(fdb.Key(stampKey), data)
	return nil
}

// LoadIndexingTypeStamp loads the indexing method stamp for an index.
// Returns nil if no stamp exists.
// Matches Java's FDBRecordStore.loadIndexingTypeStampAsync().
func (store *FDBRecordStore) LoadIndexingTypeStamp(index *Index) (*gen.IndexBuildIndexingStamp, error) {
	stampKey := store.indexBuildTypeSubspace(index).Bytes()
	data, err := store.context.Transaction().Get(fdb.Key(stampKey)).Get()
	if err != nil {
		return nil, fmt.Errorf("load indexing type stamp: %w", err)
	}
	if data == nil {
		return nil, nil
	}
	stamp := &gen.IndexBuildIndexingStamp{}
	if err := stamp.UnmarshalVT(data); err != nil {
		return nil, fmt.Errorf("unmarshal indexing type stamp: %w", err)
	}
	return stamp, nil
}

// AddBuildProgress atomically increments the records-scanned counter for an index build.
// Matches Java's IndexingBase.tieredMergeAndCommit() → MutationType.ADD at
// IndexingSubspaces.indexBuildScannedRecordsSubspace().
func (store *FDBRecordStore) AddBuildProgress(index *Index, count int64) {
	key := store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), indexBuildScannedRecordsSubKey).Bytes()
	store.context.Transaction().Add(fdb.Key(key), encodeRecordCount(count))
}

// LoadBuildProgress reads the records-scanned counter for an index build.
// Returns 0 if no progress has been tracked.
// Matches Java's IndexBuildState.loadRecordsScannedAsync().
func (store *FDBRecordStore) LoadBuildProgress(index *Index) (int64, error) {
	key := store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), indexBuildScannedRecordsSubKey).Bytes()
	data, err := store.context.Transaction().Get(fdb.Key(key)).Get()
	if err != nil {
		return 0, fmt.Errorf("load build progress: %w", err)
	}
	if data == nil {
		return 0, nil
	}
	return decodeRecordCount(data), nil
}

// clearIndexData removes all FDB data for an index.
// Matches Java's FDBRecordStore.clearIndexData().
func (store *FDBRecordStore) clearIndexData(index *Index) error {
	// Clear index entries using PrefixRange (not subspace.Range) to include
	// the exact prefix key. Ungrouped aggregate indexes (COUNT/SUM) store
	// data at the subspace prefix itself, which subspace.Range() excludes.
	// Matches Java's Range.startsWith(indexSubspace.pack()) — see comment in
	// FDBRecordStore.clearIndexData: "startsWith to handle ungrouped aggregate indexes".
	idxSubspace := store.indexSubspace(index)
	idxPrefixRange, err := fdb.PrefixRange(idxSubspace.Bytes())
	if err != nil {
		return fmt.Errorf("clear index data prefix range: %w", err)
	}
	store.context.Transaction().ClearRange(idxPrefixRange)

	// Clear secondary space
	secSubspace := store.subspace.Sub(IndexSecondarySpaceKey, index.SubspaceTupleKey())
	store.context.Transaction().ClearRange(secSubspace)

	// Clear uniqueness violations
	uvSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	store.context.Transaction().ClearRange(uvSubspace)

	// Clear range set
	rangeSubspace := store.subspace.Sub(IndexRangeSpaceKey, index.SubspaceTupleKey())
	store.context.Transaction().ClearRange(rangeSubspace)

	// Clear build space
	buildSubspace := store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey())
	store.context.Transaction().ClearRange(buildSubspace)

	return nil
}

// removeFormerIndexData clears all FDB data for a former (dropped) index.
// Matches Java's FDBRecordStore.removeFormerIndex() which clears:
// INDEX_KEY, INDEX_SECONDARY_SPACE_KEY, INDEX_RANGE_SPACE_KEY,
// INDEX_STATE_SPACE_KEY, and INDEX_UNIQUENESS_VIOLATIONS_KEY subspaces.
func (store *FDBRecordStore) removeFormerIndexData(former *FormerIndex) error {
	subKey := former.SubspaceKey

	// Clear index entries
	idxSubspace := store.subspace.Sub(IndexKey, subKey)
	pr, err := fdb.PrefixRange(idxSubspace.Bytes())
	if err != nil {
		return fmt.Errorf("remove former index prefix range: %w", err)
	}
	store.context.Transaction().ClearRange(pr)

	// Clear secondary space
	store.context.Transaction().ClearRange(store.subspace.Sub(IndexSecondarySpaceKey, subKey))

	// Clear uniqueness violations
	store.context.Transaction().ClearRange(store.subspace.Sub(IndexUniquenessViolationsKey, subKey))

	// Clear range set
	store.context.Transaction().ClearRange(store.subspace.Sub(IndexRangeSpaceKey, subKey))

	// Clear index state
	stateKey := store.subspace.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{subKey})
	store.context.Transaction().Clear(fdb.Key(stateKey))

	// Clear build space
	store.context.Transaction().ClearRange(store.subspace.Sub(IndexBuildSpaceKey, subKey))

	return nil
}

// shouldMaintainIndex returns true if the index should be updated on record changes.
// DISABLED indexes are skipped entirely. READABLE, WRITE_ONLY, and READABLE_UNIQUE_PENDING
// all receive updates.
// Caller must hold stateMu (read or write) — called from updateSecondaryIndexes which
// holds the read lock for the entire operation, matching Java's beginRecordStoreStateRead().
func (store *FDBRecordStore) shouldMaintainIndex(indexName string) bool {
	return !store.getIndexStateLocked(indexName).IsDisabled()
}

// checkIndexBuilt verifies that the index range set is complete (no unbuilt ranges).
// Matches Java's firstUnbuiltRange check in checkAndUpdateBuiltIndexState.
func (store *FDBRecordStore) checkIndexBuilt(index *Index) error {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	missing, err := rangeSet.FirstMissingRange(store.context.Transaction())
	if err != nil {
		return fmt.Errorf("check index %q built state: %w", index.Name, err)
	}
	if missing != nil {
		return &IndexNotBuiltError{IndexName: index.Name}
	}
	return nil
}

// clearReadableIndexBuildData clears build tracking data (range set and heartbeats)
// for an index that has transitioned to READABLE state.
// Matches Java's FDBRecordStore.clearReadableIndexBuildData().
func (store *FDBRecordStore) clearReadableIndexBuildData(index *Index) {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	rangeSet.Clear(store.context.Transaction())
	// Clear all heartbeats — matching Java's IndexingHeartbeat.clearAllHeartbeats().
	// Without this, stale heartbeats from crashed mutual builders accumulate
	// and cause transient blocking on re-builds.
	CleanupAllHeartbeats(store.context.Transaction(), store.subspace, index)
}
