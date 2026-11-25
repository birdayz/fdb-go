package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"time"

	"fdb.dev/internal/fdbwire"
)

// Commit sends mutations to a commit proxy.
// After successful commit, the transaction is automatically reset for reuse
// (mutations and conflict ranges cleared, read version invalidated).
// This preserves Go's auto-reuse extension; C++ RYW API >=410 does not reset
// after commit (ReadYourWrites.actor.cpp:1383-1387,1405-1410).
func (tx *Transaction) Commit(ctx context.Context) error {
	ctx, release, owned := tx.opContextWithLease(ctx, false)
	defer release.release()
	if owned == nil && tx.readOperation(ctx).lease.isActive() {
		if err := tx.commitEntryError(ctx); err != nil {
			return err
		}
		return &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	completion, err := tx.admitCommit(ctx)
	if err != nil {
		return err
	}
	return tx.commitAdmitted(ctx, completion, owned)
}

func (tx *Transaction) commitEntryError(ctx context.Context) error {
	op := tx.readOperation(ctx)
	if op.lease == nil {
		return tx.readEntryError(ctx)
	}
	// The C++ wrapper checks deferredError before entering RYW, including on
	// a canceled or timed-out transaction. Only the mutation snapshot below
	// rechecks concurrent poison; nested GRV work preserves this entry verdict.
	if op.entryErr != nil {
		return op.entryErr
	}
	if terminalCause := tx.readIncarnationCause(op.inc); terminalCause != nil {
		return terminalCause
	}
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	return nil
}

// commitAdmitted receives execution ownership from Commit or the pending
// resolver. Nested read helpers borrow metadata, never this release authority.
func (tx *Transaction) commitAdmitted(ctx context.Context, completion *versionstampCompletion, owned *executionLease) (rerr error) {
	op := tx.readOperation(ctx)
	if err := tx.commitEntryError(ctx); err != nil {
		return err
	}
	if owned == nil && op.lease.isActive() {
		return &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	nativeStarted := false
	defer func() {
		if nativeStarted && rerr != nil {
			completion.finishNative(commitOutcome{}, rerr)
		}
	}()
	if err := tx.drainCommitReads(ctx, op.inc); err != nil {
		return err
	}
	muts, conflicts, err := tx.snapshotAndValidateCommit()
	if err != nil {
		return err
	}
	defer conflicts.release(tx)

	nativeStarted = true
	input, commitStart, commitTok, readOnlyFast, err := tx.buildCommitInput(ctx, muts, &conflicts)
	if err != nil {
		return err
	}
	if readOnlyFast {
		// Read-only transaction — no commit needed.
		// NativeAPI commitMutations resets the committed version to invalidVersion.
		// Preserve Go's auto-reuse behavior; C++ API >=410 does not reset here.
		// RFC-170 (#8): activate pending watches at the READ version (committedVersion is -1 for a
		// no-commit txn — C++ setupWatches' ternary falls back to getReadVersion). Fire BEFORE
		// postCommitReset clears the read version.
		completion.finishNoWrite()
		owned.release()
		tx.publishCommit(op.inc, owned, commitOutcome{version: -1})
		return nil
	}
	return tx.executeCapturedCommit(ctx, op, completion, owned, input, commitStart, commitTok)
}

func (tx *Transaction) drainCommitReads(ctx context.Context, inc *readIncarnation) error {
	// C++ commit() waits on ryw->reading before ANY commit work — before the
	// RYW-disabled branch, the read-only fast path, and the size checks
	// (ReadYourWrites.actor.cpp:1359-1360). That wait is a COMPLETION BARRIER
	// for in-flight reads, not just a sample of past failures: drain any
	// outstanding pipelined reads first (Resolve is idempotent — a later
	// future .Get() returns the memo), so a read whose reply is still in
	// flight fails this commit exactly as it would fail libfdb_c's. Then a
	// read that failed earlier fails the commit with that same error, even if
	// the caller swallowed it (the errored future stays in the AndFuture until
	// reset). Checked after the recorded cause: a fired timebomb sits in
	// resetPromise, which the C++ commit wrapper surfaces before the actor's
	// wait(reading). Returns without resetting — the caller drives
	// OnError/Reset, which clears it (resetRyow :2715).
	tx.readErrMu.Lock()
	var drainFirst *PendingGet
	var drain []*PendingGet
	if tx.pendingReadFirst != nil && len(tx.pendingReads) == 0 {
		drainFirst = tx.pendingReadFirst
	} else if n := tx.pendingReadsCountLocked(); n > 0 {
		drain = make([]*PendingGet, 0, n)
		if tx.pendingReadFirst != nil {
			drain = append(drain, tx.pendingReadFirst)
		}
		for p := range tx.pendingReads {
			drain = append(drain, p)
		}
	}
	tx.readErrMu.Unlock()
	if drainFirst != nil {
		drainFirst.Resolve() //nolint:errcheck // outcome lands in readErr via its tracked tail
	}
	for _, p := range drain {
		p.Resolve() //nolint:errcheck // outcome lands in readErr via its tracked tail
	}
	if err := tx.waitIssuedReads(ctx, inc); err != nil {
		return err
	}
	if err := tx.readIncarnationCause(inc); err != nil {
		return err
	}
	tx.readErrMu.Lock()
	readErr := tx.readErr
	tx.readErrMu.Unlock()
	if readErr != nil {
		return readErr
	}
	return nil
}

func (tx *Transaction) snapshotAndValidateCommit() ([]Mutation, conflictSnapshot, error) {
	// Snapshot the mutation/conflict buffers under conflictMu before reading
	// them. The published contract (fdb/transaction.go) makes Set/Get/Commit
	// safe for concurrent use: a Get future resolving on another goroutine
	// appends conflicts under this lock. The slices are append-only (elements
	// are never mutated in place), so the header snapshot stays valid after
	// release — we only iterate [0:len) and a concurrent append writes beyond it.
	tx.conflictMu.Lock()
	muts := tx.mutations
	// With RYW the write map, its write conflicts and the read-conflict map are
	// frozen with the mutations: writes update them under conflictMu.
	conflicts := tx.snapshotConflictsLocked()
	// Re-read the deferred error UNDER the snapshot lock, linearized with `muts`: the
	// entry check (above) can miss an Atomic(badOp) that races this Commit and stores the poison —
	// under conflictMu — AFTER that entry Load but BEFORE this snapshot. Reading it here, in the same
	// critical section as the mutation snapshot, makes the poison-vs-commit order consistent with the
	// mutation-vs-commit order: a bad Atomic ordered before this snapshot poisons the commit; one
	// ordered after is not in `muts` either, so the commit linearizes before it. Like the entry
	// check, returns without marking the txn errored (C++ leaves it poisoned-but-alive).
	poison := tx.deferredErr.Load()
	tx.conflictMu.Unlock()
	if poison != nil {
		conflicts.release(tx)
		return nil, conflictSnapshot{}, poison
	}

	// A backstop for the RYW write checks deferIllegalLocked records at each call:
	// - set(): if key == metadataVersionKey → client_invalid_operation
	//          if key >= maxWriteKey → key_outside_legal_range
	// - atomicOp(): if key == metadataVersionKey → ONLY SetVersionstampedValue with operand ==
	//               metadataVersionRequiredValue is allowed; anything else →
	//               client_invalid_operation. metadataVersionKey skips the legal-range check.
	//               if key >= maxWriteKey → key_outside_legal_range
	maxWrite := tx.maxWriteKey()
	for _, m := range muts {
		if err := tx.validateMutation(m, maxWrite); err != nil {
			conflicts.release(tx)
			tx.state.Store(int32(txStateErrored))
			return nil, conflictSnapshot{}, err
		}
	}
	return muts, conflicts, nil
}

func (tx *Transaction) buildCommitInput(ctx context.Context, muts []Mutation, conflicts *conflictSnapshot) (*commitInput, time.Time, cacheToken, bool, error) {
	// #28 (RFC-172): ship the COALESCED RYW write map, not the unfolded op-log. libfdb_c commits
	// writeRangeToNativeTransaction's materialized vector (clears-first + per-key folded op stacks) plus
	// coalesced write-conflict ranges, so a txn that hammers one key ships O(distinct keys) mutations, not
	// O(ops) — Go otherwise trips 2101 where libfdb_c/Java commit fine. The backstop validation above stays
	// over `muts` (call order); call-time precedence is deferIllegalLocked's. Only the SIZED+SHIPPED vector is coalesced,
	// and it is derived from the already-validated `muts` SNAPSHOT, so a Set racing this Commit can never
	// ship or be sized unvalidated. The ship-decision (coalesce vs raw; the versionstamp/rywDisabled
	// carve-outs) lives in coalesceCommitVectors so it is unit-testable outside this inline flow.
	shipMuts, mutsOwned, sizeConflicts, conflictsFresh, coalesced := tx.commitVectors(muts, conflicts)
	if len(shipMuts) == 0 && len(sizeConflicts) == 0 {
		return nil, time.Time{}, cacheToken{}, true, nil
	}

	// The read conflicts are sized, checked for self-conflict and shipped as one
	// vector, frozen with the mutations above: C++ materializes its read-conflict
	// map into the native transaction before sizing it (ReadYourWrites.actor.cpp:
	// 1394-1399; NativeAPI.actor.cpp:6818, 6835).
	shipReads := conflicts.readConflicts(tx.rywDisabled)
	// The vectors alias only bytes nobody writes: the pinned version can go.
	conflicts.release(tx)

	// Transaction-size limit (transaction_too_large, 2101), in C++ commitMutations
	// order (NativeAPI.actor.cpp:6797-6836):
	//   - The read-only fast path returns BEFORE the size check (:6800): a txn with NO
	//     mutations AND NO write-conflict ranges is never rejected for size — even one
	//     carrying >10 MB of READ-conflict ranges (which getSize would otherwise count).
	//   - The size check runs AFTER per-mutation validation (key/value size AND the
	//     eager versionstamp-offset check above), so an oversized key/value/bad
	//     versionstamp that also crosses 10 MB reports 2102/2103/2000, not 2101.
	// Size the VALIDATED snapshot `muts` (via approximateCommitSize), not the live buffer:
	// a Set racing this Commit on another goroutine appends beyond `muts` and is not in this
	// commit, so GetApproximateSize() could fail a small commit for an unshipped mutation.
	// C++ ++transactionsCommitStarted in commitMutations (:6808): AFTER the
	// empty fast path (:6800-6806, excluded by the early return above) but
	// BEFORE the size check (~:6835), so a
	// persistently oversized commit shows up as Started-without-Completed.
	// Started-Completed = failed/in-flight (intentional asymmetry). RFC-097.
	if tx.db != nil {
		tx.db.metrics.transactionsCommitStarted.Add(1)
	}

	// C++ checks readOnly after the empty fast path and before the size check (:6810-6811).
	if tx.readOnly {
		tx.state.Store(int32(txStateErrored))
		return nil, time.Time{}, cacheToken{}, false, &wire.FDBError{Code: ErrTransactionReadOnly}
	}

	if tx.sizeLimit > 0 && tx.approximateCommitSize(shipMuts, shipReads, sizeConflicts) > tx.sizeLimit {
		tx.state.Store(int32(txStateErrored))
		return nil, time.Time{}, cacheToken{}, false, &wire.FDBError{Code: 2101} // transaction_too_large
	}

	// makeSelfConflicting: C++ commitMutations (NativeAPI.actor.cpp:6858-6860), placed here — AFTER the
	// read-only fast path and the size check above — exactly as C++ places it after its own read-only
	// return and transaction_too_large check. See maybeMakeSelfConflicting for the full rationale.
	//
	// #28 P1/P2b: the \xff/SC/ range it appends is added AFTER the 2101 gate (so unsized, matching C++
	// makeSelfConflicting at NativeAPI.actor.cpp:6860, which follows the size check at :6836) but MUST ship —
	// pre-#28, the request builder read tx.writeConflicts live (post-SC); the size-time
	// `sizeConflicts` snapshot predates it. The SC DECISION and the ship both work from the frozen
	// writeConflictsSnap (the shipped set), not live tx.writeConflicts: maybeMakeSelfConflicting decides on
	// the snapshot, and finalizeShipConflicts folds JUST the returned range into it (not a
	// live re-read), so the RFC-090 self-conflicting retry detects an already-applied commit WITHOUT shipping
	// a racing Set's unvalidated/unsized conflict range.
	// sizeConflicts covers the same keys as the snapshot (coalesced when the commit coalesces), so the
	// intersection and the shipped vector are those of the snapshot without sorting it again.
	shipReads, sc, scAdded := tx.selfConflictShipReads(sizeConflicts, shipReads)
	shipConflicts := finalizeShipConflicts(sizeConflicts, sc, scAdded, coalesced)
	// Adding the self-conflicting range built a new vector.
	conflictsOwned := scAdded || conflictsFresh

	// No read version yet means no reads, so no causal consistency: a risky GRV, as
	// C++ tryCommit (NativeAPI.actor.cpp:6576-6578). ReadSnapshot=0 would crash the server.
	if tx.isDummy {
		if err := tx.ensureReadVersionFlags(ctx, grvFlagCausalReadRisky); err != nil {
			return nil, time.Time{}, cacheToken{}, false, err
		}
	}

	// Pass the SAME validated snapshot to the marshal so the shipped mutation
	// set is byte-identical to the set we just validated. Without this the
	// marshal re-reads tx.mutations independently, and a Set landing on another
	// goroutine between validation and marshal (allowed by the concurrent-use
	// contract) would ship an UNVALIDATED mutation to the commit proxy.
	//
	// Caller cancellation ends at the commit GRV; resetPromise still ends the
	// public wait without cancelling the retained native actor.
	// Captured together, both BEFORE dispatch: the instant this version's MVCC
	// window opened, and the cache generation it is entitled to publish
	// against. Loading the generation after the commit returns would let a
	// commit dispatched before InvalidateGRVCache install its pre-invalidation
	// version as though it were a post-invalidation observation, and an
	// opted-in reader would then miss the very write the caller invalidated to
	// observe.
	commitStart := time.Now()
	var commitTok cacheToken
	if tx.db != nil {
		commitTok = tx.db.grvCache.token()
	}
	input := tx.captureCommitVectors(shipMuts, mutsOwned, shipConflicts, conflictsOwned, shipReads)
	return input, commitStart, commitTok, false, nil
}

func (tx *Transaction) executeCapturedCommit(ctx context.Context, op *readOperation, completion *versionstampCompletion, owned *executionLease, input *commitInput, commitStart time.Time, commitTok cacheToken) error {
	defer func() {
		if input != nil {
			input.release()
		}
	}()
	input.versionstamp = completion
	if err := tx.readEntryError(ctx); err != nil {
		return err
	}
	if tx.isDummy {
		// The dummy is a child of the native actor, not a new detached caller.
		owned.release()
		result, err := input.commit(ctx)
		input.release()
		if err != nil {
			return err
		}
		input.publishSuccess(commitStart, commitTok, result)
		tx.publishCommit(op.inc, owned, result)
		return nil
	}
	tx.captureNativeReadState(input)
	native, err := tx.startNativeCommit(op.parent, op.inc, completion, input, commitTok)
	if err != nil {
		return err
	}
	input = nil // native now owns the operand borrow
	owned.release()
	<-native.done
	runtime.KeepAlive(tx)
	if native.panicked != nil {
		panic(native.panicked)
	}
	return native.err
}

func (tx *Transaction) stateGetCommittedVersion() (int64, error) {
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	return tx.committedVersion, nil
}

func (tx *Transaction) stateGetVersionstamp(inc *readIncarnation) ([]byte, error) {
	// C++ checks deferredError before RYW's captured resetPromise.
	if e := tx.deferredErr.Load(); e != nil {
		return nil, e
	}
	if tx.hooks != nil && tx.hooks.beforeVersionstampLookup != nil {
		tx.hooks.beforeVersionstampLookup()
	}
	tx.readErrMu.Lock()
	// Turnover clears the current head before draining leases. Only the
	// incarnation captured at admission can classify this getter.
	if inc.cause != nil {
		tx.readErrMu.Unlock()
		return nil, inc.cause
	}
	completion := inc.versionstamp
	if completion == nil {
		completion = tx.lastVersionstamp
	}
	if completion != nil {
		if !completion.ready {
			tx.readErrMu.Unlock()
			return nil, &wire.FDBError{Code: 2015}
		}
		value, err := completion.value()
		tx.readErrMu.Unlock()
		return value, err
	}
	tx.readErrMu.Unlock()
	if !tx.hasCommitted {
		return nil, &wire.FDBError{Code: 2015}
	}
	vs := make([]byte, 10)
	binary.BigEndian.PutUint64(vs[0:8], uint64(tx.committedVersion))
	binary.BigEndian.PutUint16(vs[8:10], tx.txnBatchId)
	return vs, nil
}
