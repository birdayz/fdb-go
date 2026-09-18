package client

import (
	"context"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// commitInput owns every reset-sensitive byte used after commit dispatch.
// Neither the wire wait nor the uncertain-delivery barrier reads the handle.
type commitInput struct {
	db             *database
	muts           []Mutation
	readConflicts  []KeyRange
	writeConflicts []KeyRange
	readVersion    int64
	tenantID       int64
	lockAware      bool
	isDummy        bool
	span           types.SpanContext
	tags           []string
	metricStart    time.Time
	versionstamp   *versionstampCompletion
}

type commitOutcome struct {
	version int64
	batchID uint16
	epoch   int64
}

// captureCommit runs under the caller's execution lease. One byte arena owns
// mutation operands and conflict boundaries, including pooled conflict buffers.
func (tx *Transaction) captureCommit(muts []Mutation, writeConflicts []KeyRange) *commitInput {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	input := &commitInput{
		db: tx.db, tenantID: tx.tenantId, lockAware: tx.lockAware, isDummy: tx.isDummy,
		span: tx.currentSpan(), tags: append([]string(nil), tx.tags...),
		muts: make([]Mutation, len(muts)), readConflicts: make([]KeyRange, len(tx.readConflicts)),
		writeConflicts: make([]KeyRange, len(writeConflicts)),
	}
	tx.readVersionMu.Lock()
	input.readVersion, input.metricStart = tx.readVersion, tx.metricStart
	tx.readVersionMu.Unlock()
	size := 0
	for _, m := range muts {
		size += len(m.Key) + len(m.Value)
	}
	for _, kr := range tx.readConflicts {
		size += len(kr.Begin) + len(kr.End)
	}
	for _, kr := range writeConflicts {
		size += len(kr.Begin) + len(kr.End)
	}
	arena := make([]byte, size)
	copyBytes := func(src []byte) []byte {
		if src == nil {
			return nil
		}
		dst := arena[:len(src):len(src)]
		copy(dst, src)
		arena = arena[len(src):]
		return dst
	}
	for i, m := range muts {
		input.muts[i] = Mutation{Type: m.Type, Key: copyBytes(m.Key), Value: copyBytes(m.Value)}
	}
	for i, kr := range tx.readConflicts {
		input.readConflicts[i] = KeyRange{Begin: copyBytes(kr.Begin), End: copyBytes(kr.End)}
	}
	for i, kr := range writeConflicts {
		input.writeConflicts[i] = KeyRange{Begin: copyBytes(kr.Begin), End: copyBytes(kr.End)}
	}
	return input
}

// publishCommit does not reinterpret the detached operation's outcome. A stale
// completion returns to its caller but cannot publish into or reset a successor.
func (tx *Transaction) publishCommit(inc *readIncarnation, released *executionLease, result commitOutcome) {
	finish, err := tx.beginTurnover(inc, released)
	if err != nil {
		return
	}
	defer finish()
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

// This private entry also serves direct barrier tests; live Commit captures the
// same inputs before releasing its state lease.
func (tx *Transaction) commitDummyTransaction(ctx context.Context) {
	lease := tx.enterState()
	tx.conflictMu.Lock()
	writeConflicts := tx.writeConflicts
	tx.conflictMu.Unlock()
	input := tx.captureCommit(nil, writeConflicts)
	lease.release()
	input.commitDummyTransaction(ctx)
}
