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
	"errors"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
)

// isTypeStampBlocked returns true if the stamp has an active block.
// Matches Java's IndexingBase.isTypeStampBlocked().
//
// now comes from the DST clock seam, not time.Now. The WRITE side of this field already does
// (rtx.Env().Now().Add(ttl) where the stamp's expiry is minted), so a wall-clock read compares
// a simulated timestamp against real time — and the simulated clock starts at a fixed epoch far
// in the past, so under a sim env every expiry it wrote reads back as ALREADY EXPIRED. The
// branch a simulation exercises is then the opposite of the branch it wrote, which is worse than
// no coverage: the run looks like it tested the blocked path and tested the unblocked one.
func isTypeStampBlocked(stamp *metadatapb.IndexBuildIndexingStamp, now time.Time) bool {
	if !stamp.GetBlock() {
		return false
	}
	expiry := stamp.GetBlockExpireEpochMilliSeconds()
	if expiry == 0 {
		return true // permanent block
	}
	return expiry > uint64(now.UnixMilli())
}

// areSimilarStamps returns true if two stamps differ only in block-related fields.
// Matches Java's IndexingBase.areSimilar().
func areSimilarStamps(a, b *metadatapb.IndexBuildIndexingStamp) bool {
	if proto.Equal(a, b) {
		return true
	}
	return proto.Equal(blocklessStampOf(a), blocklessStampOf(b))
}

// blocklessStampOf returns a copy of the stamp with all block fields cleared.
// Matches Java's IndexingBase.blocklessStampOf().
func blocklessStampOf(stamp *metadatapb.IndexBuildIndexingStamp) *metadatapb.IndexBuildIndexingStamp {
	clone := proto.Clone(stamp).(*metadatapb.IndexBuildIndexingStamp)
	clone.Block = nil
	clone.BlockID = nil
	clone.BlockExpireEpochMilliSeconds = nil
	return clone
}

// IndexBuildState reports the current state of an index build.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.IndexBuildState.
type IndexBuildState struct {
	State          IndexState
	RecordsScanned *int64 // nil if not WRITE_ONLY or not tracked
	RecordsInTotal *int64 // nil if no COUNT index available
}

// LoadIndexBuildState loads the build state for an index within an open store.
// Matches Java's IndexBuildState.loadIndexBuildStateAsync().
func LoadIndexBuildState(store *FDBRecordStore, index *Index) (*IndexBuildState, error) {
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return nil, fmt.Errorf("load build state: %w", err)
	}
	if !state.IsWriteOnly() {
		return &IndexBuildState{State: state}, nil
	}

	scanned, err := store.LoadBuildProgress(index)
	if err != nil {
		return nil, fmt.Errorf("load build state: %w", err)
	}

	result := &IndexBuildState{
		State:          state,
		RecordsScanned: &scanned,
	}

	// Total record count, from the maintained counters or from a universal COUNT
	// index. Java catches exactly AggregateFunctionNotSupportedException here —
	// "very likely it is because there is no suitable COUNT type index defined" —
	// and reports an unknown total (IndexBuildState.java:76-82). Every other failure
	// propagates: a read error is not "no COUNT index".
	total, err := store.GetRecordCount()
	switch {
	case err == nil:
		result.RecordsInTotal = &total
	case errors.As(err, new(*AggregateFunctionNotSupportedError)):
		// RecordsInTotal stays nil.
	default:
		return nil, fmt.Errorf("load build state: %w", err)
	}

	return result, nil
}

// buildIndexingStamp creates the IndexBuildIndexingStamp proto for this build.
//
// Single-target BY_RECORDS: method=BY_RECORDS.
// Single-target BY_INDEX: method=BY_INDEX with source index info.
// Multi-target: method=MULTI_TARGET_BY_RECORDS with sorted target index names.
//
// Matches Java's IndexingByRecords/IndexingByIndex/IndexingMultiTargetByRecords
// compileSingleTargetLegacyIndexingTypeStamp()/compileTargetIndexesLegacyIndexingTypeStamp().
func (oi *OnlineIndexer) buildIndexingStamp() *metadatapb.IndexBuildIndexingStamp {
	if oi.buildsByIndex() {
		return &metadatapb.IndexBuildIndexingStamp{
			Method:                         metadatapb.IndexBuildIndexingStamp_BY_INDEX.Enum(),
			SourceIndexSubspaceKey:         tuple.Tuple{oi.sourceIndex.SubspaceTupleKey()}.Pack(),
			SourceIndexLastModifiedVersion: proto.Int32(int32(oi.sourceIndex.LastModifiedVersion)),
		}
	}

	if len(oi.targetIndexes) == 1 && !oi.buildsMutually() {
		return &metadatapb.IndexBuildIndexingStamp{
			Method: metadatapb.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
		}
	}

	// Multi-target or mutual: index names in stamp (already sorted by Build()).
	// Matches Java's compileTargetIndexesLegacyIndexingTypeStamp().
	names := make([]string, len(oi.targetIndexes))
	for i, idx := range oi.targetIndexes {
		names[i] = idx.Name
	}

	method := metadatapb.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS
	if oi.buildsMutually() {
		method = metadatapb.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS
	}

	return &metadatapb.IndexBuildIndexingStamp{
		Method:      method.Enum(),
		TargetIndex: names,
	}
}

// markWriteOnly runs the session's state transaction (prepareIndexingState):
// Java IndexingBase.handleStateAndDoBuildIndexAsync's first transaction, where
// the primary index's state and the policy decide whether the session builds
// (marking every target WRITE_ONLY), only publishes, or leaves the index alone.
// The primary (first by name) drives the decision and the followers must agree
// with it.
//
// The store is opened through the ordinary open path, whose metadata
// reconciliation (checkPossiblyRebuild) may itself rebuild or disable indexes
// before the session reads their states. checkOpenHeartbeats runs first for
// that reason.
func (oi *OnlineIndexer) markWriteOnly(ctx context.Context) (indexingSessionStart, error) {
	oi.admittedHeartbeat = nil
	oi.retiredBuildTargets = nil
	var start indexingSessionStart
	result, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStoreWithPreflight(rtx, func(store *FDBRecordStore) error {
			// Reject before the session commits any clearing or state transitions.
			for _, index := range oi.targetIndexes {
				if err := store.checkIndexCollation(index); err != nil {
					return err
				}
			}
			if oi.sourceIndex != nil {
				if err := store.checkIndexCollation(oi.sourceIndex); err != nil {
					return err
				}
			}
			return oi.checkOpenHeartbeats(store)
		})
		if err != nil {
			return nil, err
		}
		queued, attemptStart, err := oi.prepareIndexingState(store)
		start = attemptStart
		return queued, err
	})
	if err != nil {
		return 0, err
	}
	// Never publish a target set derived from an attempt that did not commit.
	oi.queuedIndexes = result.([]*Index)
	if start == indexingSessionBuild {
		oi.admittedHeartbeat = oi.sessionHeartbeat
	}
	return start, nil
}

// setIndexingTypeOrThrow implements Java's IndexingBase.setIndexingTypeOrThrow().
// It validates whether the new stamp can be written, considering the saved stamp,
// block state, policy, and build method conversion rules.
func (oi *OnlineIndexer) setIndexingTypeOrThrow(store *FDBRecordStore, continuedBuild bool, newStamp *metadatapb.IndexBuildIndexingStamp) error {
	for _, idx := range oi.targetIndexes {
		if err := oi.setIndexingTypeOrThrowForIndex(store, continuedBuild, idx, newStamp); err != nil {
			return err
		}
	}
	return nil
}

// setIndexingTypeOrThrowForIndex is the per-index stamp resolution logic.
// Matches Java's IndexingBase.setIndexingTypeOrThrow(store, continuedBuild, index, newStamp).
func (oi *OnlineIndexer) setIndexingTypeOrThrowForIndex(store *FDBRecordStore, continuedBuild bool, index *Index, newStamp *metadatapb.IndexBuildIndexingStamp) error {
	policy := oi.policy
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return err
	}
	if oi.mutual && state.IsWriteOnlyWithQueue() {
		return &RecordCoreError{Message: "Mutual indexing cannot continue a pending write queue index build", IndexName: index.Name}
	}
	forceStampOverwrite := oi.enforcedStampOverwrite || (policy != nil && policy.ForceStampOverwrite)

	// Step 1: forceStampOverwrite + fresh session = no questions asked.
	if forceStampOverwrite && !continuedBuild {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 2: Load saved stamp.
	savedStamp, err := store.LoadIndexingTypeStamp(index)
	if err != nil {
		return err
	}

	// Step 3: No saved stamp.
	if savedStamp == nil {
		if continuedBuild && newStamp.GetMethod() != metadatapb.IndexBuildIndexingStamp_BY_RECORDS {
			// Backward compatibility: maybe continuing an old BY_RECORDS session that
			// didn't write stamps. Check if any records have been scanned.
			// Matches Java's throwAsByRecordsUnlessNoRecordWasScanned().
			if !isWriteOnlyButNoRecordScanned(store, store.context, index) {
				// Records were scanned under old BY_RECORDS — reject non-BY_RECORDS takeover.
				fakeSavedStamp := &metadatapb.IndexBuildIndexingStamp{
					Method: metadatapb.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				return oi.newPartlyBuiltError(fakeSavedStamp, newStamp, index,
					"this index was partly built by records (no stamp)")
			}
		}
		// Fresh session or BY_RECORDS or no records scanned: write stamp.
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 4: Exact match — nothing to do.
	if proto.Equal(savedStamp, newStamp) {
		return nil
	}

	// Step 5: Check if blocked.
	if isTypeStampBlocked(savedStamp, store.context.Env().Now()) {
		if policy == nil || !policy.ShouldAllowUnblock(savedStamp.GetBlockID()) {
			return oi.newPartlyBuiltError(savedStamp, newStamp, index,
				"this index was partly built, and blocked")
		}
	}

	// Step 6: Similar stamps (differ only in block fields).
	if areSimilarStamps(newStamp, savedStamp) {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 7: Type conversion allowed?
	if continuedBuild && policy != nil && policy.ShouldAllowTypeConversionContinue(newStamp, savedStamp) {
		return store.SaveIndexingTypeStamp(index, newStamp)
	}

	// Step 8: forceStampOverwrite + continued build — allow if no records scanned.
	if forceStampOverwrite {
		if isWriteOnlyButNoRecordScanned(store, store.context, index) {
			return store.SaveIndexingTypeStamp(index, newStamp)
		}
		return oi.newPartlyBuiltError(savedStamp, newStamp, index,
			"this index was partly built by another method")
	}

	// Step 9: Fall through — mismatch, not allowed.
	return oi.newPartlyBuiltError(savedStamp, newStamp, index,
		"this index was partly built by another method")
}

// isWriteOnlyButNoRecordScanned returns true if the index is WRITE_ONLY with a
// completely empty range set (no records have been scanned yet).
// Matches Java's IndexingBase.isWriteOnlyButNoRecordScanned().
func isWriteOnlyButNoRecordScanned(store *FDBRecordStore, rtx *FDBRecordContext, index *Index) bool {
	rangeSet := NewIndexingRangeSet(store.subspace, index)
	missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
	if err != nil {
		return false
	}
	if missing == nil {
		return false // fully built — no missing ranges
	}
	// Empty if the first missing range spans the entire key space.
	return bytes.Equal(missing.Begin, rangeSetFirstKey) && bytes.Equal(missing.End, rangeSetFinalKey)
}

// newPartlyBuiltError creates a PartlyBuiltError with stamp descriptions.
func (oi *OnlineIndexer) newPartlyBuiltError(savedStamp, expectedStamp *metadatapb.IndexBuildIndexingStamp, index *Index, msg string) *PartlyBuiltError {
	return &PartlyBuiltError{
		IndexName:     index.Name,
		SavedStamp:    stampToString(savedStamp),
		ExpectedStamp: stampToString(expectedStamp),
		Message:       msg,
		Saved:         savedStamp,
		Expected:      expectedStamp,
		IndexerID:     oi.indexerID,
		IndexVersion:  index.LastModifiedVersion,
	}
}

// stampToString returns a human-readable representation of a stamp.
func stampToString(stamp *metadatapb.IndexBuildIndexingStamp) string {
	if stamp == nil {
		return "<nil>"
	}
	return stamp.GetMethod().String()
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
		if oi.retiredBuildTargets[idx.Name] {
			continue
		}
		var err error
		attempts := max(int64(1), oi.policy.GetPendingWriteQueueIndexesMaxDrainAttempts())
		queuedInSession := false
		for _, queued := range oi.queuedIndexes {
			if queued.Name == idx.Name {
				queuedInSession = true
				break
			}
		}
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			if queuedInSession {
				err = oi.drainPendingIndexWritesForIndex(ctx, idx, oi.sessionHeartbeat)
			}
			if err == nil {
				err = oi.mergeRequestedIndexes(ctx)
			}
			if err == nil {
				_, err = oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := oi.openStore(rtx)
					if err != nil {
						return nil, err
					}
					// Java IndexingBase.markIndexReadableForIndex (IndexingBase.java:322-326)
					// picks between the two marks on the policy, NOT unconditionally:
					//   policy.shouldAllowUniquePendingState(store)
					//       ? store.markIndexReadableOrUniquePending(index)
					//       : store.markIndexReadable(index);
					// The default policy answers false, so the default outcome of a build that
					// found duplicates is a failed build, not a quietly pending index.
					if oi.shouldAllowUniquePendingState(store) {
						if _, err = store.MarkIndexReadableOrUniquePending(idx.Name); err != nil {
							return nil, err
						}
					} else if _, err = store.MarkIndexReadable(idx.Name); err != nil {
						return nil, err
					}
					// Once the index is readable there is no need for the per-build bookkeeping.
					// Matches Java's IndexingBase: erase scanned-records/type-stamp/heartbeats in
					// the same transaction as the mark (eraseAllIndexingDataButTheLockAndRangeSet),
					// for both READABLE and READABLE_UNIQUE_PENDING.
					return nil, store.eraseAllIndexingDataButTheLockAndRangeSet(idx)
				})
			}
			attempts--
			// A non-empty queue is worth another attempt only when this session
			// drains it, as Java re-drains its queued indexes. A MARK_READABLE
			// session publishes a WRITE_ONLY_WITH_QUEUE index it never drained;
			// Java's store erases the queue as it marks the index readable, losing
			// the queued writes, while Go's MarkIndexReadable refuses a non-empty
			// queue with PendingWrites. Retrying that refusal would only repeat it,
			// so it is returned at once.
			var notBuilt *IndexNotBuiltError
			var conflict fdb.Error
			retry := (errors.As(err, &notBuilt) && notBuilt.PendingWrites && queuedInSession) || (errors.As(err, &conflict) && conflict.Code == 1020)
			if err == nil || attempts <= 0 || !retry {
				break
			}
			err = nil
		}
		if err == nil {
			oi.retireBuildTarget(idx)
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// BlockIndex sets the block flag on the indexing stamp for all target indexes.
// A blocked stamp prevents other indexers from continuing the build.
// blockID is an optional identifier for the block reason.
// ttl is the time-to-live for the block; zero means permanent.
// Matches Java's IndexingBase.performIndexingStampOperation(BLOCK, ...).
func (oi *OnlineIndexer) BlockIndex(ctx context.Context, blockID string, ttl time.Duration) error {
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil {
				continue
			}
			stamp.Block = proto.Bool(true)
			if blockID != "" {
				stamp.BlockID = proto.String(blockID)
			} else {
				stamp.BlockID = proto.String("")
			}
			if ttl > 0 {
				// Persisted lease-expiry: route through the context clock seam (RFC-199
				// Tier 0) so a simulation run writes a reproducible expiry. A nil env
				// (production) reads the wall clock — byte-identical to the prior time.Now().
				stamp.BlockExpireEpochMilliSeconds = proto.Uint64(uint64(rtx.Env().Now().Add(ttl).UnixMilli()))
			} else {
				stamp.BlockExpireEpochMilliSeconds = nil
			}
			if err := store.SaveIndexingTypeStamp(idx, stamp); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// UnblockIndex clears the block flag on the indexing stamp for all target indexes.
// If blockID is non-empty, only unblocks stamps with a matching blockID.
// Matches Java's IndexingBase.performIndexingStampOperation(UNBLOCK, ...).
func (oi *OnlineIndexer) UnblockIndex(ctx context.Context, blockID string) error {
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil || !stamp.GetBlock() {
				continue
			}
			if blockID != "" && stamp.GetBlockID() != blockID {
				continue // ID mismatch — don't unblock
			}
			stamp.Block = proto.Bool(false)
			if err := store.SaveIndexingTypeStamp(idx, stamp); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// MarkReadableIfBuilt checks each target index and marks it READABLE if fully built.
// Returns true if all target indexes are now READABLE.
// This is an idempotent operation — safe to call repeatedly.
// Matches Java's IndexingBase.markReadableIfBuilt().
func (oi *OnlineIndexer) MarkReadableIfBuilt(ctx context.Context) (bool, error) {
	allReadable := true
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		// Reset on retry — previous attempt's result is stale.
		allReadable = true
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			state, err := store.readIndexState(idx.Name)
			if err != nil {
				return nil, err
			}
			if state == IndexStateReadable {
				continue
			}
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			missing, err := rangeSet.FirstMissingRange(rtx.Transaction())
			if err != nil {
				return nil, err
			}
			if missing != nil {
				allReadable = false
				continue
			}
			// Index is fully built — mark readable.
			if _, err := store.MarkIndexReadable(idx.Name); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return false, err
	}
	return allReadable, nil
}

// QueryIndexingStamps returns the indexing stamps for all target indexes.
// Returns a map of index name → stamp. Nil stamps are returned as a NONE method stamp.
// Matches Java's IndexingBase.performIndexingStampOperation(QUERY, ...).
func (oi *OnlineIndexer) QueryIndexingStamps(ctx context.Context) (map[string]*metadatapb.IndexBuildIndexingStamp, error) {
	stamps := make(map[string]*metadatapb.IndexBuildIndexingStamp, len(oi.targetIndexes))
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		for _, idx := range oi.targetIndexes {
			stamp, err := store.LoadIndexingTypeStamp(idx)
			if err != nil {
				return nil, err
			}
			if stamp == nil {
				stamp = &metadatapb.IndexBuildIndexingStamp{
					Method: metadatapb.IndexBuildIndexingStamp_NONE.Enum(),
				}
			}
			stamps[idx.Name] = stamp
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return stamps, nil
}
