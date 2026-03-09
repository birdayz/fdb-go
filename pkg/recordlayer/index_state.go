package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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

// ErrIndexNotReadable is returned when trying to scan a non-readable index.
var ErrIndexNotReadable = fmt.Errorf("index is not readable")

// ErrIndexNotFound is returned when an index name is not in the metadata.
var ErrIndexNotFound = fmt.Errorf("index not found in metadata")

// GetIndexState returns the state of the given index. Returns READABLE if no
// explicit state is stored (matching Java's default behavior).
func (store *FDBRecordStore) GetIndexState(indexName string) IndexState {
	if store.indexStates == nil {
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
// Matches Java's FDBRecordStore.markIndexReadable().
func (store *FDBRecordStore) MarkIndexReadable(indexName string) (bool, error) {
	if store.metaData.GetIndex(indexName) == nil {
		return false, fmt.Errorf("%w: %s", ErrIndexNotFound, indexName)
	}
	current := store.GetIndexState(indexName)
	if current == IndexStateReadable {
		return false, nil
	}
	store.setIndexState(indexName, IndexStateReadable)
	return true, nil
}

// MarkIndexReadableOrUniquePending transitions a unique index to READABLE if it has
// no uniqueness violations, or to READABLE_UNIQUE_PENDING if violations exist.
// For non-unique indexes, always transitions to READABLE.
// Returns true if the state was changed.
// Matches Java's FDBRecordStore.markIndexReadable() with uniqueness check.
func (store *FDBRecordStore) MarkIndexReadableOrUniquePending(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, fmt.Errorf("%w: %s", ErrIndexNotFound, indexName)
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

	current := store.GetIndexState(indexName)
	if current == targetState {
		return false, nil
	}
	store.setIndexState(indexName, targetState)
	return true, nil
}

// MarkIndexWriteOnly transitions an index to WRITE_ONLY state.
// Returns true if the state was changed.
// Matches Java's FDBRecordStore.markIndexWriteOnly().
func (store *FDBRecordStore) MarkIndexWriteOnly(indexName string) (bool, error) {
	if store.metaData.GetIndex(indexName) == nil {
		return false, fmt.Errorf("%w: %s", ErrIndexNotFound, indexName)
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
		return false, fmt.Errorf("%w: %s", ErrIndexNotFound, indexName)
	}
	current := store.GetIndexState(indexName)
	if current == IndexStateDisabled {
		return false, nil
	}
	store.setIndexState(indexName, IndexStateDisabled)
	store.clearIndexData(idx)
	return true, nil
}

// ClearAndMarkIndexWriteOnly clears all index data and sets state to WRITE_ONLY.
// Used to start a fresh index rebuild.
// Matches Java's FDBRecordStore.clearAndMarkIndexWriteOnly().
func (store *FDBRecordStore) ClearAndMarkIndexWriteOnly(indexName string) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, fmt.Errorf("%w: %s", ErrIndexNotFound, indexName)
	}
	store.clearIndexData(idx)
	current := store.GetIndexState(indexName)
	changed := current != IndexStateWriteOnly
	store.setIndexState(indexName, IndexStateWriteOnly)
	return changed, nil
}

// setIndexState persists an index state to FDB and updates the in-memory cache.
// Key format: subspace[IndexStateSpaceKey][indexName]
// Value format: tuple.Tuple{int64(state)}.Pack() — matches Java's Tuple.from(state.code()).pack()
func (store *FDBRecordStore) setIndexState(indexName string, state IndexState) {
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
}

// loadIndexStates reads all index states from the IndexStateSpaceKey subspace.
// Only non-READABLE states are stored; absent entries default to READABLE.
// Matches Java's FDBRecordStore.loadIndexStatesAsync().
func (store *FDBRecordStore) loadIndexStates() error {
	isSubspace := store.indexStateSubspace()
	begin, end := isSubspace.FDBRangeKeys()

	kvs, err := store.context.Transaction().Snapshot().GetRange(
		fdb.KeyRange{Begin: begin, End: end},
		fdb.RangeOptions{},
	).GetSliceWithError()
	if err != nil {
		return fmt.Errorf("failed to load index states: %w", err)
	}

	states := make(map[string]IndexState)
	for _, kv := range kvs {
		// Unpack key to get index name
		t, err := isSubspace.Unpack(kv.Key)
		if err != nil {
			return fmt.Errorf("failed to unpack index state key: %w", err)
		}
		if len(t) == 0 {
			continue
		}
		indexName, ok := t[0].(string)
		if !ok {
			continue
		}

		// Unpack value to get state code
		valueTuple, err := tuple.Unpack(kv.Value)
		if err != nil {
			return fmt.Errorf("failed to unpack index state value for %q: %w", indexName, err)
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
			return fmt.Errorf("invalid index state for %q: %w", indexName, err)
		}
		states[indexName] = state
	}

	store.indexStates = states
	return nil
}

// indexStateSubspace returns the FDB subspace for index state storage.
func (store *FDBRecordStore) indexStateSubspace() subspace.Subspace {
	return store.subspace.Sub(IndexStateSpaceKey)
}

// clearIndexData removes all FDB data for an index.
// Matches Java's FDBRecordStore.clearIndexData().
func (store *FDBRecordStore) clearIndexData(index *Index) {
	// Clear index entries using PrefixRange (not subspace.Range) to include
	// the exact prefix key. Ungrouped aggregate indexes (COUNT/SUM) store
	// data at the subspace prefix itself, which subspace.Range() excludes.
	// Matches Java's Range.startsWith(indexSubspace.pack()) — see comment in
	// FDBRecordStore.clearIndexData: "startsWith to handle ungrouped aggregate indexes".
	idxSubspace := store.indexSubspace(index)
	idxPrefixRange, err := fdb.PrefixRange(idxSubspace.Bytes())
	if err == nil {
		store.context.Transaction().ClearRange(idxPrefixRange)
	} else {
		// Fallback: should never happen for valid subspace prefixes
		store.context.Transaction().ClearRange(idxSubspace)
	}

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
}

// shouldMaintainIndex returns true if the index should be updated on record changes.
// DISABLED indexes are skipped entirely. READABLE, WRITE_ONLY, and READABLE_UNIQUE_PENDING
// all receive updates.
func (store *FDBRecordStore) shouldMaintainIndex(indexName string) bool {
	return !store.GetIndexState(indexName).IsDisabled()
}
