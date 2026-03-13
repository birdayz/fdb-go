package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// OnlineIndexer builds indexes on existing data across multiple transactions.
// Each transaction processes a chunk of records, tracks progress via IndexingRangeSet,
// and the build resumes from where it left off if interrupted.
//
// Supports three modes:
//   - BY_RECORDS (default, single target): scans all records in primary key order
//   - BY_INDEX (single target): scans an existing readable VALUE index to find records
//   - MULTI_TARGET_BY_RECORDS: scans records once, builds multiple indexes simultaneously
//
// Matches Java's OnlineIndexer with IndexingByRecords, IndexingByIndex,
// and IndexingMultiTargetByRecords.
type OnlineIndexer struct {
	db            *FDBDatabase
	metaData      *RecordMetaData
	targetIndexes []*Index  // target indexes to build (first = primary for range tracking)
	sourceIndex   *Index    // non-nil = BY_INDEX strategy (single target only)
	subspace      subspace.Subspace
	limit         int
	recordTypes   []string // record types to index (empty = all types; not allowed with multi-target)
}

// primaryIndex returns the first target index, which drives range tracking.
func (oi *OnlineIndexer) primaryIndex() *Index {
	return oi.targetIndexes[0]
}

// isMultiTarget returns true if building multiple indexes simultaneously.
func (oi *OnlineIndexer) isMultiTarget() bool {
	return len(oi.targetIndexes) > 1
}

// OnlineIndexerBuilder constructs an OnlineIndexer.
type OnlineIndexerBuilder struct {
	indexer     OnlineIndexer
	singleMode bool // true if SetIndex was used (mutually exclusive with AddTargetIndex)
}

// NewOnlineIndexerBuilder creates a new builder.
func NewOnlineIndexerBuilder() *OnlineIndexerBuilder {
	return &OnlineIndexerBuilder{
		indexer: OnlineIndexer{
			limit: 100,
		},
	}
}

// SetDatabase sets the FDB database.
func (b *OnlineIndexerBuilder) SetDatabase(db *FDBDatabase) *OnlineIndexerBuilder {
	b.indexer.db = db
	return b
}

// SetMetaData sets the record metadata.
func (b *OnlineIndexerBuilder) SetMetaData(md *RecordMetaData) *OnlineIndexerBuilder {
	b.indexer.metaData = md
	return b
}

// SetIndex sets a single target index to build. Mutually exclusive with
// AddTargetIndex/SetTargetIndexes.
func (b *OnlineIndexerBuilder) SetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = []*Index{index}
	b.singleMode = true
	return b
}

// AddTargetIndex adds a target index for multi-target building. Mutually
// exclusive with SetIndex. Matches Java's OnlineIndexer.Builder.addTargetIndex().
func (b *OnlineIndexerBuilder) AddTargetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = append(b.indexer.targetIndexes, index)
	return b
}

// SetTargetIndexes sets multiple target indexes for multi-target building.
// Mutually exclusive with SetIndex. Matches Java's OnlineIndexer.Builder.setTargetIndexes().
func (b *OnlineIndexerBuilder) SetTargetIndexes(indexes []*Index) *OnlineIndexerBuilder {
	b.indexer.targetIndexes = indexes
	return b
}

// SetSubspace sets the store subspace.
func (b *OnlineIndexerBuilder) SetSubspace(ss subspace.Subspace) *OnlineIndexerBuilder {
	b.indexer.subspace = ss
	return b
}

// SetLimit sets the maximum number of records to process per transaction.
func (b *OnlineIndexerBuilder) SetLimit(limit int) *OnlineIndexerBuilder {
	b.indexer.limit = limit
	return b
}

// SetRecordTypes sets which record types to index. If empty, indexes all types
// that have this index defined. Not allowed with multi-target building.
func (b *OnlineIndexerBuilder) SetRecordTypes(types ...string) *OnlineIndexerBuilder {
	b.indexer.recordTypes = types
	return b
}

// SetSourceIndex sets the source index for the BY_INDEX strategy. The source
// index must be a READABLE VALUE index whose root expression does not create
// duplicates. Both source and target must apply to exactly one record type,
// and the target's type must be a superset of the source's.
// Not allowed with multi-target building.
// Matches Java's OnlineIndexer.Builder.setSourceIndex().
func (b *OnlineIndexerBuilder) SetSourceIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.sourceIndex = index
	return b
}

// Build creates the OnlineIndexer.
// Matches Java's OnlineIndexer.Builder.build(): validates, deduplicates, and sorts
// target indexes by name (the alphabetically-first index becomes the "primary"
// that drives range tracking).
func (b *OnlineIndexerBuilder) Build() (*OnlineIndexer, error) {
	if b.indexer.db == nil {
		return nil, fmt.Errorf("online indexer: database is required")
	}
	if b.indexer.metaData == nil {
		return nil, fmt.Errorf("online indexer: metadata is required")
	}
	if len(b.indexer.targetIndexes) == 0 {
		return nil, fmt.Errorf("online indexer: at least one target index is required")
	}
	if b.indexer.subspace == nil {
		return nil, fmt.Errorf("online indexer: subspace is required")
	}
	if b.indexer.limit <= 0 {
		b.indexer.limit = 100
	}

	// Deduplicate target indexes by name (matches Java's HashSet dedup).
	seen := make(map[string]bool, len(b.indexer.targetIndexes))
	deduped := make([]*Index, 0, len(b.indexer.targetIndexes))
	for _, idx := range b.indexer.targetIndexes {
		if !seen[idx.Name] {
			seen[idx.Name] = true
			deduped = append(deduped, idx)
		}
	}
	b.indexer.targetIndexes = deduped

	// Validate all target indexes exist in metadata (matches Java's OnlineIndexer.Builder).
	md := b.indexer.metaData
	for _, idx := range b.indexer.targetIndexes {
		if md.GetIndex(idx.Name) == nil {
			return nil, fmt.Errorf("online indexer: index %q not contained within specified metadata", idx.Name)
		}
	}

	// Sort target indexes by name so primary index selection is deterministic.
	// Matches Java's OnlineIndexer.Builder.validateIndexSetting():
	// targetIndexes.sort(Comparator.comparing(Index::getName))
	sort.Slice(b.indexer.targetIndexes, func(i, j int) bool {
		return b.indexer.targetIndexes[i].Name < b.indexer.targetIndexes[j].Name
	})

	isMulti := len(b.indexer.targetIndexes) > 1

	// Mutual exclusivity: SetIndex vs multi-target.
	if b.singleMode && isMulti {
		return nil, fmt.Errorf("online indexer: SetIndex may not be used when other target indexes are set")
	}

	// Multi-target restrictions (matches Java's IndexingCommon).
	if isMulti {
		if len(b.indexer.recordTypes) > 0 {
			return nil, fmt.Errorf("online indexer: preset record types not allowed with multi-target indexing")
		}
		if b.indexer.sourceIndex != nil {
			return nil, fmt.Errorf("online indexer: source index (BY_INDEX) not allowed with multi-target indexing")
		}
	}

	if b.indexer.sourceIndex != nil {
		if err := b.validateSourceIndex(); err != nil {
			return nil, err
		}
	}
	return &b.indexer, nil
}

// validateSourceIndex checks that the source index is valid for BY_INDEX building.
// Matches Java's IndexingByIndex.validateSourceAndTargetIndexes().
func (b *OnlineIndexerBuilder) validateSourceIndex() error {
	src := b.indexer.sourceIndex
	tgt := b.indexer.targetIndexes[0]
	md := b.indexer.metaData

	// Source must be a VALUE index.
	if src.Type != IndexTypeValue {
		return fmt.Errorf("online indexer: source index %q must be a VALUE index, got %q", src.Name, src.Type)
	}

	// Source root expression must not create duplicates.
	if createsDuplicates(src.RootExpression) {
		return fmt.Errorf("online indexer: source index %q root expression creates duplicates", src.Name)
	}

	// Both source and target must apply to exactly one record type.
	srcTypes := indexRecordTypes(md, src)
	tgtTypes := indexRecordTypes(md, tgt)

	if len(srcTypes) != 1 {
		return fmt.Errorf("online indexer: source index %q must apply to exactly 1 record type, got %d", src.Name, len(srcTypes))
	}
	if len(tgtTypes) != 1 {
		return fmt.Errorf("online indexer: target index %q must apply to exactly 1 record type, got %d", tgt.Name, len(tgtTypes))
	}

	// Target's record type must be a superset of source's.
	if srcTypes[0] != tgtTypes[0] {
		return fmt.Errorf("online indexer: target index type %q does not cover source index type %q", tgtTypes[0], srcTypes[0])
	}

	return nil
}

// indexRecordTypes returns the record type names that have this index defined.
// Returns all types for universal indexes.
func indexRecordTypes(md *RecordMetaData, idx *Index) []string {
	for _, uIdx := range md.GetUniversalIndexes() {
		if uIdx.Name == idx.Name {
			// Universal — applies to all types.
			var names []string
			for _, rt := range md.RecordTypes() {
				names = append(names, rt.Name)
			}
			return names
		}
	}
	var names []string
	for _, rt := range md.RecordTypes() {
		for _, rtIdx := range md.GetIndexesForRecordType(rt.Name) {
			if rtIdx.Name == idx.Name {
				names = append(names, rt.Name)
				break
			}
		}
	}
	return names
}

// BuildIndex runs the full index build: marks WRITE_ONLY, builds all records,
// then marks READABLE. Returns the number of records indexed.
// Matches Java's OnlineIndexer.buildIndex().
func (oi *OnlineIndexer) BuildIndex(ctx context.Context) (int64, error) {
	// Step 1: Mark all target indexes as WRITE_ONLY.
	if err := oi.markWriteOnly(ctx); err != nil {
		return 0, fmt.Errorf("mark write-only: %w", err)
	}

	// Step 2: Build in chunks across multiple transactions.
	buildFn := oi.buildRange
	if oi.sourceIndex != nil {
		buildFn = oi.buildRangeByIndex
	}

	var totalRecords int64
	for {
		n, hasMore, err := buildFn(ctx)
		if err != nil {
			return totalRecords, fmt.Errorf("build range: %w", err)
		}
		totalRecords += n
		if !hasMore {
			break
		}
	}

	// Step 3: Mark all target indexes as READABLE.
	if err := oi.markReadable(ctx); err != nil {
		return totalRecords, fmt.Errorf("mark readable: %w", err)
	}

	return totalRecords, nil
}

// markWriteOnly transitions all target indexes to WRITE_ONLY state.
//
// For single-target: matches Java's IndexingBase.handleIndexingState().
// For multi-target: matches Java's IndexingMultiTargetByRecords, where the
// primary index (first in list) drives resume detection and all indexes must
// be in a consistent state.
//
// Note: Java's OnlineIndexer opens stores with IndexMaintenanceFilter.NONE
// (no auto-rebuild), so it can skip READABLE indexes. Our openStore() uses
// plain Open() which may auto-rebuild, so we always proceed to WRITE_ONLY.
// TODO: Add a store builder option to skip checkPossiblyRebuild for OnlineIndexer.
func (oi *OnlineIndexer) markWriteOnly(ctx context.Context) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		newStamp := oi.buildIndexingStamp()
		primary := oi.primaryIndex()

		// Try to resume if primary is already WRITE_ONLY.
		if store.IsIndexWriteOnly(primary.Name) {
			resumed, err := oi.tryResumeWriteOnly(store, rtx, newStamp)
			if err != nil {
				return nil, err
			}
			if resumed {
				return nil, nil
			}
		}

		// Fresh start: clear all target indexes and mark WRITE_ONLY.
		for _, idx := range oi.targetIndexes {
			if _, err := store.ClearAndMarkIndexWriteOnly(idx.Name); err != nil {
				return nil, err
			}
			if err := store.SaveIndexingTypeStamp(idx, newStamp); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// tryResumeWriteOnly attempts to resume a build when the primary index is
// already WRITE_ONLY. Returns true if resume succeeded, false if a fresh
// start is needed.
func (oi *OnlineIndexer) tryResumeWriteOnly(store *FDBRecordStore, rtx *FDBRecordContext, newStamp *gen.IndexBuildIndexingStamp) (bool, error) {
	primary := oi.primaryIndex()

	savedStamp, err := store.LoadIndexingTypeStamp(primary)
	if err != nil {
		return false, err
	}

	if savedStamp != nil && proto.Equal(savedStamp, newStamp) {
		// Matching stamp on primary. For multi-target, validate all secondary
		// targets are also WRITE_ONLY (matching Java's state consistency check).
		for _, idx := range oi.targetIndexes[1:] {
			if !store.IsIndexWriteOnly(idx.Name) {
				return false, nil // Inconsistent — need fresh start.
			}
		}
		return true, nil // Resume.
	}

	if savedStamp == nil {
		// No stamp — check if any records have been scanned.
		rangeSet := NewIndexingRangeSet(store.subspace, primary)
		empty, err := rangeSet.rangeSet.IsEmpty(rtx.Transaction())
		if err != nil {
			return false, err
		}
		if empty {
			// No records scanned. Save stamp on all targets and ensure
			// secondary targets are WRITE_ONLY.
			for _, idx := range oi.targetIndexes {
				if err := store.SaveIndexingTypeStamp(idx, newStamp); err != nil {
					return false, err
				}
			}
			for _, idx := range oi.targetIndexes[1:] {
				if !store.IsIndexWriteOnly(idx.Name) {
					if _, err := store.ClearAndMarkIndexWriteOnly(idx.Name); err != nil {
						return false, err
					}
				}
			}
			return true, nil
		}
	}

	// Stamp mismatch or partially built with different method — need fresh start.
	return false, nil
}

// markReadable transitions all target indexes to READABLE (or READABLE_UNIQUE_PENDING
// for unique indexes with violations) after the build completes.
//
// Each index is marked in its own transaction so one failure doesn't block others.
// Matches Java's IndexingBase.markIndexReadable() → forEachTargetIndex() where each
// index uses getRunner().runAsync() (separate transaction per index).
func (oi *OnlineIndexer) markReadable(ctx context.Context) error {
	var firstErr error
	for _, idx := range oi.targetIndexes {
		_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rtx)
			if err != nil {
				return nil, err
			}
			_, err = store.MarkIndexReadableOrUniquePending(idx.Name)
			return nil, err
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// buildRange processes one chunk of records in a single transaction.
// Returns (recordsProcessed, hasMore, error).
//
// Uses Java's limit+1 look-ahead pattern (IndexingBase.scanPropertiesWithLimits):
// requests limit+1 records, indexes only the first limit, and uses the (limit+1)th
// record's PK as the exclusive range boundary. This prevents boundary records from
// being re-scanned in the next chunk — critical for non-idempotent indexes (COUNT, SUM).
//
// For multi-target: scans records once and updates all target index maintainers per
// record. Range tracking uses the primary index's RangeSet for reading, but inserts
// into all target indexes' RangeSets. Matches Java's IndexingMultiTargetByRecords.
func (oi *OnlineIndexer) buildRange(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		// Primary index's RangeSet determines the build boundaries.
		primaryRangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

		// Find first missing range.
		missing, err := primaryRangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		if missing == nil {
			hasMore = false
			return nil, nil
		}

		// Convert byte boundaries to TupleRange for record scanning.
		var rangeStart, rangeEnd tuple.Tuple
		if !bytes.Equal(missing.Begin, RangeSetFirstKey) {
			rangeStart, err = tuple.Unpack(missing.Begin)
			if err != nil {
				return nil, fmt.Errorf("unpack range start: %w", err)
			}
		}
		if !bytes.Equal(missing.End, RangeSetFinalKey) {
			rangeEnd, err = tuple.Unpack(missing.End)
			if err != nil {
				return nil, fmt.Errorf("unpack range end: %w", err)
			}
		}

		// Scan limit+1 records: process up to limit, use the extra as continuation.
		scanProps := ForwardScan()
		scanProps.ExecuteProperties.ReturnedRowLimit = oi.limit + 1

		lowEp := EndpointTypeRangeInclusive
		highEp := EndpointTypeRangeExclusive
		if rangeStart == nil {
			lowEp = EndpointTypeTreeStart
		}
		if rangeEnd == nil {
			highEp = EndpointTypeTreeEnd
		}

		cursor := store.ScanRecordsInRange(rangeStart, rangeEnd, lowEp, highEp, nil, scanProps)

		var scannedCount int
		var extraPK tuple.Tuple

		for rec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				extraPK = rec.PrimaryKey
				break
			}

			// Update each target index that applies to this record.
			// recordsProcessed counts records scanned (not per-index updates),
			// matching Java's recordsScannedCounter.
			indexed := false
			for _, idx := range oi.targetIndexes {
				if !oi.shouldIndexRecordForIndex(rec, idx) {
					continue
				}
				maintainer := store.getIndexMaintainer(idx)
				if err := maintainer.Update(nil, rec); err != nil {
					return nil, fmt.Errorf("index %q record pk=%v: %w", idx.Name, rec.PrimaryKey, err)
				}
				indexed = true
			}
			if indexed {
				recordsProcessed++
			}
		}

		// Mark progress in ALL target indexes' RangeSets.
		var rangeBeginBytes, rangeEndBytes []byte
		if rangeStart != nil {
			rangeBeginBytes = rangeStart.Pack()
		}

		if extraPK != nil {
			rangeEndBytes = extraPK.Pack()
			hasMore = true
		} else {
			if rangeEnd != nil {
				rangeEndBytes = rangeEnd.Pack()
			}
			hasMore = !bytes.Equal(missing.End, RangeSetFinalKey)
		}

		for _, idx := range oi.targetIndexes {
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			if _, err := rangeSet.InsertRange(rtx.Transaction(), rangeBeginBytes, rangeEndBytes, true); err != nil {
				return nil, fmt.Errorf("mark range built for %q: %w", idx.Name, err)
			}
		}

		return nil, nil
	})

	return recordsProcessed, hasMore, err
}

// buildIndexingStamp creates the IndexBuildIndexingStamp proto for this build.
//
// Single-target BY_RECORDS: method=BY_RECORDS.
// Single-target BY_INDEX: method=BY_INDEX with source index info.
// Multi-target: method=MULTI_TARGET_BY_RECORDS with sorted target index names.
//
// Matches Java's IndexingByRecords/IndexingByIndex/IndexingMultiTargetByRecords
// compileSingleTargetLegacyIndexingTypeStamp()/compileTargetIndexesLegacyIndexingTypeStamp().
func (oi *OnlineIndexer) buildIndexingStamp() *gen.IndexBuildIndexingStamp {
	if oi.sourceIndex != nil {
		return &gen.IndexBuildIndexingStamp{
			Method:                         gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
			SourceIndexSubspaceKey:         tuple.Tuple{oi.sourceIndex.SubspaceTupleKey()}.Pack(),
			SourceIndexLastModifiedVersion: proto.Int32(int32(oi.sourceIndex.LastModifiedVersion)),
		}
	}

	if len(oi.targetIndexes) == 1 {
		return &gen.IndexBuildIndexingStamp{
			Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
		}
	}

	// Multi-target: index names in stamp (already sorted by Build()).
	// Matches Java's compileTargetIndexesLegacyIndexingTypeStamp().
	names := make([]string, len(oi.targetIndexes))
	for i, idx := range oi.targetIndexes {
		names[i] = idx.Name
	}

	return &gen.IndexBuildIndexingStamp{
		Method:      gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS.Enum(),
		TargetIndex: names,
	}
}

// buildRangeByIndex processes one chunk via the BY_INDEX strategy: scans the
// source index to find records, then feeds them to the target index maintainer.
// Range tracking uses source index entry keys (not primary keys).
// Only valid for single-target builds.
// Matches Java's IndexingByIndex.buildRangeOnly().
func (oi *OnlineIndexer) buildRangeByIndex(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		// Validate source index is still scannable.
		if !store.IsIndexScannable(oi.sourceIndex.Name) {
			return nil, fmt.Errorf("online indexer: source index %q is not scannable", oi.sourceIndex.Name)
		}

		rangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

		// Find first missing range.
		missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		if missing == nil {
			hasMore = false
			return nil, nil
		}

		// Convert byte boundaries to TupleRange for source index scanning.
		var rangeStart, rangeEnd tuple.Tuple
		if !bytes.Equal(missing.Begin, RangeSetFirstKey) {
			rangeStart, err = tuple.Unpack(missing.Begin)
			if err != nil {
				return nil, fmt.Errorf("unpack range start: %w", err)
			}
		}
		if !bytes.Equal(missing.End, RangeSetFinalKey) {
			rangeEnd, err = tuple.Unpack(missing.End)
			if err != nil {
				return nil, fmt.Errorf("unpack range end: %w", err)
			}
		}

		scanRange := TupleRange{
			Low:          rangeStart,
			High:         rangeEnd,
			LowEndpoint:  EndpointTypeRangeInclusive,
			HighEndpoint: EndpointTypeRangeExclusive,
		}
		if rangeStart == nil {
			scanRange.LowEndpoint = EndpointTypeTreeStart
		}
		if rangeEnd == nil {
			scanRange.HighEndpoint = EndpointTypeTreeEnd
		}

		scanProps := ForwardScan()
		scanProps.ExecuteProperties.ReturnedRowLimit = oi.limit + 1

		cursor := store.ScanIndexRecords(oi.sourceIndex.Name, scanRange, nil, scanProps)

		maintainer := store.getIndexMaintainer(oi.primaryIndex())
		var scannedCount int
		var extraKey tuple.Tuple

		for indexedRec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				extraKey = indexedRec.IndexEntry.Key
				break
			}

			rec := indexedRec.Record

			if !oi.shouldIndexRecordForIndex(rec, oi.primaryIndex()) {
				continue
			}

			if err := maintainer.Update(nil, rec); err != nil {
				return nil, fmt.Errorf("index record pk=%v: %w", rec.PrimaryKey, err)
			}

			recordsProcessed++
		}

		var rangeBeginBytes, rangeEndBytes []byte
		if rangeStart != nil {
			rangeBeginBytes = rangeStart.Pack()
		}

		if extraKey != nil {
			rangeEndBytes = extraKey.Pack()
			hasMore = true
		} else {
			if rangeEnd != nil {
				rangeEndBytes = rangeEnd.Pack()
			}
			hasMore = !bytes.Equal(missing.End, RangeSetFinalKey)
		}

		_, err = rangeSet.InsertRange(rtx.Transaction(), rangeBeginBytes, rangeEndBytes, true)
		if err != nil {
			return nil, fmt.Errorf("mark range built: %w", err)
		}

		return nil, nil
	})

	return recordsProcessed, hasMore, err
}

// shouldIndexRecordForIndex checks if a record should be indexed by a specific
// index, based on record type matching.
func (oi *OnlineIndexer) shouldIndexRecordForIndex(rec *FDBStoredRecord[proto.Message], idx *Index) bool {
	if len(oi.recordTypes) > 0 {
		// Preset record types override — applies to all indexes in single-target.
		for _, t := range oi.recordTypes {
			if rec.RecordType.Name == t {
				return true
			}
		}
		return false
	}
	// Check if the record type has this index defined.
	for _, rtIdx := range oi.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
		if rtIdx.Name == idx.Name {
			return true
		}
	}
	for _, uIdx := range oi.metaData.GetUniversalIndexes() {
		if uIdx.Name == idx.Name {
			return true
		}
	}
	return false
}

// openStore opens an FDBRecordStore for the current transaction.
func (oi *OnlineIndexer) openStore(rtx *FDBRecordContext) (*FDBRecordStore, error) {
	return NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(oi.metaData).
		SetSubspace(oi.subspace).
		Open()
}
