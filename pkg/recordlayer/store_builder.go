package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// RebuildIndex rebuilds an index within the current transaction.
// Clears existing index data, scans all records, and re-indexes them.
// Upon completion, the index is marked READABLE.
//
// Because this runs in a single transaction, it is limited by FDB's
// 5-second time limit and 10MB transaction size. For large stores,
// use OnlineIndexer.BuildIndex() which splits work across transactions.
//
// Matches Java's FDBRecordStore.rebuildIndex() which delegates to
// IndexingBase.rebuildIndexAsync() for the in-transaction path.
func (store *FDBRecordStore) RebuildIndex(index *Index) error {
	if index == nil {
		return fmt.Errorf("index must not be nil")
	}
	startTime := time.Now()
	defer func() { store.context.Timer().RecordSince(EventRebuildIndex, startTime) }()

	// Step 1: Clear index data and mark WRITE_ONLY.
	// Matches Java: clearAndMarkIndexWriteOnly(index)
	if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: clear and mark write-only: %w", index.Name, err)
	}

	// Step 2: Pre-mark the full range as built in the RangeSet.
	// Java does this BEFORE scanning records so that even if marking readable
	// fails (e.g. uniqueness violations), the range set records that all data
	// was scanned, preventing re-scanning on future builds.
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	if _, err := rangeSet.InsertRange(store.context.Transaction(), nil, nil, true); err != nil {
		return fmt.Errorf("rebuild index %q: insert full range: %w", index.Name, err)
	}

	// Step 3: Scan all records and build index entries.
	scanProps := ForwardScan()
	cursor := store.ScanRecords(nil, scanProps)
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return fmt.Errorf("rebuild index %q: get maintainer: %w", index.Name, err)
	}

	for rec, err := range Seq2(cursor, store.context.ctx) {
		if err != nil {
			return fmt.Errorf("rebuild index %q: scan records: %w", index.Name, err)
		}

		if !store.shouldIndexRecordForIndex(rec, index) {
			continue
		}

		if err := maintainer.Update(nil, rec); err != nil {
			return fmt.Errorf("rebuild index %q: index record pk=%v: %w", index.Name, rec.PrimaryKey, err)
		}
	}

	// Step 4: Mark index READABLE (or READABLE_UNIQUE_PENDING if violations exist).
	// Matches Java: uses markIndexReadable which checks violations for unique indexes.
	if _, err := store.MarkIndexReadableOrUniquePending(index.Name); err != nil {
		return fmt.Errorf("rebuild index %q: mark readable: %w", index.Name, err)
	}

	return nil
}

// validateFormatVersion checks that the stored format version is supported.
// Rejects versions below formatVersionMinimum (1) and above formatVersionCurrent.
// Matches Java's FormatVersion.validateFormatVersion().
func (store *FDBRecordStore) validateFormatVersion(storeHeader *gen.DataStoreInfo) error {
	storedVersion := storeHeader.GetFormatVersion()
	if storedVersion < formatVersionMinimum || storedVersion > formatVersionCurrent {
		return &UnsupportedFormatVersionError{Version: storedVersion, MaxVersion: int32(formatVersionCurrent)}
	}
	return nil
}

// checkPossiblyRebuild compares the stored metadata version with the current
// metadata version. If the current metadata has a higher version, indexes added
// since the old version are rebuilt or marked according to the IndexRebuildPolicy.
// Matches Java's FDBRecordStore.checkPossiblyRebuild() / checkRebuild() /
// getStatesForRebuildIndexes().
func (store *FDBRecordStore) checkPossiblyRebuild(storeHeader *gen.DataStoreInfo) error {
	oldMetaDataVersion := int(storeHeader.GetMetaDataversion())
	newMetaDataVersion := store.metaData.Version()

	// Stale metadata check: stored version is newer than local version.
	// Matches Java: throws RecordStoreStaleMetaDataVersionException.
	if oldMetaDataVersion > newMetaDataVersion {
		return &StaleMetaDataVersionError{
			LocalVersion:  newMetaDataVersion,
			StoredVersion: oldMetaDataVersion,
		}
	}

	// Check record counts BEFORE the version gate — this compares the stored
	// RecordCountKey proto against the current one, independent of version.
	// Matches Java's checkRebuild() which always calls checkPossiblyRebuildRecordCounts().
	needHeaderWrite, err := store.checkPossiblyRebuildRecordCounts(storeHeader)
	if err != nil {
		return fmt.Errorf("rebuild record counts: %w", err)
	}

	if newMetaDataVersion == oldMetaDataVersion {
		// Even when versions match, the record count check may have modified
		// the header (updated RecordCountKey). Persist if needed.
		if needHeaderWrite {
			if err := store.writeStoreHeader(storeHeader); err != nil {
				return fmt.Errorf("update store header after record count rebuild: %w", err)
			}
		}
		return nil
	}

	// Version changed — set the flag (matches Java's versionChanged field).
	store.versionChanged = true

	// Clean up data for former indexes (dropped since old version).
	// Matches Java's checkRebuild() which calls removeFormerIndex() for each,
	// clearing INDEX_KEY, INDEX_SECONDARY_SPACE_KEY, INDEX_RANGE_SPACE_KEY,
	// INDEX_STATE_SPACE_KEY, and INDEX_UNIQUENESS_VIOLATIONS_KEY subspaces.
	for _, former := range store.metaData.GetFormerIndexes() {
		if former.RemovedVersion > oldMetaDataVersion {
			if err := store.removeFormerIndexData(former); err != nil {
				return err
			}
		}
	}

	// Find indexes added since the old version.
	indexesToBuild := store.metaData.GetIndexesToBuildSince(oldMetaDataVersion)
	if len(indexesToBuild) > 0 {
		// Get record count for the policy decision (lazy in Java, eager here).
		recordCount, err := store.getRecordCountForRebuildPolicy()
		if err != nil {
			return fmt.Errorf("check record count for rebuild: %w", err)
		}

		for _, index := range indexesToBuild {
			indexOnNewRecordTypes := store.areAllRecordTypesSince(index, oldMetaDataVersion)
			desiredState := store.indexRebuildPolicy(index, recordCount, indexOnNewRecordTypes)

			switch desiredState {
			case IndexStateReadable:
				if err := store.RebuildIndex(index); err != nil {
					return fmt.Errorf("auto-rebuild index %q on metadata version change (%d -> %d): %w",
						index.Name, oldMetaDataVersion, newMetaDataVersion, err)
				}
			case IndexStateWriteOnly:
				// Always clear and re-mark, matching Java's rebuildOrMarkIndex().
				// The header version update and clearAndMark are in the same FDB
				// transaction, so crash recovery is atomic — either both happen or
				// neither. No need to check current state.
				if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
					return fmt.Errorf("mark index %q write-only: %w", index.Name, err)
				}
			case IndexStateDisabled:
				if _, err := store.MarkIndexDisabled(index.Name); err != nil {
					return fmt.Errorf("mark index %q disabled: %w", index.Name, err)
				}
			}
		}
	}

	// Update store header with new metadata version and format version.
	// Matches Java's checkRebuild() which sets info.setFormatVersion(formatVersion).
	newVersion := int32(newMetaDataVersion)
	storeHeader.MetaDataversion = &newVersion
	fmtVersion := int32(formatVersionCurrent)
	storeHeader.FormatVersion = &fmtVersion
	lastUpdateTime := uint64(time.Now().UnixMilli())
	storeHeader.LastUpdateTime = &lastUpdateTime
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return fmt.Errorf("update store header after rebuild: %w", err)
	}

	return nil
}

// areAllRecordTypesSince returns true if every record type associated with
// the given index was added after oldMetaDataVersion (i.e. all have
// SinceVersion > oldMetaDataVersion). For universal indexes, checks all
// record types. Matches Java's FDBRecordStore.areAllRecordTypesSince().
func (store *FDBRecordStore) areAllRecordTypesSince(index *Index, oldMetaDataVersion int) bool {
	// Universal indexes apply to all record types.
	isUniversal := false
	for _, uIdx := range store.metaData.GetUniversalIndexes() {
		if uIdx.Name == index.Name {
			isUniversal = true
			break
		}
	}

	if isUniversal {
		for _, rt := range store.metaData.RecordTypes() {
			if rt.SinceVersion == 0 || rt.SinceVersion <= oldMetaDataVersion {
				return false
			}
		}
		return true
	}

	// Type-specific index: find which record types have it.
	found := false
	for _, rt := range store.metaData.RecordTypes() {
		for _, rtIdx := range store.metaData.GetIndexesForRecordType(rt.Name) {
			if rtIdx.Name == index.Name {
				found = true
				if rt.SinceVersion == 0 || rt.SinceVersion <= oldMetaDataVersion {
					return false
				}
			}
		}
	}
	return found
}

// checkPossiblyRebuildRecordCounts detects when the record count key expression
// has changed between metadata versions and rebuilds the counts.
// Returns true if the store header was modified (caller must persist).
// Triggers when:
//   - Current metadata has a count key but the store header has a different one (or none)
//   - Current metadata has no count key but the store header still has one
//
// Matches Java's FDBRecordStore.checkPossiblyRebuildRecordCounts().
func (store *FDBRecordStore) checkPossiblyRebuildRecordCounts(storeHeader *gen.DataStoreInfo) (bool, error) {
	currentKey := store.metaData.GetRecordCountKey()

	var needRebuild bool
	if currentKey != nil {
		// Current metadata has a count key — check if header matches.
		currentKeyProto := currentKey.ToKeyExpression()
		storedKeyProto := storeHeader.GetRecordCountKey()
		if storedKeyProto == nil || !proto.Equal(currentKeyProto, storedKeyProto) {
			needRebuild = true
		}
	} else if storeHeader.GetRecordCountKey() != nil {
		// Current metadata removed count key — clear stale data.
		needRebuild = true
	}

	if !needRebuild {
		return false, nil
	}

	// Clear existing count data. Use PrefixRange to include the exact prefix
	// key — ungrouped counts are stored at the subspace prefix itself.
	countSub := store.subspace.Sub(RecordCountKey)
	if pr, err := fdb.PrefixRange(countSub.Bytes()); err == nil {
		store.context.Transaction().ClearRange(pr)
	} else {
		store.context.Transaction().ClearRange(countSub)
	}

	// Update header with the new (or cleared) count key.
	if currentKey != nil {
		storeHeader.RecordCountKey = currentKey.ToKeyExpression()
	} else {
		storeHeader.RecordCountKey = nil
	}

	// Rebuild counts by scanning all records (only if key is set and not disabled).
	if currentKey != nil && !store.isRecordCountDisabled() {
		if err := store.rebuildRecordCounts(currentKey); err != nil {
			return false, err
		}
	}

	return true, nil
}

// rebuildRecordCounts scans all records and repopulates the count subspace.
// Uses direct SET (not atomic ADD) since we're writing from a clean state.
// Matches Java's FDBRecordStore.addRebuildRecordCountsJob().
func (store *FDBRecordStore) rebuildRecordCounts(countKey KeyExpression) error {
	ctx := context.Background()
	counts := make(map[string]int64)       // packed count key → count
	keyMap := make(map[string]tuple.Tuple) // packed → tuple (for FDB writes)

	cursor := store.ScanRecords(nil, ForwardScan())
	defer func() { _ = cursor.Close() }()

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return fmt.Errorf("scan records for count rebuild: %w", err)
		}
		if !result.HasNext() {
			break
		}
		rec := result.GetValue()
		subkeys, err := countKey.Evaluate(rec, rec.Record)
		if err != nil {
			return fmt.Errorf("evaluate count key for record: %w", err)
		}
		if len(subkeys) != 1 {
			return fmt.Errorf("count key should evaluate to single key, got %d", len(subkeys))
		}
		keyTuple := make(tuple.Tuple, len(subkeys[0]))
		for i, v := range subkeys[0] {
			keyTuple[i] = v
		}
		packed := string(keyTuple.Pack())
		counts[packed]++
		keyMap[packed] = keyTuple
	}

	countSubspace := store.subspace.Sub(RecordCountKey)
	for packed, count := range counts {
		fdbKey := countSubspace.Pack(keyMap[packed])
		store.context.Transaction().Set(fdbKey, encodeRecordCount(count))
	}

	return nil
}

// getRecordCountForRebuildPolicy returns the approximate record count
// for the IndexRebuildPolicy decision. Uses GetRecordCount if available,
// falls back to 0 (which triggers inline rebuild — safe default for stores
// without counting enabled).
func (store *FDBRecordStore) getRecordCountForRebuildPolicy() (int64, error) {
	if store.metaData.GetRecordCountKey() != nil {
		count, err := store.GetRecordCount()
		if err != nil {
			return 0, err
		}
		return count, nil
	}
	// Without counting, we can't know the count cheaply.
	// Return 0 to trigger inline rebuild (safe for small stores,
	// matches Java's lazy evaluation where count is only fetched
	// if the checker explicitly requests it).
	return 0, nil
}

// createStoreHeader creates a DataStoreInfo header for a new record store.
// Includes RecordCountKey from metadata if present, matching Java's
// checkPossiblyRebuildRecordCounts which sets it during store creation.
func createStoreHeader(metaDataVersion int32, metaData *RecordMetaData) *gen.DataStoreInfo {
	formatVersion := int32(formatVersionCurrent)
	userVersion := int32(0) // Default user version
	lastUpdateTime := uint64(time.Now().UnixMilli())

	header := &gen.DataStoreInfo{
		FormatVersion:   &formatVersion,
		MetaDataversion: &metaDataVersion,
		UserVersion:     &userVersion,
		LastUpdateTime:  &lastUpdateTime,
	}

	// Persist RecordCountKey so checkPossiblyRebuildRecordCounts doesn't trigger
	// an unnecessary full rebuild on the first reopen.
	if metaData != nil && metaData.GetRecordCountKey() != nil {
		header.RecordCountKey = metaData.GetRecordCountKey().ToKeyExpression()
		readable := gen.DataStoreInfo_READABLE
		header.RecordCountState = &readable
	}

	return header
}

// checkStoreExists checks if a store exists and returns its state
func (store *FDBRecordStore) checkStoreExists() (bool, *gen.DataStoreInfo, error) {
	// Check if the first key in the subspace exists
	begin, end := store.subspace.FDBRangeKeys()
	storeRange := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(storeRange, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		// %w, not %v: preserve the fdb.Error type so a retryable read error
		// (future_version, transaction_too_old, …) stays retryable in the
		// Transact loop rather than being flattened to a fatal string.
		return false, nil, fmt.Errorf("failed to read store range: %w", err)
	}
	if len(kvs) == 0 {
		// Store is completely empty
		return false, nil, nil
	}

	// Check if the first key is the store info header
	firstKV := kvs[0]
	expectedStoreInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})

	if !bytes.Equal(firstKV.Key, expectedStoreInfoKey) {
		// Store has data but no proper header - matches Java error
		return false, nil, &RecordStoreNoInfoButNotEmptyError{FirstKey: firstKV.Key}
	}

	// Parse the store header
	storeInfo := &gen.DataStoreInfo{}
	if err := storeInfo.UnmarshalVT(firstKV.Value); err != nil {
		return false, nil, fmt.Errorf("failed to parse store header: %v", err)
	}

	return true, storeInfo, nil
}

// writeStoreHeader writes the store header to FDB and handles cache invalidation.
// When the store is cacheable, bumps the metadata version stamp so other transactions
// will see the change. Matches Java's FDBRecordStore.updateStoreHeaderAsync().
// Caller must hold stateMu (write lock) or be in a builder path (pre-concurrent access).
func (store *FDBRecordStore) writeStoreHeader(storeInfo *gen.DataStoreInfo) error {
	oldCacheable := store.storeHeader != nil && store.storeHeader.GetCacheable()

	headerBytes, err := storeInfo.MarshalVT()
	if err != nil {
		return &RecordSerializationError{Cause: err}
	}

	storeInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})
	store.context.Transaction().Set(storeInfoKey, headerBytes)

	// Mark store state as dirty in this transaction.
	// Matches Java: context.setDirtyStoreState(true) in updateStoreHeaderAsync().
	store.context.SetDirtyStoreState(true)

	// Bump metadata version stamp when appropriate.
	// Matches Java's updateStoreHeaderAsync() cache invalidation logic.
	newCacheable := storeInfo.GetCacheable()
	if oldCacheable {
		// Old header was cacheable → always bump to invalidate cached entries.
		store.context.SetMetaDataVersionStamp()
	} else if newCacheable {
		// Transitioning to cacheable → initialize stamp if not yet set.
		stamp, _ := store.context.GetMetaDataVersionStamp()
		if stamp == nil {
			store.context.SetMetaDataVersionStamp()
		}
	}

	return nil
}

// IndexRebuildPolicy determines what state a new/changed index should be put in
// when the store is opened with updated metadata.
// Matches Java's FDBRecordStoreBase.UserVersionChecker.needRebuildIndex().
type IndexRebuildPolicy func(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState

// DefaultIndexRebuildPolicy matches Java's default behavior:
// inline rebuild (READABLE) for stores with ≤200 records or indexes on new record types,
// DISABLED otherwise (requires OnlineIndexer).
// Java constant: FDBRecordStoreBase.MAX_RECORDS_FOR_REBUILD = 200.
func DefaultIndexRebuildPolicy(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState {
	const maxRecordsForRebuild = 200
	if indexOnNewRecordTypes || recordCount <= maxRecordsForRebuild {
		return IndexStateReadable
	}
	return IndexStateDisabled
}

// WriteOnlyIfTooLargePolicy returns READABLE for small stores (inline rebuild)
// and WRITE_ONLY for larger stores. WRITE_ONLY is the production-safe choice:
// new writes maintain the index immediately, and the operator invokes
// OnlineIndexer to backfill historical data. This avoids both:
//   - READABLE: times out on large stores (single-transaction rebuild)
//   - DISABLED: index is completely ignored, new writes don't maintain it
//
// Threshold matches Java's MAX_RECORDS_FOR_REBUILD = 200.
func WriteOnlyIfTooLargePolicy(index *Index, recordCount int64, indexOnNewRecordTypes bool) IndexState {
	const maxRecordsForRebuild = 200
	if indexOnNewRecordTypes || recordCount <= maxRecordsForRebuild {
		return IndexStateReadable
	}
	return IndexStateWriteOnly
}

// AlwaysRebuildPolicy always rebuilds indexes inline.
// Matches Java's ALWAYS_READABLE_CHECKER behavior.
func AlwaysRebuildPolicy(_ *Index, _ int64, _ bool) IndexState {
	return IndexStateReadable
}

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java exactly.
type StoreBuilder struct {
	context                   *FDBRecordContext
	metaData                  *RecordMetaData
	subspace                  subspace.Subspace
	indexRebuildPolicy        IndexRebuildPolicy
	bypassFullStoreLockReason string
	storeStateCache           FDBRecordStoreStateCache // per-store override; nil = use db cache
	database                  *FDBDatabase             // for inheriting cache
	skipPossiblyRebuild       bool                     // skip checkPossiblyRebuild on open
	cachedSSKeys              *storeSubspaceKeys       // cached from getCachedSubspaceKeys; avoids sync.Map lookup per Open
	assumeAllIndexesReadable  bool                     // pre-populate empty indexStates so ensureStoreStateLoaded is a no-op
}

// NewStoreBuilder creates a new store builder
func NewStoreBuilder() *StoreBuilder {
	return &StoreBuilder{}
}

// SetContext sets the record context
func (b *StoreBuilder) SetContext(ctx *FDBRecordContext) *StoreBuilder {
	b.context = ctx
	return b
}

// SetMetaDataProvider sets the metadata
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder {
	b.metaData = metaData
	return b
}

// SetSubspace sets the subspace for this store
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder {
	b.subspace = subspace
	return b
}

// SetIndexRebuildPolicy sets the policy for rebuilding indexes during store open
// when the metadata version changes. If not set, DefaultIndexRebuildPolicy is used
// (inline rebuild for ≤200 records, DISABLED otherwise).
// Matches Java's FDBRecordStore.newBuilder().setUserVersionChecker().
func (b *StoreBuilder) SetIndexRebuildPolicy(policy IndexRebuildPolicy) *StoreBuilder {
	b.indexRebuildPolicy = policy
	return b
}

// SetBypassFullStoreLockReason sets a reason string that, if it matches the
// stored FULL_STORE lock reason exactly, allows the store to be opened despite
// the lock. This is intended for recovery operations.
// Matches Java's FDBRecordStore.Builder.setBypassFullStoreLockReason().
func (b *StoreBuilder) SetBypassFullStoreLockReason(reason string) *StoreBuilder {
	b.bypassFullStoreLockReason = reason
	return b
}

// SetStoreStateCache sets a per-store cache override. If not set, the database's
// cache is used. Matches Java's FDBRecordStore.Builder.setStoreStateCache().
func (b *StoreBuilder) SetStoreStateCache(cache FDBRecordStoreStateCache) *StoreBuilder {
	b.storeStateCache = cache
	return b
}

// SetDatabase sets the database for inheriting the store state cache.
// Matches Java's FDBRecordStore.Builder.setDatabase().
func (b *StoreBuilder) SetDatabase(db *FDBDatabase) *StoreBuilder {
	b.database = db
	return b
}

// SetSkipPossiblyRebuild disables automatic index rebuild checks during Open/CreateOrOpen.
// When set, the store will not call checkPossiblyRebuild even if the metadata version changed.
// This is used by OnlineIndexer which manages index states independently.
// Matches Java's IndexMaintenanceFilter.NONE behavior.
func (b *StoreBuilder) SetSkipPossiblyRebuild(skip bool) *StoreBuilder {
	b.skipPossiblyRebuild = skip
	return b
}

// SetAssumeAllIndexesReadable pre-populates an empty indexStates map during Build(),
// making ensureStoreStateLoaded() a complete no-op (zero FDB reads, zero lazy-load).
// Safe when CreateOrOpen ran at startup and all indexes are known to be READABLE.
// This is an explicit opt-in for maximum performance in the Build() path.
func (b *StoreBuilder) SetAssumeAllIndexesReadable(assume bool) *StoreBuilder {
	b.assumeAllIndexesReadable = assume
	return b
}

// resolveCache returns the cache to use: per-store override > database cache > pass-through.
func (b *StoreBuilder) resolveCache() FDBRecordStoreStateCache {
	if b.storeStateCache != nil {
		return b.storeStateCache
	}
	if b.database != nil && b.database.storeStateCache != nil {
		return b.database.storeStateCache
	}
	return PassThroughStoreStateCache()
}

// subspaceKeys returns the cached subspace keys, computing them lazily.
func (b *StoreBuilder) subspaceKeys() *storeSubspaceKeys {
	if b.cachedSSKeys == nil {
		b.cachedSSKeys = getCachedSubspaceKeys(b.subspace)
	}
	return b.cachedSSKeys
}

// newStore creates an FDBRecordStore from the builder's settings.
func (b *StoreBuilder) newStore() *FDBRecordStore {
	policy := b.indexRebuildPolicy
	if policy == nil {
		policy = DefaultIndexRebuildPolicy
	}
	// Use cached recordsSubspace from subspace key cache.
	recSS := b.subspaceKeys().recordsSubspace
	store := &FDBRecordStore{
		context:            b.context,
		metaData:           b.metaData,
		subspace:           b.subspace,
		recordsSubspace:    recSS,
		indexRebuildPolicy: policy,
		storeStateCache:    b.resolveCache(),
	}
	if b.assumeAllIndexesReadable {
		store.indexStates = make(map[string]IndexState)
	}
	return store
}

// validateBuilder checks that all required fields are set
func (b *StoreBuilder) validateBuilder() error {
	if b.context == nil {
		return fmt.Errorf("context is required")
	}
	if b.metaData == nil {
		return fmt.Errorf("metadata is required")
	}
	if b.subspace == nil || b.subspace.Bytes() == nil {
		return fmt.Errorf("subspace is required")
	}
	return nil
}

// Create creates a new record store, fails if store already exists
func (b *StoreBuilder) Create() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Check if store already exists
	exists, _, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, &RecordStoreAlreadyExistsError{}
	}

	// Create and write store header
	storeHeader := createStoreHeader(int32(b.metaData.Version()), b.metaData)
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return nil, err
	}
	store.storeHeader = storeHeader
	store.indexStates = make(map[string]IndexState)

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist.
// When the current metadata version is higher than the stored version,
// new indexes are automatically rebuilt inline (matching Java's checkVersion flow).
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Load store state via cache (or direct if bypassing locks).
	// Matches Java's checkVersion() which bypasses cache on full store lock bypass.
	if err := store.loadStoreState(ExistenceCheckErrorIfNotExists, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	// Validate format version is supported.
	if err := store.validateFormatVersion(store.storeHeader); err != nil {
		return nil, err
	}

	// Validate store lock state (FULL_STORE blocks open unless bypassed).
	if err := validateStoreLockState(store.storeHeader, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if !b.skipPossiblyRebuild {
		if err := store.checkPossiblyRebuild(store.storeHeader); err != nil {
			return nil, err
		}
	}

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// CreateOrOpen creates store if it doesn't exist, opens if it does (like Java).
// When opening an existing store whose metadata version is older than the
// current metadata, new indexes are automatically rebuilt inline.
// Matches Java's FDBRecordStore.checkPossiblyRebuild().
func (b *StoreBuilder) CreateOrOpen() (*FDBRecordStore, error) {
	startTime := time.Now()
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Load store state via cache (or direct).
	if err := store.loadStoreState(ExistenceCheckNone, b.bypassFullStoreLockReason); err != nil {
		return nil, err
	}

	exists := store.storeHeader != nil

	if !exists {
		// Create store header if it doesn't exist
		storeHeader := createStoreHeader(int32(b.metaData.Version()), b.metaData)
		if err := store.writeStoreHeader(storeHeader); err != nil {
			return nil, err
		}
		store.storeHeader = storeHeader
		store.indexStates = make(map[string]IndexState)
	} else {
		// Validate format version is supported.
		if err := store.validateFormatVersion(store.storeHeader); err != nil {
			return nil, err
		}
		// Validate store lock state (FULL_STORE blocks open unless bypassed).
		if err := validateStoreLockState(store.storeHeader, b.bypassFullStoreLockReason); err != nil {
			return nil, err
		}
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if exists && !b.skipPossiblyRebuild {
		if err := store.checkPossiblyRebuild(store.storeHeader); err != nil {
			return nil, err
		}
	}

	b.context.Timer().RecordSince(EventOpenStore, startTime)

	return store, nil
}

// Build returns a store without checking database state (advanced use case)
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	return b.newStore(), nil
}
