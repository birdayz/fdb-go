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
	"log/slog"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
)

// validateSourceAndTargetIndexes checks that the source index can feed a
// BY_INDEX build of the target. Matches Java's
// IndexingByIndex.validateSourceAndTargetIndexes (IndexingByIndex.java:240-252),
// messages included, and runs where Java runs it: at session time, after the
// state transaction, so that the build catcher can answer a failure by falling
// back to a records scan (or return it under ForbidRecordScan) rather than the
// builder refusing an indexer Java would build.
func (oi *OnlineIndexer) validateSourceAndTargetIndexes() error {
	src := oi.sourceIndex
	srcTypes := oi.metaData.RecordTypesForIndex(src)
	targetTypes := oi.indexedRecordTypes()
	if len(targetTypes) != 1 {
		return oi.sourceValidationError("target index has multiple types")
	}
	if len(srcTypes) != 1 {
		return oi.sourceValidationError("source index has multiple types")
	}
	for _, rt := range srcTypes {
		if rt.IsSynthetic() {
			return oi.sourceValidationError("source index is on synthetic record types")
		}
	}
	if createsDuplicates(src.RootExpression) {
		return oi.sourceValidationError("source index creates duplicates")
	}
	if src.Type != IndexTypeValue {
		return oi.sourceValidationError("source index is not a VALUE index")
	}
	for _, srcType := range srcTypes {
		covered := false
		for _, rt := range targetTypes {
			if rt.Name == srcType.Name {
				covered = true
				break
			}
		}
		if !covered {
			return oi.sourceValidationError("source index's type is not equal to target index's")
		}
	}
	return nil
}

// sourceValidationError is Java's IndexingBase.validateOrThrowEx failure, with
// its INDEX_NAME, SOURCE_INDEX and INDEXER_ID keys.
func (oi *OnlineIndexer) sourceValidationError(msg string) *IndexingValidationError {
	return &IndexingValidationError{Message: msg, IndexName: oi.primaryIndex().Name, SourceIndexName: oi.sourceIndex.Name, IndexerID: oi.indexerID}
}

// indexedRecordTypes returns the record types this build targets — the explicit
// SetRecordTypes filter, or the union of types covered by the target indexes.
// Matches Java's IndexingCommon.getAllRecordTypes().
func (oi *OnlineIndexer) indexedRecordTypes() []*RecordType {
	if len(oi.recordTypes) > 0 {
		types := make([]*RecordType, 0, len(oi.recordTypes))
		for _, name := range oi.recordTypes {
			if rt := oi.metaData.GetRecordType(name); rt != nil {
				types = append(types, rt)
			}
		}
		return types
	}
	seen := make(map[string]bool)
	var types []*RecordType
	for _, idx := range oi.targetIndexes {
		for _, rt := range oi.metaData.RecordTypesForIndex(idx) {
			if !seen[rt.Name] {
				seen[rt.Name] = true
				types = append(types, rt)
			}
		}
	}
	return types
}

// computeRecordsRange computes the [begin, end) byte range covering the records of the
// indexed record types, when each has a record-type-key primary-key prefix and none is
// synthetic. Returns ok=false (the whole records space may be relevant) if any indexed
// type lacks the prefix or is synthetic, or if there are no indexed types. The returned
// bounds are byte-for-byte Java's TupleRange.betweenInclusive(low, high).toRange():
// begin = low.Pack() verbatim, end = high.Pack()+0xff (RANGE_INCLUSIVE high appends one
// 0xff, NOT strinc). Matches Java's IndexingCommon.computeRecordsRange().
func (oi *OnlineIndexer) computeRecordsRange() (begin, end []byte, ok bool) {
	var lowBytes, highBytes []byte
	for _, rt := range oi.indexedRecordTypes() {
		if !rt.PrimaryKeyHasRecordTypePrefix() || rt.IsSynthetic() {
			return nil, nil, false
		}
		// Java's Tuple.compareTo is the order of the packed bytes, for a type
		// key of any tuple type.
		prefix := tuple.Tuple{rt.GetRecordTypeKey()}.Pack()
		switch {
		case lowBytes == nil:
			lowBytes, highBytes = prefix, prefix
		case bytes.Compare(lowBytes, prefix) > 0:
			lowBytes = prefix
		case bytes.Compare(highBytes, prefix) < 0:
			highBytes = prefix
		}
	}
	if lowBytes == nil {
		return nil, nil, false
	}
	// end = high.pack() + 0xff — Java TupleRange.toRange, RANGE_INCLUSIVE high.
	end = make([]byte, len(highBytes)+1)
	copy(end, highBytes)
	end[len(highBytes)] = 0xff
	return lowBytes, end, true
}

// maybePresetRecordsRange preemptively marks the key ranges OUTSIDE the indexed record
// types' range as already-built in every target index's range set, so the build skips
// scanning keys that cannot hold an indexed-type record. No-op when the whole records
// space may be relevant. Matches Java's IndexingBase.maybePresetRecordsRangeAsync (used
// by the multi-target and mutual build paths).
func (oi *OnlineIndexer) maybePresetRecordsRange(ctx context.Context) error {
	begin, end, ok := oi.computeRecordsRange()
	if !ok {
		return nil
	}
	_, err := oi.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		if err := oi.validateBuildSession(store); err != nil {
			return nil, err
		}
		tr := store.context.Transaction()
		// Mark the leading and trailing out-of-range gaps as built, sequentially per
		// target (ordered mutations within the transaction, matching Java's insertRanges):
		// [nil, begin) then [end, nil). requireEmpty=true keeps it resume-safe.
		for _, idx := range oi.targetIndexes {
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			if _, err := rangeSet.InsertRange(tr, nil, begin, true); err != nil {
				return nil, err
			}
			if _, err := rangeSet.InsertRange(tr, end, nil, true); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// indexingAttemptsLimit is Java's OnlineIndexer.INDEXING_ATTEMPTS_RECURSION_LIMIT,
// a safety net over the catcher's retries that the algorithm should never reach.
const indexingAttemptsLimit = 5

// requestedIndexingMethod is what a BY_INDEX continuation replaced: the policy
// and source index the caller asked for, restored with REBUILD if the previous
// method's source index turns out to be unusable (Java indexingLauncher's
// requestedPolicy).
type requestedIndexingMethod struct {
	policy      *IndexingPolicy
	sourceIndex *Index
}

// indexingCatch is what the build catcher makes of a failed attempt.
type indexingCatch int

const (
	// indexingCatchFail returns the error.
	indexingCatchFail indexingCatch = iota
	// indexingCatchRetry runs another attempt under the adjusted method.
	indexingCatchRetry
	// indexingCatchDone ends the build successfully: mutual peers have
	// published every target.
	indexingCatchDone
)

// BuildIndex runs the index build and returns the number of records scanned.
// Matches Java's OnlineIndexer.buildIndex: every attempt resolves the session
// against the primary index's state, which may build (and then publish), only
// publish, or leave a READABLE index alone, and a failed attempt goes to
// indexingCatcher, which may retry it by another method.
//
// The count is every record scanned by every attempt, a retried one included.
// LastBuildOutcome reports what the call did.
func (oi *OnlineIndexer) BuildIndex(ctx context.Context) (int64, error) {
	oi.lastOutcome = IndexBuildOutcomeNone
	var requested *requestedIndexingMethod
	var total int64
	// The outcome is the CALL's, not its last attempt's: an attempt that indexed
	// records and then failed into a retry built them, whatever the next attempt
	// finds (a peer may publish in between, and the next attempt then leaves the
	// READABLE index alone or ends the mutual build as completed by its peers).
	built := false
	outcomeOfCall := func(last IndexBuildOutcome) IndexBuildOutcome {
		if built {
			return IndexBuildOutcomeBuilt
		}
		return last
	}
	for attempt := 1; ; attempt++ {
		n, outcome, err := oi.buildIndexAttempt(ctx)
		total += n
		if err == nil {
			oi.lastOutcome = outcomeOfCall(outcome)
			return total, nil
		}
		if n > 0 {
			built = true
		}
		catch, next, err := oi.indexingCatcher(ctx, err, attempt, requested)
		switch catch {
		case indexingCatchDone:
			oi.lastOutcome = outcomeOfCall(IndexBuildOutcomeCompletedByPeers)
			return total, nil
		case indexingCatchRetry:
			requested = next
		default:
			return total, err
		}
	}
}

// indexingCatcher ports Java's OnlineIndexer.indexingCatcher
// (OnlineIndexer.java:152-274) arm for arm:
//   - a PartlyBuiltError follows IfMismatchPrevious: CONTINUE resumes a single
//     target's previous method (a records scan for BY_RECORDS or
//     MULTI_TARGET_BY_RECORDS, the saved source index for BY_INDEX), REBUILD
//     rebuilds by the requested method, and anything else fails;
//   - an IndexingValidationError of a BY_INDEX build restores the requested
//     method with REBUILD when the failed source was a continuation's, and
//     otherwise falls back to a records scan unless ForbidRecordScan;
//   - under a mutual policy an UnexpectedReadableError ends the build when every
//     target is readable, and otherwise falls back to a records scan, whose
//     session start checks and publishes what the peers left.
//
// Java counts the attempt a failure arrived on from one; past
// indexingAttemptsLimit the error is returned whatever it is.
func (oi *OnlineIndexer) indexingCatcher(ctx context.Context, err error, attempt int, requested *requestedIndexingMethod) (indexingCatch, *requestedIndexingMethod, error) {
	logger := oi.logger
	if logger == nil {
		logger = slog.Default()
	}
	if attempt > indexingAttemptsLimit {
		logger.LogAttrs(ctx, slog.LevelError, "Too many indexing attempts",
			slog.Int("attempt", attempt), slog.String("index", oi.targetIndexNamesForLog()))
		return indexingCatchFail, nil, err
	}

	var partly *PartlyBuiltError
	if errors.As(err, &partly) {
		action, aerr := oi.policy.GetIfMismatchPrevious()
		if aerr != nil {
			return indexingCatchFail, nil, aerr
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "conflicting indexing type stamp",
			slog.Int("attempt", attempt), slog.String("index", oi.targetIndexNamesForLog()),
			slog.Int("desired_action", int(action)),
			slog.String("actual", partly.SavedStamp), slog.String("expected", partly.ExpectedStamp))
		switch action {
		case DesiredActionContinue:
			if oi.isMultiTarget() {
				return indexingCatchFail, nil, err
			}
			switch partly.Saved.GetMethod() {
			case metadatapb.IndexBuildIndexingStamp_BY_RECORDS, metadatapb.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS:
				// Java allows the fallback from a multi-target build to a single
				// target, but not to a subset.
				oi.fallBackToRecordsScan()
				return indexingCatchRetry, nil, nil
			case metadatapb.IndexBuildIndexingStamp_BY_INDEX:
				source, serr := oi.savedSourceIndex(partly.Saved)
				if serr != nil {
					return indexingCatchFail, nil, serr
				}
				orig := &requestedIndexingMethod{policy: oi.policy, sourceIndex: oi.sourceIndex}
				oi.sourceIndex = source
				return indexingCatchRetry, orig, nil
			}
			return indexingCatchFail, nil, err
		case DesiredActionRebuild:
			oi.policy = oi.policy.withIfWriteOnly(DesiredActionRebuild)
			return indexingCatchRetry, nil, nil
		}
		return indexingCatchFail, nil, err
	}

	var validation *IndexingValidationError
	if oi.sourceIndex != nil && errors.As(err, &validation) {
		if requested != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "The previous method's source index isn't usable. Rebuild by the requested policy",
				slog.Int("attempt", attempt), slog.String("index", oi.targetIndexNamesForLog()))
			oi.policy = requested.policy.withIfWriteOnly(DesiredActionRebuild)
			oi.sourceIndex = requested.sourceIndex
			return indexingCatchRetry, nil, nil
		}
		if (oi.policy == nil || !oi.policy.ForbidRecordScan) && !oi.fallbackToRecordsScan {
			logger.LogAttrs(ctx, slog.LevelWarn, "Fallback to a by-record scan",
				slog.Int("attempt", attempt), slog.String("index", oi.targetIndexNamesForLog()))
			oi.fallBackToRecordsScan()
			return indexingCatchRetry, nil, nil
		}
	}

	if oi.mutual {
		var unexpected *UnexpectedReadableError
		if errors.As(err, &unexpected) {
			if unexpected.AllReadable {
				return indexingCatchDone, nil, nil
			}
			oi.fallBackToRecordsScan()
			return indexingCatchRetry, nil, nil
		}
	}
	return indexingCatchFail, nil, err
}

// fallBackToRecordsScan is the catcher's switch to Java's
// IndexingMultiTargetByRecords, which getIndexer hands a forced stamp overwrite.
func (oi *OnlineIndexer) fallBackToRecordsScan() {
	oi.fallbackToRecordsScan = true
	oi.enforcedStampOverwrite = true
}

// savedSourceIndex resolves the source index a BY_INDEX stamp names, as Java
// resolves the policy's source index subspace key: Index.decodeSubspaceKey, then
// RecordMetaData.getIndexFromSubspaceKey.
func (oi *OnlineIndexer) savedSourceIndex(stamp *metadatapb.IndexBuildIndexingStamp) (*Index, error) {
	key, err := tuple.Unpack(stamp.GetSourceIndexSubspaceKey())
	if err != nil || len(key) != 1 {
		return nil, &RecordCoreError{Message: "subspace key must encode a single item tuple"}
	}
	source := oi.metaData.GetIndexFromSubspaceKey(key[0])
	if source == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("Unknown index subspace key %v", key[0])}
	}
	return source, nil
}

// buildIndexAttempt is one launch of Java's indexing function: the session
// start, the build by the method the catcher has left in force, and the
// publication. Each attempt writes a heartbeat under that method with the
// indexer's one identity (Java's indexerId, shared by every attempt of the
// indexer, IndexingCommon.java:53), and removes it when the attempt ends; the
// throttle's adapted limit carries over, where Java's new indexer per attempt
// starts again from the configured one.
func (oi *OnlineIndexer) buildIndexAttempt(ctx context.Context) (int64, IndexBuildOutcome, error) {
	// Java's policy names its source index, and each session compiles the stamp
	// and validates the source from the metadata's index of that name
	// (IndexingByIndex.java:68-71, :95-101), so a caller's *Index whose subspace
	// key, versions or type differ from the metadata's never reaches the stamp or
	// validateSourceAndTargetIndexes. (The scan already reads the source by name.)
	// A name the metadata lacks is refused in the state transaction
	// (prepareIndexingState).
	if oi.buildsByIndex() && oi.metaData != nil {
		if resolved := oi.metaData.GetIndex(oi.sourceIndex.Name); resolved != nil {
			oi.sourceIndex = resolved
		}
	}
	// Java names a build's heartbeat by its stamp's method (IndexingBase.java:457);
	// Go's SynchronizedSessionLockedError also reports it as ExistingInfo, a Go
	// addition (Java's lock exception carries no info key).
	heartbeat := oi.newHeartbeat(oi.buildIndexingStamp().GetMethod().String(), oi.buildsMutually(), oi.db.Env())
	oi.sessionHeartbeat = heartbeat
	oi.admittedHeartbeat = nil
	defer func() {
		if oi.sessionHeartbeat != nil {
			oi.cleanupPendingQueueHeartbeat(heartbeat)
			oi.sessionHeartbeat = nil
			oi.admittedHeartbeat = nil
		}
	}()
	// The build clock, not a metric: this anchor is what throttleBetweenRanges measures the
	// configured timeLimit against, so it decides when the build stops and therefore HOW MANY
	// RANGES end up durably recorded in the built-range set. It has to come off the env clock
	// or a simulated build stops at a wall-clock-determined, unreproducible point.
	startTime := oi.db.Env().Now()

	// Step 1: Resolve the session against the index state, marking the targets
	// WRITE_ONLY when it builds.
	start, err := oi.markWriteOnly(ctx)
	if err != nil {
		return 0, IndexBuildOutcomeNone, fmt.Errorf("mark write-only: %w", err)
	}
	switch start {
	case indexingSessionSkip:
		return 0, IndexBuildOutcomeLeftAlone, nil
	case indexingSessionMarkReadable:
		if !oi.markReadableEnabled {
			return 0, IndexBuildOutcomeLeftAlone, nil
		}
		if err := oi.markReadable(ctx); err != nil {
			return 0, IndexBuildOutcomeNone, fmt.Errorf("mark readable: %w", err)
		}
		return 0, IndexBuildOutcomePublished, nil
	}

	// Step 2: Build in chunks across multiple transactions.
	if oi.buildsMutually() {
		n, err := oi.buildIndexMutual(ctx, startTime)
		return n, IndexBuildOutcomeBuilt, err
	}

	// A records-scan build presets the out-of-range gaps as already built so the scan
	// skips them. Java's records-scan indexer is IndexingMultiTargetByRecords for one
	// target or several (OnlineIndexer.getIndexer, :302-314: everything that is not
	// mutual or a single-target BY_INDEX build, a BY_INDEX fallen back to a records scan
	// included), and it presets (IndexingMultiTargetByRecords.java:120). Go preset only
	// for several targets, so a single-target build wrote a range set Java does not
	// (conformance "The online build presets a string-keyed record type's range as Java
	// does"). A BY_INDEX build does not preset, in either engine.
	if !oi.buildsByIndex() {
		if err := oi.maybePresetRecordsRange(ctx); err != nil {
			return 0, IndexBuildOutcomeNone, fmt.Errorf("preset records range: %w", err)
		}
	}

	buildFn := oi.buildRange
	if oi.buildsByIndex() {
		// Java IndexingByIndex.buildIndexInternalAsync validates after the state
		// transaction has committed, so a failure leaves the target WRITE_ONLY
		// under a BY_INDEX stamp that the catcher's records scan then overwrites.
		if err := oi.validateSourceAndTargetIndexes(); err != nil {
			return 0, IndexBuildOutcomeNone, err
		}
		buildFn = oi.buildRangeByIndex
	}

	var totalRecords int64
	for {
		n, hasMore, err := oi.buildRangeWithRetries(ctx, buildFn)
		if err != nil {
			return totalRecords, IndexBuildOutcomeNone, fmt.Errorf("build range: %w", err)
		}
		totalRecords += n
		if err := oi.drainPendingIndexWrites(ctx, heartbeat); err != nil {
			return totalRecords, IndexBuildOutcomeNone, err
		}
		if err := oi.mergeRequestedIndexes(ctx); err != nil {
			return totalRecords, IndexBuildOutcomeNone, err
		}
		if !hasMore {
			break
		}

		// Time-limit check + enforced post-transaction delay + progress log between ranges.
		if err := oi.throttleBetweenRanges(ctx, startTime, totalRecords, n); err != nil {
			return totalRecords, IndexBuildOutcomeNone, err
		}
	}

	// Step 3: Mark all target indexes as READABLE (unless disabled — e.g. to inspect or
	// resume the build). Matches Java OnlineIndexer.buildIndex(boolean markReadable).
	if oi.markReadableEnabled {
		if err := oi.markReadable(ctx); err != nil {
			return totalRecords, IndexBuildOutcomeNone, fmt.Errorf("mark readable: %w", err)
		}
	}

	return totalRecords, IndexBuildOutcomeBuilt, nil
}

// buildIndexMutual runs the mutual (concurrent) build path.
func (oi *OnlineIndexer) buildIndexMutual(ctx context.Context, startTime time.Time) (int64, error) {
	mutual := newMutualIndexBuilder(oi)

	// Mutual builds preset the out-of-range gaps as already-built (Java
	// IndexingMutuallyByRecords), before the fragmented build loop.
	if err := oi.maybePresetRecordsRange(ctx); err != nil {
		return 0, fmt.Errorf("preset records range: %w", err)
	}

	var totalRecords int64
	for {
		// Use buildRangeWithRetries for adaptive throttling + retry on transient
		// FDB errors, matching the BY_RECORDS path. Without this, mutual builds
		// had no limit adjustment on failure and no rate limiting.
		n, hasMore, err := oi.buildRangeWithRetries(ctx, func(ctx context.Context) (int64, bool, error) {
			return mutual.buildMutual(ctx)
		})
		if err != nil {
			return totalRecords, fmt.Errorf("mutual build range: %w", err)
		}
		totalRecords += n
		if err := oi.mergeRequestedIndexes(ctx); err != nil {
			return totalRecords, err
		}
		if !hasMore {
			break
		}

		// Time-limit check + enforced post-transaction delay + progress log between ranges.
		if err := oi.throttleBetweenRanges(ctx, startTime, totalRecords, n); err != nil {
			return totalRecords, err
		}
	}

	// Mark all target indexes as READABLE (unless disabled). Matches Java
	// OnlineIndexer.buildIndex(boolean markReadable).
	if oi.markReadableEnabled {
		if err := oi.markReadable(ctx); err != nil {
			return totalRecords, fmt.Errorf("mark readable: %w", err)
		}
	}

	return totalRecords, nil
}
