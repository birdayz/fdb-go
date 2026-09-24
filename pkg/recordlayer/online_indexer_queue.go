package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"fdb.dev/pkg/fdbgo/fdb"

	"fdb.dev/gen"
)

// IndexingValidationError matches Java's IndexingBase.ValidationException: a
// session precondition failed. It carries the log keys Java attaches at each
// throw site: the primary index (and, for "Index state is not as expected", its
// last-modified version), a disagreeing follower, and a BY_INDEX build's
// source index. The build catcher falls back from a BY_INDEX build on it.
type IndexingValidationError struct {
	Message          string
	IndexName        string
	IndexVersion     int
	IndexState       IndexState
	TargetIndexName  string
	TargetIndexState IndexState
	SourceIndexName  string
	// IndexerID is Java's INDEXER_ID log key, which the source-index validations
	// carry (validateOrThrowEx, IndexingBase.java:1171-1174); the state checks do
	// not (:203-207, :237-241), and leave it nil.
	IndexerID uuid.UUID
}

func (e *IndexingValidationError) Error() string { return e.Message }

// UnexpectedReadableError matches Java's IndexingBase.UnexpectedReadableException:
// a build transaction found some (AllReadable false) or all of its targets
// already published, typically by a mutual peer. Under a mutual policy the
// build catcher treats the second as success and falls back to a records scan
// on the first; any other build returns it.
type UnexpectedReadableError struct {
	AllReadable bool
	Message     string
	IndexNames  []string
	IndexStates []IndexState
}

func (e *UnexpectedReadableError) Error() string { return e.Message }

// shouldUsePendingWriteQueue is a fresh-build decision only. Continuing builds
// recover their queue targets from persisted state, not today's request policy.
func (oi *OnlineIndexer) shouldUsePendingWriteQueue(store *FDBRecordStore, index *Index) (bool, error) {
	if !oi.policy.ShouldUsePendingWriteQueue(index) || oi.mutual {
		return false, nil
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return false, err
	}
	return maintainer.IsPendingWriteQueueAllowed() && countVersionColumns(index.RootExpression) == 0 && store.GetFormatVersion() >= formatVersionPendingWrites, nil
}

// checkOpenHeartbeats is the session's open-time preflight. It runs before the
// store's metadata reconciliation, which can rebuild or disable indexes and so
// clear their indexing metadata. When the stored metadata version is current
// no reconciliation will run, and the preflight only refuses legacy and
// malformed heartbeat keys (allowMutual): whether a live peer matters is
// decided by prepareIndexingState once the session knows what it will do, and
// a session that leaves a READABLE index alone or refuses under ERROR must not
// be stopped by a peer. When reconciliation will run, it demands quiescence:
// no live heartbeat but the session's own. Java's indexer reconciles on open
// too, and under a live peer: it opens through the builder's openAsync
// (IndexingBase.java:129-130), whose createOrOpenAsync runs checkVersion
// (FDBRecordStore.java:6015) and so checkPossiblyRebuild (:2689), removing
// former indexes and rebuilding or disabling new ones (:4841-4986) without
// looking at heartbeats. Go refuses that reconciliation until the other
// sessions' leases expire, because it never resets state a live session is
// building.
func (oi *OnlineIndexer) checkOpenHeartbeats(store *FDBRecordStore) error {
	reconciles := int(store.storeHeader.GetMetaDataversion()) != store.metaData.Version()
	return oi.checkHeartbeatAdmission(store, !reconciles)
}

// checkHeartbeatAdmission reads every target's heartbeats without renewing
// them. allowMutual admits live peers and still refuses incompatible keys.
func (oi *OnlineIndexer) checkHeartbeatAdmission(store *FDBRecordStore, allowMutual bool) error {
	heartbeat := oi.sessionHeartbeat
	if heartbeat == nil {
		heartbeat = oi.newHeartbeat("index preparation", oi.buildsMutually(), store.context.Env())
	}
	for _, index := range oi.targetIndexes {
		if err := heartbeat.checkAdmission(store.context.Transaction(), store.subspace, index, allowMutual); err != nil {
			return err
		}
	}
	return nil
}

// indexingSessionStart is what a session does after resolving the index state:
// the three outcomes of Java IndexingBase.handleStateAndDoBuildIndexAsync.
type indexingSessionStart int

const (
	// indexingSessionBuild builds the targets; they are WRITE_ONLY.
	indexingSessionBuild indexingSessionStart = iota
	// indexingSessionSkip neither builds nor publishes: the index is READABLE and
	// the policy continues (Java's shouldBuild false).
	indexingSessionSkip
	// indexingSessionMarkReadable publishes without building (Java MARK_READABLE).
	indexingSessionMarkReadable
)

// prepareIndexingState mirrors IndexingBase.handleStateAndDoBuildIndexAsync's
// state transaction: the policy's desired action for the primary index's state,
// the followers' agreement with it, the clears, the WRITE_ONLY marks and the
// stamp. The returned queue targets become session state only after the caller
// commits.
//
// A READABLE index under the default policy is left alone. Clearing it instead,
// as a fresh build, re-armed WRITE_ONLY over an empty range set under any mutual
// peer that had already finished its fragments, and that peer's publication then
// failed with IndexNotBuiltError.
//
// Heartbeat admission is checked once the action is resolved, and only for a
// session that builds: a mutual session may share a continued build with live
// peers, but a clear or a fresh WRITE_ONLY mark requires that no other session
// is live. Java admits mutual peers by the stamp's method alone; Go refuses to
// reset state under a live peer, whatever the method.
func (oi *OnlineIndexer) prepareIndexingState(store *FDBRecordStore) ([]*Index, indexingSessionStart, error) {
	primary := oi.primaryIndex()
	state, err := store.readIndexState(primary.Name)
	if err != nil {
		return nil, 0, err
	}
	action, err := oi.policy.GetStateDesiredAction(state)
	if err != nil {
		return nil, 0, err
	}
	switch action {
	case DesiredActionError:
		return nil, 0, &IndexingValidationError{Message: "Index state is not as expected", IndexName: primary.Name, IndexVersion: primary.LastModifiedVersion, IndexState: state}
	case DesiredActionMarkReadable:
		return nil, indexingSessionMarkReadable, nil
	}
	shouldClear := action == DesiredActionRebuild
	if !shouldClear && state == IndexStateReadable {
		return nil, indexingSessionSkip, nil
	}
	continued := !shouldClear && state.IsWriteOnly()
	toClear := make(map[string]bool, len(oi.targetIndexes))
	if shouldClear {
		toClear[primary.Name] = true
	}
	for _, index := range oi.targetIndexes {
		target, err := store.readIndexState(index.Name)
		if err != nil {
			return nil, 0, err
		}
		if index == primary {
			continue
		}
		// A follower must share the primary's state, unless its own state's
		// action is REBUILD on a fresh session: then it alone is cleared.
		if target != state {
			targetAction, err := oi.policy.GetStateDesiredAction(target)
			if err != nil {
				return nil, 0, err
			}
			if targetAction != DesiredActionRebuild || continued {
				return nil, 0, &IndexingValidationError{Message: "A target index state doesn't match the primary index state", IndexName: primary.Name, IndexState: state, TargetIndexName: index.Name, TargetIndexState: target}
			}
			toClear[index.Name] = true
		} else if shouldClear {
			toClear[index.Name] = true
		}
	}
	// A BY_INDEX session names its source by name; Java resolves it in the
	// state transaction (IndexingByIndex.getSourceIndex through
	// getIndexingTypeStamp) and a name the metadata lacks is a MetaDataException,
	// which the catcher does not answer, so nothing is stamped or cleared.
	if oi.buildsByIndex() && oi.metaData != nil && oi.metaData.GetIndex(oi.sourceIndex.Name) == nil {
		return nil, 0, &MetaDataError{Message: "Index " + oi.sourceIndex.Name + " not defined"}
	}
	if err := oi.checkHeartbeatAdmission(store, oi.buildsMutually() && continued); err != nil {
		return nil, 0, err
	}
	if !continued {
		for _, index := range oi.targetIndexes {
			queued, err := oi.shouldUsePendingWriteQueue(store, index)
			if err != nil {
				return nil, 0, err
			}
			switch {
			case toClear[index.Name] && queued:
				_, err = store.ClearAndMarkIndexWriteOnlyWithQueue(index.Name)
			case toClear[index.Name]:
				_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
			case queued:
				_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
			default:
				_, err = store.MarkIndexWriteOnly(index.Name)
			}
			if err != nil {
				return nil, 0, err
			}
		}
	}
	if err := oi.setIndexingTypeOrThrow(store, continued, oi.buildIndexingStamp()); err != nil {
		return nil, 0, err
	}
	var queued []*Index
	for _, index := range oi.targetIndexes {
		state, err := store.readIndexState(index.Name)
		if err != nil {
			return nil, 0, err
		}
		if state.IsWriteOnlyWithQueue() {
			queued = append(queued, index)
		}
	}
	if oi.sessionHeartbeat != nil {
		for _, index := range oi.targetIndexes {
			if err := oi.sessionHeartbeat.CheckAndUpdate(store.context.Transaction(), store.subspace, index); err != nil {
				return nil, 0, err
			}
		}
	}
	return queued, indexingSessionBuild, nil
}

// drainPendingIndexWrites gives each queue drain one retry owner and refreshes
// the build session at commit, including the final empty transaction.
func (oi *OnlineIndexer) drainPendingIndexWrites(ctx context.Context, heartbeat *IndexingHeartbeat) error {
	for _, index := range oi.queuedIndexes {
		if err := oi.drainPendingIndexWritesForIndex(ctx, index, heartbeat); err != nil {
			return err
		}
	}
	return nil
}

func (oi *OnlineIndexer) drainPendingIndexWritesForIndex(ctx context.Context, index *Index, heartbeat *IndexingHeartbeat) error {
	if oi.retiredBuildTargets[index.Name] {
		return nil
	}
	if heartbeat == nil {
		return &RecordCoreError{Message: "pending queue drain requires a build session", IndexName: index.Name}
	}
	var store *FDBRecordStore
	iterator := newThrottledRetryingIterator(NewFDBDatabaseRunner(oi.db),
		func(_ context.Context, rc *FDBRecordContext, continuation []byte, limit int) (RecordCursor[*PendingWritesQueueEntry[*gen.PendingWritesQueueEntry]], error) {
			var err error
			store, err = oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			rc.getOrCreateCommitCheck(pendingWriteCommitCheckPrefix(store.subspace)+"heartbeat:"+index.Name, func(string) CommitCheckFunc {
				return func() error {
					state, err := store.readIndexState(index.Name)
					if err != nil {
						return err
					}
					if !state.IsWriteOnlyWithQueue() {
						return &RecordCoreError{Message: "pending write queue index state changed during drain", IndexName: index.Name}
					}
					stamp, err := store.LoadIndexingTypeStamp(index)
					if err != nil {
						return err
					}
					if err := oi.validateBuildStamp(store, index, stamp); err != nil {
						return err
					}
					return oi.refreshFollowupHeartbeats(store, heartbeat)
				}
			})
			props := ForwardScan()
			props.ExecuteProperties.ReturnedRowLimit = limit
			return store.indexingPendingWriteQueue(index, 0).GetQueueCursor(rc, props, continuation), nil
		},
		func(_ context.Context, _ *FDBRecordContext, entry *PendingWritesQueueEntry[*gen.PendingWritesQueueEntry], quota *iterationQuota) error {
			if err := store.replayPendingIndexWrite(index, entry); err != nil {
				return err
			}
			if entry != nil {
				quota.deleted++
			}
			return nil
		})
	iterator.onSuccess = func(*iterationQuota) {
		oi.addMergeRequests(store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes())
	}
	iterator.deletedPerSecond = 10000
	err := iterator.iterateAll(ctx)
	iterator.Close()
	return err
}

// cleanupPendingQueueHeartbeat never borrows a cancelled build context and only
// removes this session's keys. Expiry remains the fallback if cleanup fails.
func (oi *OnlineIndexer) cleanupPendingQueueHeartbeat(heartbeat *IndexingHeartbeat) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = oi.cleanupHeartbeatWithin(ctx, heartbeat)
}

// cleanupHeartbeatWithin owns the retry and commit lifecycle rather than using
// DB.Run, whose dispatched commits intentionally outlive caller cancellation.
// Repeating own-key clears after an unknown commit is safe. Expiry is the
// fallback after a terminal failure or exhaustion of the cleanup deadline.
func (oi *OnlineIndexer) cleanupHeartbeatWithin(ctx context.Context, heartbeat *IndexingHeartbeat) error {
	runner := NewFDBDatabaseRunner(oi.db)
	var err error
	for attempt := 0; attempt < runner.MaxAttempts; attempt++ {
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		if attempt > 0 {
			timer := time.NewTimer(runner.calculateDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		err = oi.cleanupHeartbeatAttempt(ctx, heartbeat)
		if err == nil || !isRetryableError(err) {
			return err
		}
	}
	return err
}

func (oi *OnlineIndexer) cleanupHeartbeatAttempt(ctx context.Context, heartbeat *IndexingHeartbeat) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return &RecordCoreError{Message: "heartbeat cleanup requires a deadline"}
	}
	tx, err := oi.db.CreateWritableTransaction()
	if err != nil {
		return err
	}
	defer tx.Cancel()
	// Each retry receives only the remaining total cleanup budget. The C backend
	// enforces this timeout during commit; the pure-Go detached commit requires
	// the independently bounded readiness wait below.
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if err := tx.Options().SetTimeout(max(int64(1), remaining.Milliseconds())); err != nil {
		return err
	}
	// Each key is READ before it is cleared, so the clear is conditional on
	// nothing having written the key since this transaction's read version. The
	// identity is one per OnlineIndexer (as Java's indexerId is), so the next
	// attempt, BuildIndex or MergeIndexes of this indexer writes the SAME key; a
	// detached commit of this clear that landed after that write would erase a
	// live heartbeat and admit a peer's exclusive session under it. With the read,
	// such a late commit conflicts with the write (or is rejected as too old)
	// instead. Java awaits its clear before the next session starts
	// (IndexingBase.java:174), so it has no late commit to guard against.
	for _, index := range oi.targetIndexes {
		if _, err := tx.Get(heartbeat.heartbeatKey(oi.subspace, index)).Get(); err != nil {
			return err
		}
		heartbeat.Cleanup(tx, oi.subspace, index)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// This transaction contains raw clears only: no record-context hooks or
	// version mutations. Own the commit future and bound the wait independently
	// of backend cancellation behavior after dispatch.
	future := tx.Commit()
	defer future.Cancel()
	// A dispatched pure-Go commit is deliberately detached and its Future.Cancel
	// is a no-op. Do not enter Get until ready: polling the nonblocking readiness
	// contract bounds our wait without abandoning a blocked Get goroutine. The
	// backend may still finish its detached own-key clear after we return; the
	// reads above make it conflict with any write of the key since.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		if future.IsReady() {
			return future.Get()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// validateBuildStamp follows IndexingBase.validateTypeStamp: a missing stamp is
// legacy BY_RECORDS, and ongoing validation compares the method, not ancillary
// takeover-policy fields. A live block always prevents progress.
func (oi *OnlineIndexer) validateBuildStamp(store *FDBRecordStore, index *Index, stamp *gen.IndexBuildIndexingStamp) error {
	expected := oi.buildIndexingStamp()
	if stamp == nil && expected.GetMethod() == gen.IndexBuildIndexingStamp_BY_RECORDS {
		return nil
	}
	if stamp == nil || stamp.GetMethod() != expected.GetMethod() || isTypeStampBlocked(stamp, store.context.Env().Now()) {
		return oi.newPartlyBuiltError(stamp, expected, index, "Indexing stamp had changed")
	}
	return nil
}

// renewSessionHeartbeat separates compatibility admission from periodic mutual
// renewal. Once preparation has committed, Java's mutual path writes only its
// own UUID key: scanning peers here would serialize disjoint range builders.
// Unadmitted callers retain the fail-closed checks, as do all exclusive renewals.
func (oi *OnlineIndexer) renewSessionHeartbeat(store *FDBRecordStore, index *Index, heartbeat *IndexingHeartbeat) error {
	if heartbeat.allowMutual && heartbeat == oi.admittedHeartbeat {
		heartbeat.update(store.context.Transaction(), store.subspace, index)
		return nil
	}
	return heartbeat.CheckAndUpdate(store.context.Transaction(), store.subspace, index)
}

// retireBuildTarget releases follow-up ownership without changing the target
// list used for stamp identity. Publication or observation must commit first:
// a failed transaction cannot release a still-owned target, and a later rebuild
// of a completed target must not become part of this session again.
func (oi *OnlineIndexer) retireBuildTarget(index *Index) {
	if oi.retiredBuildTargets == nil {
		oi.retiredBuildTargets = make(map[string]bool)
	}
	oi.retiredBuildTargets[index.Name] = true
}

// refreshFollowupHeartbeats keeps the whole session alive while sequential
// drains or merges work on one target. Scannable targets have been published
// (possibly by a mutual peer) and no longer own build heartbeats. Active builds
// validate every remaining target's state and stamp, not only the current one.
// Standalone MergeIndexes has no admitted build stamp to validate.
func (oi *OnlineIndexer) refreshFollowupHeartbeats(store *FDBRecordStore, heartbeat *IndexingHeartbeat) error {
	for _, index := range oi.targetIndexes {
		if oi.retiredBuildTargets[index.Name] {
			continue
		}
		state, err := store.readIndexState(index.Name)
		if err != nil {
			return err
		}
		if state.IsScannable() {
			if heartbeat != nil && heartbeat == oi.admittedHeartbeat {
				store.context.AddPostCommit(func() { oi.retireBuildTarget(index) })
			}
			continue
		}
		if !state.IsWriteOnly() {
			// Go-only, and declared (DIVERGENCES.md, "OnlineIndexer session start
			// and build catcher"): a target that is neither scannable nor write-only
			// (disabled under the session) fails this drain or merge transaction.
			// Java's follow-up heartbeat update skips such a target
			// (IndexingBase.java:969-972) and commits; what fails is whatever runs
			// next: a following build transaction's state check
			// (RecordCoreStorageException "Unexpected index state(s)",
			// IndexingThrottle.java:437), or, after the last range, markIndexReadable
			// (IndexNotBuiltException, FDBRecordStore.java:3983). Go refuses at the
			// first transaction that sees the state, with the state check's class
			// and message. Not an IndexingValidationError: that would send a
			// BY_INDEX build to the catcher's records-scan fallback, which Java
			// never takes here.
			return &RecordCoreStorageError{Message: "Unexpected index state(s)", IndexName: index.Name}
		}
		if heartbeat == oi.admittedHeartbeat {
			if err := oi.validateBuildTarget(store, index, heartbeat); err != nil {
				return err
			}
		} else if err := oi.renewSessionHeartbeat(store, index, heartbeat); err != nil {
			return err
		}
	}
	return nil
}

// validateBuildSession runs before every batch, even one whose range
// is already complete. Serializable state/stamp/heartbeat reads fence concurrent
// takeover, disablement, and newly installed blocks.
func (oi *OnlineIndexer) validateBuildSession(store *FDBRecordStore) error {
	if err := oi.expectedIndexStatesOrThrow(store); err != nil {
		return err
	}
	for _, index := range oi.targetIndexes {
		if err := oi.validateBuildTarget(store, index, oi.sessionHeartbeat); err != nil {
			return err
		}
	}
	return nil
}

// expectedIndexStatesOrThrow is Java IndexingThrottle.expectedIndexStatesOrThrow
// (IndexingThrottle.java:410-440), run at the start of every build transaction:
// every target must still be WRITE_ONLY (with or without a queue). A target
// published meanwhile, by a mutual peer or any other process, is an
// UnexpectedReadableError; any other state is a storage error.
func (oi *OnlineIndexer) expectedIndexStatesOrThrow(store *FDBRecordStore) error {
	names := make([]string, len(oi.targetIndexes))
	states := make([]IndexState, len(oi.targetIndexes))
	allWriteOnly, allScannable, allWriteOnlyOrScannable := true, true, true
	for i, index := range oi.targetIndexes {
		state, err := store.readIndexState(index.Name)
		if err != nil {
			return err
		}
		names[i], states[i] = index.Name, state
		allWriteOnly = allWriteOnly && state.IsWriteOnly()
		allScannable = allScannable && state.IsScannable()
		allWriteOnlyOrScannable = allWriteOnlyOrScannable && (state.IsWriteOnly() || state.IsScannable())
	}
	switch {
	case allWriteOnly:
		return nil
	case allScannable:
		return &UnexpectedReadableError{AllReadable: true, Message: "All indexes are built", IndexNames: names, IndexStates: states}
	case allWriteOnlyOrScannable:
		return &UnexpectedReadableError{Message: "Some indexes are built", IndexNames: names, IndexStates: states}
	}
	return &RecordCoreStorageError{Message: "Unexpected index state(s)", IndexName: strings.Join(names, ",")}
}

func (oi *OnlineIndexer) validateBuildTarget(store *FDBRecordStore, index *Index, heartbeat *IndexingHeartbeat) error {
	expected := IndexStateWriteOnly
	for _, queued := range oi.queuedIndexes {
		if queued.Name == index.Name {
			expected = IndexStateWriteOnlyWithQueue
			break
		}
	}
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return err
	}
	if state != expected {
		// Every target is write-only (expectedIndexStatesOrThrow passed), but
		// this one moved between WRITE_ONLY and WRITE_ONLY_WITH_QUEUE since the
		// session started. Java does not check the queue flag here; Go does,
		// because its queued targets are session state, and reports it as the
		// storage error Java's state check raises rather than as a validation
		// error the catcher would answer with a records-scan fallback.
		return &RecordCoreStorageError{Message: "Unexpected index state(s)", IndexName: index.Name}
	}
	stamp, err := store.LoadIndexingTypeStamp(index)
	if err != nil {
		return err
	}
	if err := oi.validateBuildStamp(store, index, stamp); err != nil {
		return err
	}
	if heartbeat != nil {
		return oi.renewSessionHeartbeat(store, index, heartbeat)
	}
	return nil
}

// IndexRangeClaimLostError prevents committing staged index writes when a
// target's range was already claimed. It is a failed batch, not successful
// progress or a reason to commit and jump to another fragment.
type IndexRangeClaimLostError struct {
	IndexName  string
	Begin, End []byte
}

func (e *IndexRangeClaimLostError) Error() string {
	return fmt.Sprintf("index %q build range [%x,%x) was already claimed", e.IndexName, e.Begin, e.End)
}

func insertIndexBuildRange(ranges *IndexingRangeSet, tx fdb.WritableTransaction, index *Index, begin, end []byte) error {
	inserted, err := ranges.InsertRange(tx, begin, end, true)
	if err != nil {
		return err
	}
	if !inserted {
		return &IndexRangeClaimLostError{IndexName: index.Name, Begin: bytes.Clone(begin), End: bytes.Clone(end)}
	}
	return nil
}
