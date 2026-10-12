package client

import (
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
	readState      *Transaction // native GRV owner, sharing only readVersionState
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
	return tx.captureCommitVectors(muts, false, writeConflicts)
}

// captureCommitVectors takes muts as is when the caller owns that vector.
func (tx *Transaction) captureCommitVectors(muts []Mutation, mutsOwned bool, writeConflicts []KeyRange) *commitInput {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if !mutsOwned {
		muts = slices.Clone(muts)
	}
	input := &commitInput{
		db: tx.db, tenantID: tx.tenantId, lockAware: tx.lockAware, isDummy: tx.isDummy,
		span: tx.currentSpan(), tags: append([]string(nil), tx.tags...),
		muts: muts, readConflicts: slices.Clone(tx.readConflicts),
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
	native := &Transaction{
		db: tx.db, readVersionState: tx.readVersionState,
		txOptions: txOptions{
			priority: tx.priority, causalReadRisky: tx.causalReadRisky,
			lockAware: tx.lockAware, readLockAware: tx.readLockAware,
			useGrvCache: tx.useGrvCache, skipGrvCache: tx.skipGrvCache,
			tags: input.tags,
		},
	}
	span := input.span
	native.spanContext.Store(&span)
	input.readState = native
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
