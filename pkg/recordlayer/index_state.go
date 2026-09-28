package recordlayer

import (
	"bytes"
	"fmt"
	"maps"
	"sync"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
	// IndexStateWriteOnlyWithQueue defers user maintenance to the pending queue.
	IndexStateWriteOnlyWithQueue IndexState = 4
)

// IsScannable returns true if this state allows index scans.
// Matches Java's IndexState.isScannable() — READABLE or READABLE_UNIQUE_PENDING.
func (s IndexState) IsScannable() bool {
	return s == IndexStateReadable || s == IndexStateReadableUniquePending
}

// IsWriteOnly returns true for either write-only state.
func (s IndexState) IsWriteOnly() bool {
	return s.IsWriteOnlyNoQueue() || s.IsWriteOnlyWithQueue()
}

func (s IndexState) IsWriteOnlyNoQueue() bool   { return s == IndexStateWriteOnly }
func (s IndexState) IsWriteOnlyWithQueue() bool { return s == IndexStateWriteOnlyWithQueue }

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
	case IndexStateWriteOnlyWithQueue:
		return "WRITE_ONLY_WITH_QUEUE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

// indexStateFromCode converts a numeric code to IndexState.
// Matches Java's IndexState.fromCode().
func indexStateFromCode(code int64) (IndexState, error) {
	switch IndexState(code) {
	case IndexStateReadable, IndexStateWriteOnly, IndexStateDisabled, IndexStateReadableUniquePending, IndexStateWriteOnlyWithQueue:
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
	if store.indexStateView != nil {
		state, _ := store.indexStateView.read(store.context.Transaction(), indexName)
		return state
	}
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

// IsIndexWriteOnly returns true for either write-only state.
func (store *FDBRecordStore) IsIndexWriteOnly(indexName string) bool {
	return store.GetIndexState(indexName).IsWriteOnly()
}

func (store *FDBRecordStore) IsIndexWriteOnlyNoQueue(indexName string) bool {
	return store.GetIndexState(indexName).IsWriteOnlyNoQueue()
}

func (store *FDBRecordStore) IsIndexWriteOnlyWithQueue(indexName string) bool {
	return store.GetIndexState(indexName).IsWriteOnlyWithQueue()
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
	if err := store.beginIndexStateWrite(); err != nil {
		return false, err
	}
	defer store.endIndexStateWrite()
	current, err := store.readIndexState(indexName)
	if err != nil {
		return false, err
	}
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
				IndexName:   indexName,
				IndexKey:    violations[0].IndexKey,
				PrimaryKey:  violations[0].PrimaryKey,
				ExistingKey: violations[0].ExistingKey,
			}
		}
	}

	store.setIndexStateLocked(indexName, IndexStateReadable)
	store.clearReadableIndexBuildData(idx)
	store.scheduleReplacementRetirement()
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

	if err := store.beginIndexStateWrite(); err != nil {
		return false, err
	}
	defer store.endIndexStateWrite()
	current, err := store.readIndexState(indexName)
	if err != nil {
		return false, err
	}
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

	store.setIndexStateLocked(indexName, targetState)
	if targetState == IndexStateReadable {
		// Clear build data only when transitioning to READABLE.
		// READABLE_UNIQUE_PENDING keeps build data until violations are resolved.
		// Matches Java's clearReadableIndexBuildData().
		store.clearReadableIndexBuildData(idx)
	}
	store.scheduleReplacementRetirement()
	return true, nil
}

// MarkIndexWriteOnly transitions an index to WRITE_ONLY state.
// Returns true if the state was changed.
// Matches Java's FDBRecordStore.markIndexWriteOnly().
func (store *FDBRecordStore) MarkIndexWriteOnly(indexName string) (bool, error) {
	return store.markIndexWriteOnly(indexName, IndexStateWriteOnly)
}

// MarkIndexWriteOnlyWithQueue requests deferred user maintenance. RFC-257 adds
// eligibility validation to Java's setter so unsupported requests cannot persist
// an index state that the store cannot maintain.
func (store *FDBRecordStore) MarkIndexWriteOnlyWithQueue(indexName string) (bool, error) {
	return store.markIndexWriteOnly(indexName, IndexStateWriteOnlyWithQueue)
}

func (store *FDBRecordStore) markIndexWriteOnly(indexName string, target IndexState) (bool, error) {
	index := store.metaData.GetIndex(indexName)
	if index == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	if err := store.beginIndexStateWrite(); err != nil {
		return false, err
	}
	defer store.endIndexStateWrite()
	current, err := store.readIndexState(indexName)
	if err != nil {
		return false, err
	}
	if target.IsWriteOnlyWithQueue() {
		if err := store.validatePendingQueueIndex(index); err != nil {
			return false, err
		}
	}
	if current == target {
		return false, nil
	}
	if current == IndexStateReadable {
		// Readable indexes discard their completed range set. Restore that
		// coverage when leaving readable without clearing index data, as Java's
		// markIndexNotReadable does. Preserve any existing partial coverage.
		ranges := NewIndexingRangeSet(store.subspace, store.metaData.GetIndex(indexName))
		empty, err := ranges.rangeSet.IsEmpty(store.context.Transaction())
		if err != nil {
			return false, err
		}
		if empty {
			if _, err := ranges.InsertRange(store.context.Transaction(), nil, nil, false); err != nil {
				return false, err
			}
		}
	}
	store.setIndexStateLocked(indexName, target)
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
	if err := store.beginIndexStateWrite(); err != nil {
		return false, err
	}
	defer store.endIndexStateWrite()
	current, err := store.readIndexState(indexName)
	if err != nil {
		return false, err
	}
	if current == IndexStateDisabled {
		return false, nil
	}
	store.setIndexStateLocked(indexName, IndexStateDisabled)
	if err := store.clearIndexData(idx); err != nil {
		return false, err
	}
	return true, nil
}

// ClearAndMarkIndexWriteOnly clears all index data and sets state to WRITE_ONLY.
// Used to start a fresh index rebuild.
// Matches Java's FDBRecordStore.clearAndMarkIndexWriteOnly().
func (store *FDBRecordStore) ClearAndMarkIndexWriteOnly(indexName string) (bool, error) {
	return store.clearAndMarkIndexWriteOnly(indexName, IndexStateWriteOnly)
}

func (store *FDBRecordStore) ClearAndMarkIndexWriteOnlyWithQueue(indexName string) (bool, error) {
	return store.clearAndMarkIndexWriteOnly(indexName, IndexStateWriteOnlyWithQueue)
}

func (store *FDBRecordStore) clearAndMarkIndexWriteOnly(indexName string, target IndexState) (bool, error) {
	idx := store.metaData.GetIndex(indexName)
	if idx == nil {
		return false, &IndexNotFoundError{IndexName: indexName}
	}
	if err := store.beginIndexStateWrite(); err != nil {
		return false, err
	}
	defer store.endIndexStateWrite()
	current, err := store.readIndexState(indexName)
	if err != nil {
		return false, err
	}
	if target.IsWriteOnlyWithQueue() {
		if err := store.validatePendingQueueIndex(idx); err != nil {
			return false, err
		}
	}
	if err := store.clearIndexData(idx); err != nil {
		return false, err
	}
	changed := current != target
	store.setIndexStateLocked(indexName, target)
	return changed, nil
}

// transactionIndexStateView shares state changes between store handles in one
// context. Cached state is paired with per-index read conflicts, as in Java's
// getIndexState; checking state must not issue a point read for every record.
type transactionIndexStateView struct {
	maintenance   sync.RWMutex // state decision through maintenance; exclusive for transitions
	mu            sync.Mutex
	storeSubspace subspace.Subspace
	subspace      subspace.Subspace
	states        map[string]IndexState
	conflicts     map[string]bool
}

// indexStateViewLocked binds loaded state while indexStateMu is held. The caller
// must hold that lock from before loading state until after binding it.
func (rc *FDBRecordContext) indexStateViewLocked(ss subspace.Subspace, states map[string]IndexState) *transactionIndexStateView {
	if rc.indexStateViews == nil {
		rc.indexStateViews = make(map[string]*transactionIndexStateView)
	}
	key := string(ss.Bytes())
	view := rc.indexStateViews[key]
	if view == nil {
		view = &transactionIndexStateView{storeSubspace: ss, subspace: ss.Sub(IndexStateSpaceKey), states: maps.Clone(states), conflicts: make(map[string]bool)}
		rc.indexStateViews[key] = view
	}
	return view
}

func indexStateRangeOverlaps(ss subspace.Subspace, begin, end []byte) bool {
	stateBegin, stateEnd := ss.FDBRangeKeys()
	return bytes.Compare(begin, end) < 0 && bytes.Compare(begin, stateEnd.FDBKey()) < 0 && bytes.Compare(stateBegin.FDBKey(), end) < 0
}

// rangeMayChangeStoreStateLocked is conservative for unclassified ranges.
// Known store data excludes the header and index-state region. Before a store
// has been opened, a record-key prefix is only trusted after reading its header;
// bytes alone cannot distinguish a store prefix from arbitrary application keys.
// Caller holds indexStateMu, before issuing the clear.
func (rc *FDBRecordContext) rangeMayChangeStoreStateLocked(begin, end []byte) bool {
	if bytes.Compare(begin, end) >= 0 {
		return false
	}
	knownData := false
	for _, view := range rc.indexStateViews {
		headerKey := view.storeSubspace.Pack(tuple.Tuple{StoreInfoKey})
		if indexStateRangeOverlaps(view.subspace, begin, end) ||
			(bytes.Compare(begin, headerKey) <= 0 && bytes.Compare(headerKey, end) < 0) {
			return true
		}
		storeBegin, storeEnd := view.storeSubspace.FDBRangeKeys()
		if bytes.Compare(storeBegin.FDBKey(), begin) <= 0 && bytes.Compare(end, storeEnd.FDBKey()) <= 0 {
			knownData = true
		}
	}
	if knownData {
		return false
	}

	// Record-only clears need not invalidate every cached store in the cluster.
	// Search candidate tuple boundaries because the store's raw prefix need not
	// itself be a tuple. A candidate must contain the entire range and have a
	// valid store header; unrecognized ranges retain conservative invalidation.
	recordKey := tuple.Tuple{RecordKey}.Pack()
	for offset := 0; offset < len(begin); {
		at := bytes.Index(begin[offset:], recordKey)
		if at < 0 {
			break
		}
		at += offset
		offset = at + len(recordKey)
		ss := subspace.FromBytes(begin[:at])
		recordBegin, recordEnd := ss.Sub(RecordKey).FDBRangeKeys()
		if bytes.Compare(recordBegin.FDBKey(), begin) > 0 || bytes.Compare(end, recordEnd.FDBKey()) > 0 {
			continue
		}
		raw, err := rc.tx.Get(ss.Pack(tuple.Tuple{StoreInfoKey})).Get()
		if err != nil || raw == nil {
			continue
		}
		header := &gen.DataStoreInfo{}
		if err := UnmarshalVTAsJava(header, raw); err == nil && header.GetFormatVersion() >= formatVersionMinimum {
			return false
		}
	}
	return true
}

func (rc *FDBRecordContext) clearRangeWithIndexStateViews(keyRange fdb.ExactRange, invalidateState bool) {
	beginKey, endKey := keyRange.FDBRangeKeys()
	begin, end := beginKey.FDBKey(), endKey.FDBKey()
	rc.indexStateMu.Lock()
	defer rc.indexStateMu.Unlock()
	// Keep the transaction clear and every affected projection in one ordering
	// domain with state writes, including writes through another store handle.
	for _, view := range rc.indexStateViews {
		view.mu.Lock()
		defer view.mu.Unlock()
	}
	if invalidateState && rc.rangeMayChangeStoreStateLocked(begin, end) {
		// Unknown ranges must invalidate before commit, even if no store is
		// opened in this context. Deferring until a later load loses history.
		rc.SetDirtyStoreState(true)
		rc.SetMetaDataVersionStamp()
	}
	rc.tx.ClearRange(keyRange)
	for _, view := range rc.indexStateViews {
		for name := range view.states {
			key := view.subspace.Pack(tuple.Tuple{name})
			if bytes.Compare(key, begin) >= 0 && bytes.Compare(key, end) < 0 {
				delete(view.states, name)
			}
		}
	}
}

func (view *transactionIndexStateView) read(tx fdb.WritableTransaction, indexName string) (IndexState, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if !view.conflicts[indexName] {
		if err := tx.AddReadConflictKey(view.subspace.Pack(tuple.Tuple{indexName})); err != nil {
			return IndexStateReadable, err
		}
		view.conflicts[indexName] = true
	}
	return view.states[indexName], nil
}

// ReadIndexState returns transaction-visible state with a serializable state-key
// conflict and propagates initialization and transaction-liveness errors.
func (store *FDBRecordStore) ReadIndexState(indexName string) (IndexState, error) {
	return store.readIndexState(indexName)
}

// readIndexState answers from the context's transaction-visible state and adds
// a serializable state-key conflict. Actual index-data isolation is independent.
func (store *FDBRecordStore) readIndexState(indexName string) (IndexState, error) {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return IndexStateReadable, err
	}
	// A cached read version checks cancellation/timeouts without another GRV.
	// Adding a conflict alone does not validate transaction liveness in FDB.
	if _, err := store.context.Transaction().GetReadVersion().Get(); err != nil {
		return IndexStateReadable, err
	}
	return store.indexStateView.read(store.context.Transaction(), indexName)
}

func replacementRetirementCheckName(ss subspace.Subspace) string {
	return fmt.Sprintf("removeReplacedIndexes_%x", ss.Bytes())
}

func replacementRetirementMetadataKey(ss subspace.Subspace) string {
	return replacementRetirementCheckName(ss) + "_metadata"
}

// rememberRetirementMetadata keeps deferred work on the newest successfully
// opened metadata in this context. An older handle may finish a replacement
// after a newer schema has canceled that relationship; it must not restore it.
func (store *FDBRecordStore) rememberRetirementMetadata() {
	ctx := store.context
	key := replacementRetirementMetadataKey(store.subspace)
	ctx.sessionMu.Lock()
	defer ctx.sessionMu.Unlock()
	if ctx.session == nil {
		ctx.session = make(map[string]any)
	}
	current, _ := ctx.session[key].(*FDBRecordStore)
	if current == nil || current.metaData.Version() <= store.metaData.Version() {
		ctx.session[key] = store
	}
}

func (store *FDBRecordStore) scheduleReplacementRetirement() {
	store.rememberRetirementMetadata()
	ctx := store.context
	key := replacementRetirementMetadataKey(store.subspace)
	ctx.getOrCreateCommitCheck(replacementRetirementCheckName(store.subspace), func(string) CommitCheckFunc {
		return func() error {
			current, _ := ctx.Session(key).(*FDBRecordStore)
			if current == nil {
				return nil
			}
			return current.removeReplacedIndexes()
		}
	})
}

// removeReplacedIndexes requires every replacement to be exactly READABLE.
// Collect candidates under the state read lock, then release it before the
// disabling transitions acquire the write lock, as in Java's implementation.
func (store *FDBRecordStore) removeReplacedIndexes() error {
	candidates, err := store.replacedIndexesReady()
	if err != nil {
		return err
	}
	for _, index := range candidates {
		if _, err := store.MarkIndexDisabled(index.Name); err != nil {
			return err
		}
	}
	return nil
}

func (store *FDBRecordStore) replacedIndexesReady() ([]*Index, error) {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	var candidates []*Index
	for _, index := range store.metaData.GetAllIndexes() {
		replacements := index.GetReplacedByIndexNames()
		if len(replacements) == 0 {
			continue
		}
		ready := true
		for _, name := range replacements {
			if store.metaData.GetIndex(name) == nil {
				ready = false
				break
			}
			state, err := store.readIndexState(name)
			if err != nil {
				return nil, err
			}
			if state != IndexStateReadable {
				ready = false
				break
			}
		}
		if ready {
			candidates = append(candidates, index)
		}
	}
	return candidates, nil
}

// beginIndexStateWrite excludes record maintenance across every handle bound to
// this store in the context. Load before locking; registry/view/version locks
// are lower in the order than the handle and maintenance locks.
func (store *FDBRecordStore) beginIndexStateWrite() error {
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return err
	}
	store.stateMu.Lock()
	store.indexStateView.maintenance.Lock()
	return nil
}

func (store *FDBRecordStore) endIndexStateWrite() {
	store.indexStateView.maintenance.Unlock()
	store.stateMu.Unlock()
}

// setIndexState persists an index state to FDB and updates the in-memory cache.
// Key format: subspace[IndexStateSpaceKey][indexName]
// Value format: tuple.Tuple{int64(state)}.Pack() — matches Java's Tuple.from(state.code()).pack()
// Also handles cache invalidation: sets dirty store state and bumps metadata version
// stamp when the store is cacheable.
// Goroutine-safe via stateMu (write lock) — matches Java's beginRecordStoreStateWrite().
// Matches Java's FDBRecordStore.updateIndexState().
func (store *FDBRecordStore) setIndexState(indexName string, state IndexState) {
	// Lifecycle reconciliation can write through a newly opened handle before
	// any state getter is used. Attach it to the context authority first.
	store.ensureStoreStateLoaded()
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	store.indexStateView.maintenance.Lock()
	defer store.indexStateView.maintenance.Unlock()
	store.setIndexStateLocked(indexName, state)
}

// setIndexStateLocked requires exclusive handle and shared maintenance locks.
func (store *FDBRecordStore) setIndexStateLocked(indexName string, state IndexState) {
	store.indexStateView.mu.Lock()
	defer store.indexStateView.mu.Unlock()
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

	if state == IndexStateReadable {
		delete(store.indexStateView.states, indexName)
	} else {
		store.indexStateView.states[indexName] = state
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
	return LoadIndexStates(tx, ss)
}

// LoadIndexStates reads a record store's index-state map from a READ
// transaction, without opening the store. Only states that differ from the
// default are stored, so an absent index is READABLE — the same contract
// FDBRecordStore.GetIndexState applies (and Java's RecordStoreState).
//
// It exists because the QUERY PLANNER needs index states before any store is
// open: Java assembles its planner's readable-index view from the store state
// it already holds (PlanContext.java:236-247), while Go plans from metadata
// alone and reaches FDB only at execution. Opening a full store just to read
// four bytes per exceptional index would pull in the header, the metadata
// version check and the store lock; this reads the one range it needs.
func LoadIndexStates(tx fdb.ReadTransaction, ss subspace.Subspace) (map[string]IndexState, error) {
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
	if err := UnmarshalVTAsJava(stamp, data); err != nil {
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
	store.context.ClearRange(idxPrefixRange)

	// Clear secondary space
	secSubspace := store.subspace.Sub(IndexSecondarySpaceKey, index.SubspaceTupleKey())
	store.context.ClearRange(secSubspace)

	// Clear sliding-window bookkeeping. Matches Java's
	// `context.clear(indexSlidingWindowSubspace(index).range())`.
	//
	// It has to go with the rest: the keyspace-10 entry list and its
	// count/boundary describe WHICH records are in the index that was just
	// emptied. Leaving them behind starts a rebuild believing the window is
	// already full, so the first insert compares against a boundary naming a
	// record the graph no longer holds and skips its own insert.
	store.context.ClearRange(
		store.subspace.Sub(IndexSlidingWindowSpaceKey, index.SubspaceTupleKey()))

	// Clear uniqueness violations
	uvSubspace := store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())
	store.context.ClearRange(uvSubspace)

	// Clear range set
	rangeSubspace := store.subspace.Sub(IndexRangeSpaceKey, index.SubspaceTupleKey())
	store.context.ClearRange(rangeSubspace)

	// Preserve a Java online builder's lock while removing its other artifacts.
	return store.eraseAllIndexingDataButTheLockAndRangeSet(index)
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
	store.context.ClearRange(pr)

	// Clear secondary space
	store.context.ClearRange(store.subspace.Sub(IndexSecondarySpaceKey, subKey))

	// Clear sliding-window bookkeeping. Matches Java's removeFormerIndex, which
	// clears INDEX_SLIDING_WINDOW_SPACE_KEY alongside the others. A dropped
	// index that left its keyspace-10 region behind would leak it forever: no
	// maintainer exists for a former index, so nothing would ever clear it, and
	// a later index reusing the subspace key would inherit a full window.
	store.context.ClearRange(store.subspace.Sub(IndexSlidingWindowSpaceKey, subKey))

	// Clear uniqueness violations
	store.context.ClearRange(store.subspace.Sub(IndexUniquenessViolationsKey, subKey))

	// Clear range set
	store.context.ClearRange(store.subspace.Sub(IndexRangeSpaceKey, subKey))

	// State is keyed by index name, not by its independently assigned subspace
	// key. Use the ordinary state update to invalidate cached store state too.
	if former.FormerName != "" {
		store.setIndexState(former.FormerName, IndexStateReadable)
	}
	// Java leaves build-space data, including another builder's lock, intact.

	return nil
}

// shouldMaintainIndex returns true if the index should be updated on record changes.
// DISABLED indexes are skipped entirely. READABLE, WRITE_ONLY, and READABLE_UNIQUE_PENDING
// all receive updates.
// Caller must hold stateMu (read or write) — called from updateSecondaryIndexes which
// holds the read lock for the entire operation, matching Java's beginRecordStoreStateRead().
func (store *FDBRecordStore) shouldMaintainIndex(indexName string) (bool, error) {
	state, err := store.indexStateView.read(store.context.Transaction(), indexName)
	return !state.IsDisabled(), err
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
	// RFC-257 strengthens Java's checked publication: complete scan ranges do
	// not imply that deferred writes have been applied. Check the shared
	// buffered registry and a serializable persisted range before publication.
	empty, err := store.isIndexPendingQueueEmpty(index)
	if err != nil {
		return err
	}
	if !empty {
		return &IndexNotBuiltError{IndexName: index.Name, PendingWrites: true}
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

// eraseAllIndexingDataButTheLockAndRangeSet clears Java's build bookkeeping,
// preserving the lock (subkey 0), unknown future subkeys, and the separate range
// set. Java-created scrub and pending-queue data must also be cleared even when
// Go does not produce it. Clearing it does not enable queued index maintenance.
// Matches Java's IndexingSubspaces.eraseAllIndexingDataButTheLockAndRangeSet.
func (store *FDBRecordStore) eraseAllIndexingDataButTheLockAndRangeSet(index *Index) error {
	subkeys := []int64{
		indexBuildScannedRecordsSubKey,
		indexBuildTypeVersionSubKey,
		3, // INDEX_SCRUBBED_INDEX_RANGES_ZERO
		4, // INDEX_SCRUBBED_RECORDS_RANGES_ZERO
		5, // INDEX_SCRUBBED_RECORDS_RANGES
		6, // INDEX_SCRUBBED_INDEX_RANGES
		indexBuildHeartbeatSubKey,
		8, // INDEX_PENDING_WRITE_QUEUE_PREFIX
		9, // INDEX_PENDING_WRITE_QUEUE_SIZE
	}
	for _, key := range subkeys {
		prefix := store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), key).Bytes()
		// Include the prefix itself: counters and type stamps live at that key.
		pr, err := fdb.PrefixRange(prefix)
		if err != nil {
			return fmt.Errorf("erase indexing data: %w", err)
		}
		store.context.ClearRange(pr)
	}
	return nil
}
