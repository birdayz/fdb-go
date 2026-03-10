package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
	maintainer := store.getIndexMaintainer(index)

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
// Matches Java's FormatVersion.validateFormatVersion().
func (store *FDBRecordStore) validateFormatVersion(storeHeader *gen.DataStoreInfo) error {
	storedVersion := storeHeader.GetFormatVersion()
	if storedVersion > FormatVersionCurrent {
		return fmt.Errorf("unsupported format version %d (max supported: %d)", storedVersion, FormatVersionCurrent)
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

	// Find indexes added since the old version.
	indexesToBuild := store.metaData.GetIndexesToBuildSince(oldMetaDataVersion)
	if len(indexesToBuild) > 0 {
		// Get record count for the policy decision (lazy in Java, eager here).
		recordCount, err := store.getRecordCountForRebuildPolicy()
		if err != nil {
			return fmt.Errorf("check record count for rebuild: %w", err)
		}

		for _, index := range indexesToBuild {
			// TODO: detect indexOnNewRecordTypes (index covers only record types
			// added in this same version bump). For now, conservatively false.
			desiredState := store.indexRebuildPolicy(index, recordCount, false)

			switch desiredState {
			case IndexStateReadable:
				if err := store.RebuildIndex(index); err != nil {
					return fmt.Errorf("auto-rebuild index %q on metadata version change (%d -> %d): %w",
						index.Name, oldMetaDataVersion, newMetaDataVersion, err)
				}
			case IndexStateWriteOnly:
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
	fmtVersion := int32(FormatVersionCurrent)
	storeHeader.FormatVersion = &fmtVersion
	lastUpdateTime := uint64(time.Now().UnixMilli())
	storeHeader.LastUpdateTime = &lastUpdateTime
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return fmt.Errorf("update store header after rebuild: %w", err)
	}

	return nil
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
	counts := make(map[string]int64) // packed count key → count
	keyMap := make(map[string]tuple.Tuple) // packed → tuple (for FDB writes)

	cursor := store.ScanRecords(nil, ForwardScan())
	defer cursor.Close()

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return fmt.Errorf("scan records for count rebuild: %w", err)
		}
		if !result.HasNext() {
			break
		}
		rec := result.GetValue()
		subkeys, err := countKey.Evaluate(rec.Record)
		if err != nil || len(subkeys) != 1 {
			continue
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

// createStoreHeader creates a DataStoreInfo header for a new record store
func createStoreHeader(metaDataVersion int32) *gen.DataStoreInfo {
	formatVersion := int32(FormatVersionCurrent)
	userVersion := int32(0) // Default user version
	lastUpdateTime := uint64(time.Now().UnixMilli())

	return &gen.DataStoreInfo{
		FormatVersion:   &formatVersion,
		MetaDataversion: &metaDataVersion,
		UserVersion:     &userVersion,
		LastUpdateTime:  &lastUpdateTime,
	}
}

// checkStoreExists checks if a store exists and returns its state
func (store *FDBRecordStore) checkStoreExists() (bool, *gen.DataStoreInfo, error) {
	// Check if the first key in the subspace exists
	begin, end := store.subspace.FDBRangeKeys()
	storeRange := fdb.KeyRange{Begin: begin, End: end}

	kvs, err := store.context.Transaction().GetRange(storeRange, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return false, nil, fmt.Errorf("failed to read store range: %v", err)
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
		return false, nil, ErrRecordStoreNoInfoButNotEmpty
	}

	// Parse the store header
	storeInfo := &gen.DataStoreInfo{}
	if err := proto.Unmarshal(firstKV.Value, storeInfo); err != nil {
		return false, nil, fmt.Errorf("failed to parse store header: %v", err)
	}

	return true, storeInfo, nil
}

// writeStoreHeader writes the store header to FDB
func (store *FDBRecordStore) writeStoreHeader(storeInfo *gen.DataStoreInfo) error {
	headerBytes, err := proto.Marshal(storeInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal store header: %v", err)
	}

	storeInfoKey := store.subspace.Pack(tuple.Tuple{StoreInfoKey})
	store.context.Transaction().Set(storeInfoKey, headerBytes)
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

// AlwaysRebuildPolicy always rebuilds indexes inline.
// Matches Java's ALWAYS_READABLE_CHECKER behavior.
func AlwaysRebuildPolicy(_ *Index, _ int64, _ bool) IndexState {
	return IndexStateReadable
}

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java exactly.
type StoreBuilder struct {
	context            *FDBRecordContext
	metaData           *RecordMetaData
	subspace           subspace.Subspace
	indexRebuildPolicy IndexRebuildPolicy
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

// newStore creates an FDBRecordStore from the builder's settings.
func (b *StoreBuilder) newStore() *FDBRecordStore {
	policy := b.indexRebuildPolicy
	if policy == nil {
		policy = DefaultIndexRebuildPolicy
	}
	return &FDBRecordStore{
		context:            b.context,
		metaData:           b.metaData,
		subspace:           b.subspace,
		indexRebuildPolicy: policy,
	}
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
		return nil, ErrRecordStoreAlreadyExists
	}

	// Create and write store header
	storeHeader := createStoreHeader(int32(b.metaData.Version()))
	if err := store.writeStoreHeader(storeHeader); err != nil {
		return nil, err
	}
	store.storeHeader = storeHeader
	store.indexStates = make(map[string]IndexState)

	return store, nil
}

// Open opens an existing record store, fails if store doesn't exist.
// When the current metadata version is higher than the stored version,
// new indexes are automatically rebuilt inline (matching Java's checkVersion flow).
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Verify store exists and has proper header
	exists, storeHeader, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrRecordStoreDoesNotExist
	}
	store.storeHeader = storeHeader

	// Validate format version is supported.
	// Matches Java's FormatVersion.validateFormatVersion().
	if err := store.validateFormatVersion(storeHeader); err != nil {
		return nil, err
	}

	if err := store.loadIndexStates(); err != nil {
		return nil, err
	}

	// Check if metadata has evolved — rebuild new indexes if needed.
	if err := store.checkPossiblyRebuild(storeHeader); err != nil {
		return nil, err
	}

	return store, nil
}

// CreateOrOpen creates store if it doesn't exist, opens if it does (like Java).
// When opening an existing store whose metadata version is older than the
// current metadata, new indexes are automatically rebuilt inline.
// Matches Java's FDBRecordStore.checkPossiblyRebuild().
func (b *StoreBuilder) CreateOrOpen() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	store := b.newStore()

	// Check if store exists
	exists, storeHeader, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}

	if !exists {
		// Create store header if it doesn't exist
		storeHeader = createStoreHeader(int32(b.metaData.Version()))
		if err := store.writeStoreHeader(storeHeader); err != nil {
			return nil, err
		}
		store.indexStates = make(map[string]IndexState)
	} else {
		// Validate format version is supported.
		if err := store.validateFormatVersion(storeHeader); err != nil {
			return nil, err
		}
		if err := store.loadIndexStates(); err != nil {
			return nil, err
		}
	}
	store.storeHeader = storeHeader

	// Check if metadata has evolved — rebuild new indexes if needed.
	if exists {
		if err := store.checkPossiblyRebuild(storeHeader); err != nil {
			return nil, err
		}
	}

	return store, nil
}

// Build returns a store without checking database state (advanced use case)
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if err := b.validateBuilder(); err != nil {
		return nil, err
	}

	return b.newStore(), nil
}
