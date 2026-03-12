package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// OnlineIndexer builds indexes on existing data across multiple transactions.
// Each transaction processes a chunk of records, tracks progress via IndexingRangeSet,
// and the build resumes from where it left off if interrupted.
//
// Supports two strategies:
//   - BY_RECORDS (default): scans all records in primary key order
//   - BY_INDEX: scans an existing readable VALUE index to find records
//
// Matches Java's OnlineIndexer with IndexingByRecords and IndexingByIndex.
type OnlineIndexer struct {
	db          *FDBDatabase
	metaData    *RecordMetaData
	index       *Index
	sourceIndex *Index // non-nil = BY_INDEX strategy
	subspace    subspace.Subspace
	limit       int
	recordTypes []string // record types to index (empty = all types for this index)
}

// OnlineIndexerBuilder constructs an OnlineIndexer.
type OnlineIndexerBuilder struct {
	indexer OnlineIndexer
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

// SetIndex sets the target index to build.
func (b *OnlineIndexerBuilder) SetIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.index = index
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
// that have this index defined.
func (b *OnlineIndexerBuilder) SetRecordTypes(types ...string) *OnlineIndexerBuilder {
	b.indexer.recordTypes = types
	return b
}

// SetSourceIndex sets the source index for the BY_INDEX strategy. The source
// index must be a READABLE VALUE index whose root expression does not create
// duplicates. Both source and target must apply to exactly one record type,
// and the target's type must be a superset of the source's.
// Matches Java's OnlineIndexer.Builder.setSourceIndex().
func (b *OnlineIndexerBuilder) SetSourceIndex(index *Index) *OnlineIndexerBuilder {
	b.indexer.sourceIndex = index
	return b
}

// Build creates the OnlineIndexer.
func (b *OnlineIndexerBuilder) Build() (*OnlineIndexer, error) {
	if b.indexer.db == nil {
		return nil, fmt.Errorf("online indexer: database is required")
	}
	if b.indexer.metaData == nil {
		return nil, fmt.Errorf("online indexer: metadata is required")
	}
	if b.indexer.index == nil {
		return nil, fmt.Errorf("online indexer: index is required")
	}
	if b.indexer.subspace == nil {
		return nil, fmt.Errorf("online indexer: subspace is required")
	}
	if b.indexer.limit <= 0 {
		b.indexer.limit = 100
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
	tgt := b.indexer.index
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
// Returns nil for universal indexes.
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
	// Step 1: Mark index as WRITE_ONLY.
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

	// Step 3: Mark index as READABLE.
	if err := oi.markReadable(ctx); err != nil {
		return totalRecords, fmt.Errorf("mark readable: %w", err)
	}

	return totalRecords, nil
}

// markWriteOnly transitions the index to WRITE_ONLY state and saves the
// BY_RECORDS indexing type stamp. Matches Java's IndexingBase.handleIndexingState()
// which calls clearAndMarkIndexWriteOnly + setIndexingTypeOrThrow.
func (oi *OnlineIndexer) markWriteOnly(ctx context.Context) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.ClearAndMarkIndexWriteOnly(oi.index.Name)
		if err != nil {
			return nil, err
		}
		// Save the indexing type stamp. clearAndMarkIndexWriteOnly already cleared
		// any existing stamp (via clearIndexData), so this is always a fresh write.
		// Matches Java's compileSingleTargetLegacyIndexingTypeStamp().
		stamp := oi.buildIndexingStamp()
		return nil, store.SaveIndexingTypeStamp(oi.index, stamp)
	})
	return err
}

// markReadable transitions the index to READABLE (or READABLE_UNIQUE_PENDING for
// unique indexes with violations) after the build completes.
// Matches Java's IndexingBase.markIndexReadable() which calls
// store.markIndexReadableOrUniquePending() when the policy allows.
func (oi *OnlineIndexer) markReadable(ctx context.Context) error {
	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		_, err = store.MarkIndexReadableOrUniquePending(oi.index.Name)
		return nil, err
	})
	return err
}

// buildRange processes one chunk of records in a single transaction.
// Returns (recordsProcessed, hasMore, error).
//
// Uses Java's limit+1 look-ahead pattern (IndexingBase.scanPropertiesWithLimits):
// requests limit+1 records, indexes only the first limit, and uses the (limit+1)th
// record's PK as the exclusive range boundary. This prevents boundary records from
// being re-scanned in the next chunk — critical for non-idempotent indexes (COUNT, SUM).
//
// Also tracks lastScannedPK across ALL records (not just indexed ones), so type-filtered
// records still advance the scan position and don't cause the build to incorrectly
// mark the entire remaining range as complete.
func (oi *OnlineIndexer) buildRange(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}

		rangeSet := NewIndexingRangeSet(store.subspace, oi.index)

		// Find first missing range.
		missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		if missing == nil {
			// All done.
			hasMore = false
			return nil, nil
		}

		// Convert byte boundaries to TupleRange for record scanning.
		// Java: Tuple.fromBytes(range.begin) unless isFirstKey/isFinalKey.
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
		// Matches Java's IndexingBase.scanPropertiesWithLimits().
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

		// Process each record: evaluate index and write entries.
		// Track scannedCount across ALL records (including type-filtered ones)
		// so that filtered-out records still advance the scan position.
		maintainer := store.getIndexMaintainer(oi.index)
		var scannedCount int
		var extraPK tuple.Tuple // PK of the (limit+1)th record (look-ahead, not indexed)

		for rec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				// This is the extra look-ahead record — don't index it.
				// Its PK becomes the exclusive range boundary.
				extraPK = rec.PrimaryKey
				break
			}

			if !oi.shouldIndexRecord(rec) {
				continue
			}

			// Insert index entries (oldRecord=nil, newRecord=rec).
			if err := maintainer.Update(nil, rec); err != nil {
				return nil, fmt.Errorf("index record pk=%v: %w", rec.PrimaryKey, err)
			}

			recordsProcessed++
		}

		// Mark progress in the RangeSet.
		var rangeBeginBytes, rangeEndBytes []byte
		if rangeStart != nil {
			rangeBeginBytes = rangeStart.Pack()
		}

		if extraPK != nil {
			// Got limit+1 records — more remain. Mark up to (exclusive) the
			// look-ahead record's PK. It will be the start of the next chunk.
			rangeEndBytes = extraPK.Pack()
			hasMore = true
		} else {
			// Cursor returned < limit+1 records — source exhausted in this range.
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

// buildIndexingStamp creates the IndexBuildIndexingStamp proto for this build.
// BY_INDEX stamps include the source index's subspace key and last modified version
// so that resume can validate the source index hasn't changed.
// Matches Java's IndexingByIndex.compileSingleTargetLegacyIndexingTypeStamp().
func (oi *OnlineIndexer) buildIndexingStamp() *gen.IndexBuildIndexingStamp {
	if oi.sourceIndex == nil {
		return &gen.IndexBuildIndexingStamp{
			Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
		}
	}
	return &gen.IndexBuildIndexingStamp{
		Method:                         gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
		SourceIndexSubspaceKey:         tuple.Tuple{oi.sourceIndex.SubspaceTupleKey()}.Pack(),
		SourceIndexLastModifiedVersion: proto.Int32(int32(oi.sourceIndex.LastModifiedVersion)),
	}
}

// buildRangeByIndex processes one chunk via the BY_INDEX strategy: scans the
// source index to find records, then feeds them to the target index maintainer.
// Range tracking uses source index entry keys (not primary keys).
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

		rangeSet := NewIndexingRangeSet(store.subspace, oi.index)

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
		// These are source index entry keys, not primary keys.
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

		// Build TupleRange for the source index scan.
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

		// Scan source index with limit+1 look-ahead.
		scanProps := ForwardScan()
		scanProps.ExecuteProperties.ReturnedRowLimit = oi.limit + 1

		cursor := store.ScanIndexRecords(oi.sourceIndex.Name, scanRange, nil, scanProps)

		maintainer := store.getIndexMaintainer(oi.index)
		var scannedCount int
		var extraKey tuple.Tuple // Key of the (limit+1)th entry (look-ahead)

		for indexedRec, iterErr := range Seq2(cursor, ctx) {
			if iterErr != nil {
				return nil, iterErr
			}

			scannedCount++

			if scannedCount > oi.limit {
				// Look-ahead entry — don't index. Its key becomes the boundary.
				extraKey = indexedRec.IndexEntry.Key
				break
			}

			rec := indexedRec.Record

			// Type filter: source and target index must cover same type,
			// but record loaded from index might not match if index is shared.
			if !oi.shouldIndexRecord(rec) {
				continue
			}

			if err := maintainer.Update(nil, rec); err != nil {
				return nil, fmt.Errorf("index record pk=%v: %w", rec.PrimaryKey, err)
			}

			recordsProcessed++
		}

		// Mark progress in the RangeSet using source index entry keys.
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

// shouldIndexRecord checks if a record matches the index's record types.
func (oi *OnlineIndexer) shouldIndexRecord(rec *FDBStoredRecord[proto.Message]) bool {
	if len(oi.recordTypes) == 0 {
		// Check if the record type has this index defined.
		for _, idx := range oi.metaData.GetIndexesForRecordType(rec.RecordType.Name) {
			if idx.Name == oi.index.Name {
				return true
			}
		}
		// Check universal indexes.
		for _, idx := range oi.metaData.GetUniversalIndexes() {
			if idx.Name == oi.index.Name {
				return true
			}
		}
		return false
	}
	for _, t := range oi.recordTypes {
		if rec.RecordType.Name == t {
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

