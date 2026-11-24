// Portions derived from FoundationDB Record Layer (OnlineIndexer.java,
// IndexingBase.java, OnlineIndexOperationConfig.java, IndexingByIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"fdb.dev/internal/recordlayer/tuplefast"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// unpackRangeEndBoundary converts a range-set END boundary into a scan tuple plus its high
// endpoint type. A normal boundary is a packed primary key, scanned EXCLUSIVE. A
// RANGE_INCLUSIVE-high boundary — a tuple followed by a single 0xff (Java
// TupleRange.toRange's representation, written by the typed-records preset's high bound) —
// is not itself a valid tuple; it is scanned as the stripped tuple with an INCLUSIVE high
// endpoint, covering that tuple's whole sub-range exactly as the 0xff byte does. The plain
// tuple is tried first, so an ordinary key whose pack happens to end in 0xff (e.g. int 255 =
// 0x15 0xff) is correctly treated as that key, not as a +0xff bound.
func unpackRangeEndBoundary(b []byte) (tuple.Tuple, EndpointType, error) {
	if t, err := tuplefast.Unpack(b); err == nil {
		return t, EndpointTypeRangeExclusive, nil
	}
	if n := len(b); n > 0 && b[n-1] == 0xff {
		if t, err := tuplefast.Unpack(b[:n-1]); err == nil {
			return t, EndpointTypeRangeInclusive, nil
		}
	}
	return nil, 0, fmt.Errorf("range end boundary %x is neither a tuple nor a tuple+0xff bound", b)
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
	var r buildRangeResult
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		var err error
		// Each attempt starts from a fresh result; a retry's values are stale.
		r, err = oi.buildRangeInTransaction(ctx, rtx)
		return nil, err
	})
	if err == nil {
		oi.mergeRequiredIndexes = r.mergeRequests
	}
	return r.recordsProcessed, r.hasMore, err
}

// buildRangeResult is one buildRange transaction's outcome.
type buildRangeResult struct {
	recordsProcessed int64
	hasMore          bool
	mergeRequests    []*Index
}

// buildRangeInTransaction is buildRange's body: it indexes one chunk in rtx,
// which the caller commits.
func (oi *OnlineIndexer) buildRangeInTransaction(ctx context.Context, rtx *FDBRecordContext) (buildRangeResult, error) {
	var r buildRangeResult
	started := rtx.Env().Now()

	store, err := oi.openStore(rtx)
	if err != nil {
		return r, err
	}

	if err := oi.validateBuildSession(store); err != nil {
		return r, err
	}

	// Primary index's RangeSet determines the build boundaries.
	primaryRangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

	// Find first missing range.
	missing, err := primaryRangeSet.FirstMissingRange(rtx.Transaction())
	if err != nil {
		return r, err
	}
	if missing == nil {
		r.hasMore = false
		return r, nil
	}

	rangeStart, rangeEnd, lowEp, highEp, err := unpackRecordScanBounds(missing)
	if err != nil {
		return r, err
	}

	// Scan limit+1 records: process up to limit, use the extra as continuation.
	// Idempotent indexes use SNAPSHOT reads (no conflict tracking) for better
	// throughput. Non-idempotent indexes use SERIALIZABLE to prevent double-counting.
	// Matches Java's IndexingBase.scanPropertiesWithLimits().
	scanProps := ForwardScan()
	scanProps.ExecuteProperties.ReturnedRowLimit = saturatingAdd(oi.limit, 1)
	if oi.allTargetIndexesIdempotent() {
		scanProps.ExecuteProperties.IsolationLevel = IsolationLevelSnapshot
	}

	cursor := store.ScanRecordsInRange(rangeStart, rangeEnd, lowEp, highEp, nil, scanProps)

	var scannedCount int
	var extraPK tuple.Tuple
	// reachedLimits: the last record indexed took the transaction past its
	// time or write limit, so the next record ends the range.
	var reachedLimits bool

	for rec, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			return r, iterErr
		}

		scannedCount++

		if scannedCount > oi.limit || reachedLimits {
			extraPK = rec.PrimaryKey
			break
		}

		// Count ALL scanned records, not just indexed ones.
		// Matches Java's IndexingBase.handleCursorResult() which increments
		// recordsScannedCounter for every record regardless of type filtering.
		r.recordsProcessed++

		indexed, err := oi.indexScannedRecord(store, rec)
		if err != nil {
			return r, err
		}
		if indexed {
			if reachedLimits, err = oi.transactionReachedLimits(rtx, started); err != nil {
				return r, err
			}
		}
	}

	r.hasMore, r.mergeRequests, err = oi.finishBuiltRange(store, rtx, rangeStart, missing, extraPK, r.recordsProcessed)
	if err != nil {
		return r, err
	}
	return r, nil
}

// unpackRecordScanBounds converts a RangeSetRange into TupleRange scan bounds.
func unpackRecordScanBounds(missing *RangeSetRange) (rangeStart, rangeEnd tuple.Tuple, lowEp, highEp EndpointType, err error) {
	// Convert byte boundaries to a TupleRange for record scanning. The end boundary may
	// be a tuple+0xff (RANGE_INCLUSIVE high) written by the typed-records preset, which
	// unpackRangeEndBoundary maps to an INCLUSIVE high endpoint.
	lowEp = EndpointTypeRangeInclusive
	highEp = EndpointTypeRangeExclusive
	if bytes.Equal(missing.Begin, rangeSetFirstKey) {
		lowEp = EndpointTypeTreeStart
	} else if rangeStart, err = tuplefast.Unpack(missing.Begin); err != nil {
		return nil, nil, lowEp, highEp, fmt.Errorf("unpack range start: %w", err)
	}
	if bytes.Equal(missing.End, rangeSetFinalKey) {
		highEp = EndpointTypeTreeEnd
	} else if rangeEnd, highEp, err = unpackRangeEndBoundary(missing.End); err != nil {
		return nil, nil, lowEp, highEp, fmt.Errorf("unpack range end: %w", err)
	}
	return rangeStart, rangeEnd, lowEp, highEp, nil
}

// finishBuiltRange marks the built range across all target RangeSets and updates progress.
func (oi *OnlineIndexer) finishBuiltRange(store *FDBRecordStore, rtx *FDBRecordContext, rangeStart tuple.Tuple, missing *RangeSetRange, extraPK tuple.Tuple, recordsProcessed int64) (bool, []*Index, error) {
	var hasMore bool
	// Mark progress in ALL target indexes' RangeSets.
	var rangeBeginBytes, rangeEndBytes []byte
	if rangeStart != nil {
		rangeBeginBytes = rangeStart.Pack()
	}

	if extraPK != nil {
		rangeEndBytes = extraPK.Pack()
		hasMore = true
	} else {
		// Mark up to the ORIGINAL byte boundary, not rangeEnd.Pack(): the boundary may
		// be a tuple+0xff (RANGE_INCLUSIVE high) written by the typed-records preset, and
		// the stripped rangeEnd.Pack() would drop the 0xff and invert the range.
		if !bytes.Equal(missing.End, rangeSetFinalKey) {
			rangeEndBytes = missing.End
		}
		hasMore = !bytes.Equal(missing.End, rangeSetFinalKey)
	}

	for _, idx := range oi.targetIndexes {
		rangeSet := NewIndexingRangeSet(store.subspace, idx)
		if err := insertIndexBuildRange(rangeSet, rtx.Transaction(), idx, rangeBeginBytes, rangeEndBytes); err != nil {
			return false, nil, fmt.Errorf("mark range built for %q: %w", idx.Name, err)
		}
	}

	if recordsProcessed > 0 {
		for _, idx := range oi.targetIndexes {
			store.AddBuildProgress(idx, recordsProcessed)
		}
	}

	return hasMore, store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes(), nil
}

// indexScannedRecord maintains the targets covering rec, reporting whether any did. As in Java's
// handleCursorResult, the record-type filter precedes the read conflict.
func (oi *OnlineIndexer) indexScannedRecord(store *FDBRecordStore, rec *FDBStoredRecord[proto.Message]) (bool, error) {
	var applicable []*Index
	for _, idx := range oi.targetIndexes {
		if oi.shouldIndexRecordForIndex(rec, idx) {
			applicable = append(applicable, idx)
		}
	}
	if len(applicable) == 0 {
		return false, nil
	}
	if oi.allTargetIndexesIdempotent() {
		if err := store.AddRecordReadConflict(rec.PrimaryKey); err != nil {
			return false, err
		}
	}
	for _, idx := range applicable {
		maintainer, err := store.getIndexMaintainer(idx)
		if err != nil {
			return false, fmt.Errorf("index %q get maintainer: %w", idx.Name, err)
		}
		if err := maintainer.Update(nil, rec); err != nil {
			return false, fmt.Errorf("index %q record pk=%v: %w", idx.Name, rec.PrimaryKey, err)
		}
	}
	return true, nil
}

// transactionReachedLimits is Java's IndexingBase.hadTransactionReachedLimits:
// the range's transaction is older than the transaction time limit or larger
// than the write limit, so the range commits before its next record (which
// becomes the range's exclusive end, as a record past the count limit does).
func (oi *OnlineIndexer) transactionReachedLimits(rtx *FDBRecordContext, started time.Time) (bool, error) {
	if oi.transactionTimeLimitMs > 0 && rtx.Env().Now().Sub(started).Milliseconds() > oi.transactionTimeLimitMs {
		return true, nil
	}
	if oi.maxWriteLimitBytes > 0 {
		size, err := rtx.GetApproximateTransactionSize()
		if err != nil {
			return false, err
		}
		return size > oi.maxWriteLimitBytes, nil
	}
	return false, nil
}

// buildRangeByIndex processes one chunk via the BY_INDEX strategy: scans the
// source index to find records, then feeds them to the target index maintainer.
// Range tracking uses source index entry keys (not primary keys).
// Only valid for single-target builds.
// Matches Java's IndexingByIndex.buildRangeOnly().
func (oi *OnlineIndexer) buildRangeByIndex(ctx context.Context) (int64, bool, error) {
	var r buildRangeResult
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		var err error
		// Reset on retry — previous attempt's values are stale.
		r, err = oi.buildRangeByIndexInTransaction(ctx, rtx)
		return nil, err
	})

	if err == nil {
		oi.mergeRequiredIndexes = r.mergeRequests
	}
	return r.recordsProcessed, r.hasMore, err
}

func (oi *OnlineIndexer) buildRangeByIndexInTransaction(ctx context.Context, rtx *FDBRecordContext) (buildRangeResult, error) {
	var r buildRangeResult
	started := rtx.Env().Now()

	store, err := oi.openStore(rtx)
	if err != nil {
		return r, err
	}

	if err := oi.validateBuildSession(store); err != nil {
		return r, err
	}

	// FormatVersion 10 check: non-idempotent indexes cannot be built from a source
	// index on stores with format version < CHECK_INDEX_BUILD_TYPE_DURING_UPDATE.
	// On older format versions, UpdateWhileWriteOnly uses primary key range set checks
	// which are incorrect for source-index-based builds. Both this and the
	// scannability check below are Java IndexingByIndex.buildRangeOnly's
	// validateOrThrowEx failures, which the build catcher answers by falling
	// back to a records scan.
	if store.GetFormatVersion() < formatVersionCheckIndexBuildType && !isIndexIdempotent(oi.primaryIndex()) {
		return r, oi.sourceValidationError("target index is not idempotent")
	}

	state, err := store.readIndexState(oi.sourceIndex.Name)
	if err != nil {
		return r, err
	}
	if !state.IsScannable() {
		return r, oi.sourceValidationError("source index is not scannable")
	}

	rangeSet := NewIndexingRangeSet(store.subspace, oi.primaryIndex())

	// Find first missing range.
	missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
	if err != nil {
		return r, err
	}
	if missing == nil {
		r.hasMore = false
		return r, nil
	}

	rangeStart, rangeEnd, scanRange, err := unpackSourceIndexScanRange(missing)
	if err != nil {
		return r, err
	}

	scanProps := ForwardScan()
	scanProps.ExecuteProperties.ReturnedRowLimit = saturatingAdd(oi.limit, 1)
	if oi.allTargetIndexesIdempotent() {
		scanProps.ExecuteProperties.IsolationLevel = IsolationLevelSnapshot
	}

	cursor := store.ScanIndexRecords(oi.sourceIndex.Name, scanRange, nil, scanProps)

	maintainer, mErr := store.getIndexMaintainer(oi.primaryIndex())
	if mErr != nil {
		return r, mErr
	}
	var scannedCount int
	var extraKey tuple.Tuple
	var reachedLimits bool // as in buildRange

	for indexedRec, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			return r, iterErr
		}

		scannedCount++

		if scannedCount > oi.limit || reachedLimits {
			extraKey = indexedRec.IndexEntry.Key
			break
		}

		rec := indexedRec.Record
		// Count ALL scanned records regardless of type filtering.
		// Matches Java's IndexingBase.handleCursorResult().
		r.recordsProcessed++

		if !oi.shouldIndexRecordForIndex(rec, oi.primaryIndex()) {
			continue
		}

		if oi.allTargetIndexesIdempotent() {
			if err := store.AddRecordReadConflict(rec.PrimaryKey); err != nil {
				return r, err
			}
		}
		if err := maintainer.Update(nil, rec); err != nil {
			return r, fmt.Errorf("index record pk=%v: %w", rec.PrimaryKey, err)
		}
		if reachedLimits, err = oi.transactionReachedLimits(rtx, started); err != nil {
			return r, err
		}
	}

	r.hasMore, r.mergeRequests, err = oi.finishBuiltSourceRange(store, rtx, rangeSet, rangeStart, rangeEnd, missing, extraKey, r.recordsProcessed)
	if err != nil {
		return r, err
	}
	return r, nil
}

func unpackSourceIndexScanRange(missing *RangeSetRange) (rangeStart, rangeEnd tuple.Tuple, scanRange TupleRange, err error) {
	// Convert byte boundaries to TupleRange for source index scanning.
	if !bytes.Equal(missing.Begin, rangeSetFirstKey) {
		rangeStart, err = tuplefast.Unpack(missing.Begin)
		if err != nil {
			return nil, nil, TupleRange{}, fmt.Errorf("unpack range start: %w", err)
		}
	}
	if !bytes.Equal(missing.End, rangeSetFinalKey) {
		rangeEnd, err = tuplefast.Unpack(missing.End)
		if err != nil {
			return nil, nil, TupleRange{}, fmt.Errorf("unpack range end: %w", err)
		}
	}

	scanRange = TupleRange{
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
	return rangeStart, rangeEnd, scanRange, nil
}

func (oi *OnlineIndexer) finishBuiltSourceRange(store *FDBRecordStore, rtx *FDBRecordContext, rangeSet *IndexingRangeSet, rangeStart, rangeEnd tuple.Tuple, missing *RangeSetRange, extraKey tuple.Tuple, recordsProcessed int64) (bool, []*Index, error) {
	var hasMore bool
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
		hasMore = !bytes.Equal(missing.End, rangeSetFinalKey)
	}

	err := insertIndexBuildRange(rangeSet, rtx.Transaction(), oi.primaryIndex(), rangeBeginBytes, rangeEndBytes)
	if err != nil {
		return false, nil, fmt.Errorf("mark range built: %w", err)
	}

	// Track progress: atomic ADD of records scanned.
	if recordsProcessed > 0 {
		store.AddBuildProgress(oi.primaryIndex(), recordsProcessed)
	}

	mergeRequests := store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes()
	return hasMore, mergeRequests, nil
}

// isIndexTypeIdempotent returns true if the given index type produces idempotent
// updates. Idempotent indexes can safely use SNAPSHOT reads during online builds
// because re-applying the same operation produces the same result.
// Matches Java's IndexMaintainer.isIdempotent().
func isIndexIdempotent(index *Index) bool {
	switch canonicalIndexType(index.Type) {
	case IndexTypeValue,
		IndexTypeMinEverLong, IndexTypeMaxEverLong,
		IndexTypeMinEverTuple, IndexTypeMaxEverTuple,
		IndexTypeMaxEverVersion, IndexTypeVersion,
		IndexTypePermutedMin, IndexTypePermutedMax,
		IndexTypeText, IndexTypeBitmapValue, IndexTypeMultidimensional,
		IndexTypeVector:
		// StandardIndexMaintainer.isIdempotent's true, which these do not
		// override (StandardIndexMaintainer.java:736-738), and the atomic
		// mutations AtomicMutation.isIdempotent calls idempotent. A windowed
		// VECTOR index is its delegate's (SlidingWindowIndexMaintainer.java:364).
		return true
	case IndexTypeRank, IndexTypeTimeWindowLeaderboard:
		// RankIndexMaintainer.isIdempotent and
		// TimeWindowLeaderboardIndexMaintainer.isIdempotent: not when
		// duplicates are counted.
		return !index.GetBooleanOption(IndexOptionRankCountDuplicates, false)
	case IndexTypeCount, IndexTypeCountNotNull, IndexTypeCountUpdates, IndexTypeSum:
		return false
	default:
		// A registered factory answers for its own type (Java asks the
		// maintainer's isIdempotent).
		if f, err := lookupIndexMaintainerFactory(index); err == nil && f != nil {
			if i, ok := f.(idempotentIndexMaintainerFactory); ok {
				return i.IsIdempotent(index)
			}
		}
		// A type Go does not maintain has no maintainer, and Java no
		// isIdempotent to ask. Go's own vector_spfresh index is built as
		// non-idempotent (its generations are reconciled by
		// spfreshIndexMaintainer.UpdateWhileWriteOnly).
		return false
	}
}

// allTargetIndexesIdempotent returns true if all target indexes are idempotent.
// Matches Java's IndexMaintainer.isIdempotent() per index.
func (oi *OnlineIndexer) allTargetIndexesIdempotent() bool {
	for _, idx := range oi.targetIndexes {
		if !isIndexIdempotent(idx) {
			return false
		}
	}
	return true
}

// shouldIndexRecordForIndex checks if a record should be indexed by a specific
// index, based on record type matching.
func (oi *OnlineIndexer) shouldIndexRecordForIndex(rec *FDBStoredRecord[proto.Message], idx *Index) bool {
	if len(oi.recordTypes) > 0 {
		// Preset record types override — applies to all indexes in single-target.
		//
		// RESOLVE each configured name, exactly as indexedRecordTypes does. Both
		// read the same oi.recordTypes, which SetRecordTypes takes verbatim from
		// an exported builder, and GetRecordType accepts a SQL identifier for a
		// type stored under its escaped spelling. Comparing raw here while
		// resolving there let the two disagree about the SAME configuration: the
		// indexer would report the type as in scope and then index none of its
		// records, leaving a BUILT, READABLE, EMPTY index. That is worse than an
		// empty scan -- queries answer from it and are wrong.
		for _, t := range oi.recordTypes {
			want := t
			if rt := oi.metaData.GetRecordType(t); rt != nil {
				want = rt.Name
			}
			if rec.RecordType.Name == want {
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
