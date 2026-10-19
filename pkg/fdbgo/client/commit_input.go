package client

import (
	"bytes"
	"context"
	"slices"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// commitInput owns the vectors and borrows the operand slab used after commit
// dispatch; neither the wire wait nor the uncertain-delivery barrier reads the handle.
type commitInput struct {
	slab           *conflictBuf // borrowed operand storage; see release
	db             *database
	muts           []Mutation
	readConflicts  []KeyRange
	writeConflicts []KeyRange
	readVersion    int64
	readState      *nativeReadState // shared GRV future and captured request options
	tenantID       int64
	lockAware      bool
	isDummy        bool
	span           types.SpanContext
	tags           []string
	metricStart    time.Time
	commitStarted  time.Time
	versionstamp   *versionstampCompletion
}

type commitOutcome struct {
	version int64
	batchID uint16
	epoch   int64
}

// captureCommit runs under the caller's execution lease. Operands stay in the
// borrowed slab or in heap buffers nothing rewrites; vectors, which a retry refills, are copied.
func (tx *Transaction) captureCommit(muts []Mutation, writeConflicts []KeyRange) *commitInput {
	return tx.captureCommitVectors(muts, false, writeConflicts, false)
}

// captureCommitVectors takes muts as is when the caller owns that vector.
// selfConflict says the last read conflict range is the self-conflicting one,
// which C++ adds to the native transaction after the RYW read conflicts.
func (tx *Transaction) captureCommitVectors(muts []Mutation, mutsOwned bool, writeConflicts []KeyRange, selfConflict bool) *commitInput {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !mutsOwned {
		muts = slices.Clone(muts)
	}
	input := &commitInput{
		db: tx.db, tenantID: tx.tenantId, lockAware: tx.lockAware, isDummy: tx.isDummy,
		span: tx.currentSpan(), tags: append([]string(nil), tx.tags...),
		muts: muts, readConflicts: shipReadConflicts(tx.readConflicts, tx.rywDisabled, selfConflict),
		writeConflicts: slices.Clone(writeConflicts),
	}
	tx.readVersionMu.Lock()
	input.readVersion, input.metricStart = tx.readVersion, tx.metricStart
	tx.readVersionMu.Unlock()
	if len(tx.conflictBuf) > 0 {
		if tx.conflictBufOwner == nil {
			// Reset checks the borrow on the handle; an ownerless slab would look free.
			tx.conflictBufOwner = &conflictBuf{}
		}
		tx.conflictBufOwner.borrows.Add(1)
		input.slab = tx.conflictBufOwner
	}
	return input
}

// release ends the borrow once nothing reads the captured bytes. Idempotent.
func (input *commitInput) release() {
	if input.slab != nil {
		input.slab.borrows.Add(-1)
		input.slab = nil
	}
}

// publishCommit does not reinterpret the detached operation's outcome. A stale
// completion returns to its caller but cannot publish into or reset a successor.
func (tx *Transaction) publishCommit(inc *readIncarnation, released *executionLease, result commitOutcome) {
	turnover, err := tx.beginTurnover(inc, released)
	if err != nil {
		return
	}
	defer turnover.finish()
	tx.committedVersion = result.version
	tx.txnBatchId = result.batchID
	tx.commitEpoch.Store(result.epoch)
	tx.hasCommitted = true
	// Retain the latest admitted producer, not necessarily this successful
	// producer. Turnover has drained admission and sealed any pending result.
	tx.readErrMu.Lock()
	tx.lastVersionstamp = inc.versionstamp
	tx.readErrMu.Unlock()
	tx.fireWatchActivation(result.version, false)
	tx.postCommitResetFields()
}

// The native owner shares the existing GRV future, but neither the public
// resetPromise nor a back-reference to the public transaction.
func (tx *Transaction) captureNativeReadState(input *commitInput) {
	tx.readVersionMu.Lock()
	if tx.metricStart.IsZero() {
		tx.metricStart = time.Now()
		tx.ensureTxSpan()
	}
	tx.readVersionMu.Unlock()
	request := tx.readVersionRequest()
	request.span, request.tags = input.span, input.tags
	request.lockAware = tx.lockAware || tx.readLockAware
	input.readState = &nativeReadState{readVersionState: tx.readVersionState, request: request}
}

// Native success updates database-wide observations even if resetPromise already
// ended the public wait (NativeAPI.actor.cpp:6657–6682).
func (input *commitInput) publishSuccess(started time.Time, tok cacheToken, result commitOutcome) {
	if input.db == nil {
		return
	}
	input.db.metrics.transactionsCommitCompleted.Add(1)
	now := time.Now()
	input.db.metrics.observeCommitLatency(now.Sub(started))
	if !input.metricStart.IsZero() {
		input.db.metrics.observeTotalLatency(now.Sub(input.metricStart))
	}
	if result.version > 0 {
		input.db.grvCache.update(tok.withEpoch(result.epoch), started, result.version)
	}
}

// This private entry also serves direct barrier tests; live Commit captures the
// same inputs before releasing its state lease.
func (tx *Transaction) commitDummyTransaction(ctx context.Context) error {
	lease := tx.enterState()
	tx.conflictMu.Lock()
	writeConflicts := tx.writeConflicts
	tx.conflictMu.Unlock()
	input := tx.captureCommit(nil, writeConflicts)
	defer input.release()
	lease.release()
	return input.commitDummyTransaction(ctx)
}

// shipReadConflicts copies the read conflict ranges a commit ships. With RYW,
// C++ keeps them in a CoalescedKeyRefRangeMap<bool> (ReadYourWrites.h:237)
// whose inserts merge with equal neighbours (KeyRangeMap.h:304-350); every
// insert is true, so it ships the sorted union of the read ranges, overlapping
// and adjacent ones merged (ReadYourWrites.actor.cpp:318-350, 1394-1399). The
// self-conflicting range is added to the native transaction afterwards, so it
// stays last. Without RYW the native transaction ships them as added.
func shipReadConflicts(reads []KeyRange, rywDisabled, selfConflict bool) []KeyRange {
	out := slices.Clone(reads)
	if rywDisabled || len(out) == 0 {
		return out
	}
	body, tail := out, []KeyRange(nil)
	if selfConflict {
		body, tail = slices.Clone(out[:len(out)-1]), out[len(out)-1:]
	}
	return append(readConflictMap(body), tail...)
}

// readConflictMap is the read-conflict map's ranges: the sorted union of
// ranges, empty ones dropped (an empty insert does nothing). It reorders ranges.
func readConflictMap(ranges []KeyRange) []KeyRange {
	ranges = slices.DeleteFunc(ranges, func(r KeyRange) bool { return bytes.Compare(r.Begin, r.End) >= 0 })
	return coalesceOwnedWriteConflicts(ranges)
}
