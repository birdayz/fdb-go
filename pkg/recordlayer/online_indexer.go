// Portions derived from FoundationDB Record Layer (OnlineIndexer.java,
// IndexingBase.java, OnlineIndexOperationConfig.java, IndexingByIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"log/slog"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"github.com/google/uuid"
)

// TakeoverType represents allowed build method conversions when resuming an index build
// that was started by a different indexing method.
// Matches Java's OnlineIndexer.IndexingPolicy.TakeoverTypes.
type TakeoverType int

const (
	// TakeoverMultiTargetToSingle allows a single-target BY_RECORDS build to continue
	// a MULTI_TARGET_BY_RECORDS build.
	TakeoverMultiTargetToSingle TakeoverType = iota
	// TakeoverMutualToSingle allows a single-target BY_RECORDS build to continue
	// a MUTUAL_BY_RECORDS build.
	TakeoverMutualToSingle
	// TakeoverByRecordsToMutual allows a MUTUAL_BY_RECORDS build to continue
	// a BY_RECORDS or MULTI_TARGET_BY_RECORDS build.
	TakeoverByRecordsToMutual
)

// IndexingDesiredAction is what a build session does with the index state it
// finds. Matches Java's OnlineIndexer.IndexingPolicy.DesiredAction.
type IndexingDesiredAction int

const (
	// DesiredActionDefault leaves the choice to Java's builder default for the
	// state: REBUILD if disabled, CONTINUE if write-only or readable. It is the
	// zero value, so an IndexingPolicy that never mentions a state gets Java's
	// default for it.
	DesiredActionDefault IndexingDesiredAction = iota
	// DesiredActionError refuses to start a session over the state.
	DesiredActionError
	// DesiredActionRebuild clears the index and builds it from scratch.
	DesiredActionRebuild
	// DesiredActionContinue continues a write-only build, builds a disabled index
	// without clearing it, and leaves a readable index alone.
	DesiredActionContinue
	// DesiredActionMarkReadable publishes the index without building it.
	DesiredActionMarkReadable
)

// IndexingPolicy configures how the OnlineIndexer handles stamp conflicts,
// blocked stamps, and build method conversions.
// Matches Java's OnlineIndexer.IndexingPolicy.
type IndexingPolicy struct {
	// IfDisabled, IfWriteOnly and IfReadable choose what a session does with the
	// primary index's state (Java ifDisabled/ifWriteOnly/ifReadable). The zero
	// value is Java's default for that state; see GetStateDesiredAction.
	IfDisabled  IndexingDesiredAction
	IfWriteOnly IndexingDesiredAction
	IfReadable  IndexingDesiredAction

	// IfMismatchPrevious chooses what BuildIndex does when the index is partly
	// built by a different method than the one requested (Java
	// ifMismatchPrevious, read by OnlineIndexer.indexingCatcher). CONTINUE, the
	// zero value's meaning, continues the previous method; REBUILD clears and
	// builds by the requested one; ERROR and MARK_READABLE return the
	// PartlyBuiltError. See GetIfMismatchPrevious.
	IfMismatchPrevious IndexingDesiredAction

	// ForbidRecordScan stops a BY_INDEX build whose source index cannot be used
	// from falling back to a records scan (Java forbidRecordScan).
	ForbidRecordScan bool

	// InitialMergesCountLimit is the initial deferred-merge budget (zero is unlimited).
	InitialMergesCountLimit int64
	// ForceStampOverwrite forces writing the stamp without conflict checks on
	// fresh (non-continued) builds, or allows overwriting on continued builds
	// when no records have been scanned. Used internally during rebuild.
	ForceStampOverwrite bool

	// AllowUnblock permits unblocking a blocked stamp during resume.
	AllowUnblock bool

	// AllowUnblockID restricts unblocking to stamps with a matching blockID.
	// Empty string means any blockID is accepted (when AllowUnblock is true).
	AllowUnblockID string

	// AllowedTakeovers is the set of allowed build method conversions.
	AllowedTakeovers map[TakeoverType]bool

	// PendingWriteQueueIndexes explicitly requests deferred maintenance by index name.
	PendingWriteQueueIndexes map[string]bool
	// Nil selects Java's default of 100 closeout attempts; zero remains explicit.
	PendingWriteQueueIndexesMaxDrainAttempts *int64
}

// GetStateDesiredAction resolves what a session does with an index in state.
// Matches Java's IndexingPolicy.getStateDesiredAction, with the builder defaults
// (IndexingPolicy.Builder: ifDisabled REBUILD, ifWriteOnly CONTINUE, ifReadable
// CONTINUE) standing in for an unset field. READABLE_UNIQUE_PENDING is always
// MARK_READABLE, as in Java: the index is built and only publication remains.
func (p *IndexingPolicy) GetStateDesiredAction(state IndexState) (IndexingDesiredAction, error) {
	var set, def IndexingDesiredAction
	switch state {
	case IndexStateDisabled:
		def = DesiredActionRebuild
		if p != nil {
			set = p.IfDisabled
		}
	case IndexStateWriteOnly, IndexStateWriteOnlyWithQueue:
		def = DesiredActionContinue
		if p != nil {
			set = p.IfWriteOnly
		}
	case IndexStateReadable:
		def = DesiredActionContinue
		if p != nil {
			set = p.IfReadable
		}
	case IndexStateReadableUniquePending:
		return DesiredActionMarkReadable, nil
	default:
		return 0, &RecordCoreError{Message: "bad index state: " + state.String()}
	}
	switch set {
	case DesiredActionDefault:
		return def, nil
	case DesiredActionError, DesiredActionRebuild, DesiredActionContinue, DesiredActionMarkReadable:
		return set, nil
	default:
		return 0, &RecordCoreError{Message: fmt.Sprintf("bad indexing desired action %d for index state %s", int(set), state)}
	}
}

// GetIfMismatchPrevious resolves IfMismatchPrevious, with Java's builder default
// CONTINUE standing in for an unset field (IndexingPolicy.Builder). An action
// outside the enum is a RecordCoreError, as in GetStateDesiredAction.
func (p *IndexingPolicy) GetIfMismatchPrevious() (IndexingDesiredAction, error) {
	if p == nil || p.IfMismatchPrevious == DesiredActionDefault {
		return DesiredActionContinue, nil
	}
	switch p.IfMismatchPrevious {
	case DesiredActionError, DesiredActionRebuild, DesiredActionContinue, DesiredActionMarkReadable:
		return p.IfMismatchPrevious, nil
	}
	return 0, &RecordCoreError{Message: fmt.Sprintf("bad indexing desired action %d for a mismatched previous build", int(p.IfMismatchPrevious))}
}

// withIfWriteOnly is Java's policy.toBuilder().setIfWriteOnly(action).build():
// the catcher's retries replace the indexer's policy and never write through
// the caller's pointer.
func (p *IndexingPolicy) withIfWriteOnly(action IndexingDesiredAction) *IndexingPolicy {
	var copied IndexingPolicy
	if p != nil {
		copied = *p
	}
	copied.IfWriteOnly = action
	return &copied
}

func (p *IndexingPolicy) ShouldUsePendingWriteQueue(index *Index) bool {
	return p != nil && p.PendingWriteQueueIndexes[index.Name]
}

func (p *IndexingPolicy) GetPendingWriteQueueIndexesMaxDrainAttempts() int64 {
	if p == nil || p.PendingWriteQueueIndexesMaxDrainAttempts == nil {
		return 100
	}
	return *p.PendingWriteQueueIndexesMaxDrainAttempts
}

// ShouldAllowUnblock returns true if the policy allows unblocking a stamp with the given blockID.
// Matches Java's IndexingPolicy.shouldAllowUnblock().
func (p *IndexingPolicy) ShouldAllowUnblock(stampBlockID string) bool {
	if p == nil || !p.AllowUnblock {
		return false
	}
	return p.AllowUnblockID == "" || p.AllowUnblockID == stampBlockID
}

// ShouldAllowTypeConversionContinue returns true if switching from savedStamp's method
// to newStamp's method is permitted by the policy.
// Matches Java's IndexingPolicy.shouldAllowTypeConversionContinue().
func (p *IndexingPolicy) ShouldAllowTypeConversionContinue(newStamp, savedStamp *metadatapb.IndexBuildIndexingStamp) bool {
	if p == nil || len(p.AllowedTakeovers) == 0 {
		return false
	}
	newMethod := newStamp.GetMethod()
	oldMethod := savedStamp.GetMethod()

	if newMethod == metadatapb.IndexBuildIndexingStamp_BY_RECORDS {
		if oldMethod == metadatapb.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS {
			return p.AllowedTakeovers[TakeoverMultiTargetToSingle]
		}
		if oldMethod == metadatapb.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS {
			return p.AllowedTakeovers[TakeoverMutualToSingle]
		}
	}

	if newMethod == metadatapb.IndexBuildIndexingStamp_MUTUAL_BY_RECORDS {
		if !p.AllowedTakeovers[TakeoverByRecordsToMutual] {
			return false
		}
		if oldMethod == metadatapb.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS {
			// Allow if same set of targets or single target.
			if len(newStamp.GetTargetIndex()) == 1 {
				return true
			}
			newTargets := newStamp.GetTargetIndex()
			oldTargets := savedStamp.GetTargetIndex()
			if len(newTargets) != len(oldTargets) {
				return false
			}
			oldSet := make(map[string]bool, len(oldTargets))
			for _, t := range oldTargets {
				oldSet[t] = true
			}
			for _, t := range newTargets {
				if !oldSet[t] {
					return false
				}
			}
			return true
		}
		if oldMethod == metadatapb.IndexBuildIndexingStamp_BY_RECORDS {
			return len(newStamp.GetTargetIndex()) == 1
		}
	}
	return false
}

// OnlineIndexer builds indexes on existing data across multiple transactions.
// Each transaction processes a chunk of records, tracks progress via IndexingRangeSet,
// and the build resumes from where it left off if interrupted.
//
// Supports four modes:
//   - BY_RECORDS (default, single target): scans all records in primary key order
//   - BY_INDEX (single target): scans an existing readable VALUE index to find records
//   - MULTI_TARGET_BY_RECORDS: scans records once, builds multiple indexes simultaneously
//   - MUTUAL_BY_RECORDS: concurrent multi-process building with fragment-based work division
//
// Matches Java's OnlineIndexer with IndexingByRecords, IndexingByIndex,
// IndexingMultiTargetByRecords, and IndexingMutuallyByRecords.
type OnlineIndexer struct {
	mergeRequiredIndexes []*Index
	mergers              map[string]*indexingMerger
	db                   *FDBDatabase
	metaData             *RecordMetaData
	targetIndexes        []*Index           // target indexes to build (first = primary for range tracking)
	queuedIndexes        []*Index           // reconstructed from persisted states after preparation commits
	sessionHeartbeat     *IndexingHeartbeat // stable across all transactions of the build
	admittedHeartbeat    *IndexingHeartbeat // published only after preparation commits
	retiredBuildTargets  map[string]bool    // commit-bound completion; targetIndexes still defines stamp identity
	sourceIndex          *Index             // non-nil = BY_INDEX strategy (single target only)
	subspace             subspace.Subspace
	limit                int
	maxRetries           int // max retries per range on transient failures (0 = no retries)
	// maxAttempts bounds each transaction the indexer runs (Java's runner
	// maxAttempts, OnlineIndexOperationBaseBuilder.setMaxAttempts); 0 means the
	// database's (default 10). maxRetries' lessen-work retries run over it.
	maxAttempts      int
	recordsPerSecond int // inter-transaction rate limit (0 = unlimited, default 10000)
	// enforcedPostTransactionDelay, if > 0, is a fixed per-transaction delay (ms) applied
	// INSTEAD of recordsPerSecond. Java OnlineIndexOperationConfig.enforcedPostTransactionDelay.
	enforcedPostTransactionDelay int
	timeLimit                    time.Duration     // overall build time limit (0 = unlimited)
	recordTypes                  []string          // record types to index (empty = all types; not allowed with multi-target)
	policy                       *IndexingPolicy   // stamp conflict resolution policy (nil = default behavior)
	throttle                     *indexingThrottle // adaptive throttle (created at Build time)
	mutual                       bool              // the policy asks for MUTUAL_BY_RECORDS; see buildsMutually
	// fallbackToRecordsScan is set by the build catcher when the requested
	// method cannot be used and the build continues as a records scan (Java
	// OnlineIndexer.fallbackToRecordsScan). Like Java's, it is never reset: a
	// later BuildIndex on the same indexer keeps scanning records.
	fallbackToRecordsScan bool

	// initialLimit, increaseLimitAfter, maxWriteLimitBytes and
	// transactionTimeLimitMs are Java's OnlineIndexOperationConfig fields of
	// those names: the starting per-transaction limit (0 = limit), the
	// successes before a re-increase, and the approximate transaction size and
	// age past which a range commits early (0 = no such limit).
	initialLimit           int
	increaseLimitAfter     int
	maxWriteLimitBytes     int64
	transactionTimeLimitMs int64
	// enforcedStampOverwrite is Java IndexingBase.enforceStampOverwrite, set with
	// the fallback: the records scan must replace the abandoned method's stamp.
	enforcedStampOverwrite bool
	// indexerID is this indexer's heartbeat identity, minted once (Java
	// IndexingCommon.indexerId): every attempt the catcher runs writes its
	// heartbeat under it, so an attempt never mistakes the previous attempt's
	// heartbeat for a live peer. Merges use it as their merge session ID.
	indexerID uuid.UUID
	// lastOutcome is what the last completed BuildIndex did; see LastBuildOutcome.
	lastOutcome      IndexBuildOutcome
	mutualBoundaries [][]byte // pre-set fragment boundaries (nil: read from the shard map)
	leaseLengthMs    int64    // heartbeat lease duration in ms (default defaultLeaseLengthMs)
	// markReadableEnabled controls whether BuildIndex marks the index readable (and
	// thus erases the per-build bookkeeping) at the end. Defaults to true. Matches
	// Java's OnlineIndexer.buildIndex(boolean markReadable): set false to build the
	// index data while leaving it WRITE_ONLY with its build stamp/progress intact
	// (e.g. to inspect or resume the build).
	markReadableEnabled bool

	// allowUniquePendingState opts in to leaving a unique index in
	// READABLE_UNIQUE_PENDING when the build finds duplicates, instead of failing
	// the build. Java OnlineIndexer.IndexingPolicy.Builder.allowUniquePendingState,
	// whose field defaults FALSE (OnlineIndexer.java:1220) and whose javadoc states
	// the contract exactly: "allow=false (default, backward compatible): throw an
	// exception."
	//
	// It is deliberately NOT enough on its own — see shouldAllowUniquePendingState.
	allowUniquePendingState bool

	// formatVersion pins the format version every store this indexer opens uses;
	// 0 means the newest this binary knows. Java carries it on the shared
	// record-store builder rather than as a separate indexer field.
	formatVersion *int32

	// maintenanceFilter is the IndexMaintenanceFilter every store this indexer
	// opens uses, so the build maintains the entries the store's writes do.
	// Java carries it on the shared record-store builder too.
	maintenanceFilter IndexMaintenanceFilter

	// storeTemplate is the caller's record-store builder (SetRecordStoreBuilder,
	// SetRecordStore), context-free: every store the indexer opens is opened
	// through a copy of it, so it carries what the caller configured and the
	// indexer has no field for (the serializer among it). Nil opens a fresh
	// builder.
	storeTemplate *StoreBuilder

	// progressLogIntervalMillis throttles the per-range "Indexer: Built Range"
	// progress log (see maybeLogBuildProgress). Matches Java
	// OnlineIndexOperationConfig.progressLogIntervalMillis:
	//   <0  → never log (DEFAULT_PROGRESS_LOG_INTERVAL = -1, disabled);
	//    0  → log after every range;
	//   >0  → log at most once per this many milliseconds.
	progressLogIntervalMillis int64
	// timeOfLastProgressLogMillis is the throttle clock for the progress log
	// (Java IndexingBase.timeOfLastProgressLogMillis). Mutated only by
	// shouldLogBuildProgress, which runs single-threaded in the build loop.
	timeOfLastProgressLogMillis int64
	// logger receives the operational progress events. nil → slog.Default() at
	// log time, mirroring the client's WithLogger convention (RFC-097) so a
	// zero-config app keeps the standard slog integration and tests never mutate
	// process globals.
	logger *slog.Logger
}

// shouldAllowUniquePendingState reports whether a finished build may leave a
// unique index in READABLE_UNIQUE_PENDING rather than failing on duplicates.
//
// Port of Java's OnlineIndexer.IndexingPolicy.shouldAllowUniquePendingState
// (OnlineIndexer.java:1117-1120), which is an AND of two independent conditions:
//
//	return allowUniquePendingState
//	        && store.getFormatVersionEnum().isAtLeast(FormatVersion.READABLE_UNIQUE_PENDING);
//
// The format-version half is not redundant belt-and-braces. READABLE_UNIQUE_PENDING
// is format version 9 (FormatVersion.java:145); writing that state into a store an
// older binary can still open would hand it an index state it cannot interpret. The
// opt-in alone must therefore never be sufficient.
func (oi *OnlineIndexer) shouldAllowUniquePendingState(store *FDBRecordStore) bool {
	return oi.allowUniquePendingState &&
		store.GetFormatVersion() >= int32(formatVersionReadableUniquePending)
}

// primaryIndex returns the first target index, which drives range tracking.
func (oi *OnlineIndexer) primaryIndex() *Index {
	return oi.targetIndexes[0]
}

// isMultiTarget returns true if building multiple indexes simultaneously.
func (oi *OnlineIndexer) isMultiTarget() bool {
	return len(oi.targetIndexes) > 1
}

// buildsMutually and buildsByIndex pick the build method the way Java's
// OnlineIndexer.getIndexer picks the indexer: mutual first, then by index for a
// single target, and a records scan once the catcher has fallen back. The
// policy's own mutual flag (oi.mutual) still decides what Java decides from
// policy.isMutual() rather than from the indexer: the queue refusals and the
// catcher's UnexpectedReadable arm.
func (oi *OnlineIndexer) buildsMutually() bool {
	return oi.mutual && !oi.fallbackToRecordsScan
}

func (oi *OnlineIndexer) buildsByIndex() bool {
	return oi.sourceIndex != nil && !oi.isMultiTarget() && !oi.fallbackToRecordsScan && !oi.buildsMutually()
}

// newHeartbeat is a heartbeat under this indexer's one identity, minted from
// env the first time it is needed.
func (oi *OnlineIndexer) newHeartbeat(info string, allowMutual bool, env *Env) *IndexingHeartbeat {
	return newIndexingHeartbeatWithID(oi.ensureIndexerID(env), info, resolvedLeaseLengthMs(oi.leaseLengthMs), allowMutual, env)
}

// ensureIndexerID mints this indexer's one identity from env on first use.
func (oi *OnlineIndexer) ensureIndexerID(env *Env) uuid.UUID {
	if oi.indexerID == uuid.Nil {
		oi.indexerID = newIndexerID(env)
	}
	return oi.indexerID
}

// IndexBuildOutcome is what a BuildIndex call did. It is a Go extension: Java's
// buildIndex returns nothing, and a session that left a READABLE index alone,
// one that only published, and a build over an empty store all return 0 records.
type IndexBuildOutcome int

const (
	// IndexBuildOutcomeNone: no BuildIndex call has completed, or the last one
	// failed.
	IndexBuildOutcomeNone IndexBuildOutcome = iota
	// IndexBuildOutcomeBuilt: the call succeeded and built. Either the attempt
	// that ended it ran the build over the targets' ranges (published unless
	// publication was disabled), which it counts even when it indexed no
	// records, as over an empty store; or an earlier attempt indexed records
	// and failed into a retry, whatever the attempt that ended the call then
	// found: the targets published, which it may have done itself
	// (MARK_READABLE), or a mutual build its peers completed.
	IndexBuildOutcomeBuilt
	// IndexBuildOutcomeLeftAlone: the call neither built nor published. That
	// covers two cases: the primary index was READABLE under CONTINUE (what fdb
	// reports as already readable), or MARK_READABLE was resolved with
	// publication disabled (unreachable from fdb, which always publishes).
	IndexBuildOutcomeLeftAlone
	// IndexBuildOutcomePublished: the call published the targets without
	// building them (MARK_READABLE).
	IndexBuildOutcomePublished
	// IndexBuildOutcomeCompletedByPeers: a mutual build found every target
	// already published by its peers, and this call indexed nothing.
	IndexBuildOutcomeCompletedByPeers
)

// LastBuildOutcome reports what the last BuildIndex call did, over all of its
// attempts, when it returned no error; IndexBuildOutcomeNone before any call,
// and after a call that failed, whatever an earlier call of the same indexer
// did.
func (oi *OnlineIndexer) LastBuildOutcome() IndexBuildOutcome {
	return oi.lastOutcome
}

// openStore opens an FDBRecordStore for the current transaction.
func (oi *OnlineIndexer) openStore(rtx *FDBRecordContext) (*FDBRecordStore, error) {
	return oi.openStoreWithPreflight(rtx, nil)
}

func (oi *OnlineIndexer) openStoreWithPreflight(rtx *FDBRecordContext, preflight func(*FDBRecordStore) error) (*FDBRecordStore, error) {
	// The format version rides along with every store this indexer opens. Java
	// threads it the same way, by reusing the caller's record-store builder
	// (IndexingCommon.getRecordStoreBuilder), so an indexer cannot silently open a
	// store at a newer format than the instance that configured it -- which would
	// defeat the format half of shouldAllowUniquePendingState.
	sb := NewStoreBuilder()
	if oi.storeTemplate != nil {
		sb = oi.storeTemplate.copyBuilder()
	}
	sb = sb.SetContext(rtx).
		SetMetaDataProvider(oi.metaData).
		SetSubspace(oi.subspace)
	// Only PASS THROUGH an explicit pin. Calling SetFormatVersion unconditionally
	// would hand the store builder an explicit 0, which it rejects.
	if oi.formatVersion != nil {
		sb = sb.SetFormatVersion(*oi.formatVersion)
	}
	sb = sb.SetIndexMaintenanceFilter(oi.maintenanceFilter)
	return sb.openWithPreflight(preflight)
}
