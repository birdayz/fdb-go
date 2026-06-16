package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
	"unsafe"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// commit sends a CommitTransactionRequest to a commit proxy.
// ALL connection errors → commit_unknown_result, matching C++ AtMostOnce::True.
// No distinction between dial/send/response failure — C++ makes zero distinction
// (all are broken_promise → request_maybe_delivered → commit_unknown_result).
//
// On commit_unknown_result, runs commitDummyTransaction as a synchronization
// barrier before returning the error. This matches C++ NativeAPI.actor.cpp
// tryCommit() which calls commitDummyTransaction to confirm the original
// request is no longer in-flight before allowing OnError to retry.
func (tx *Transaction) commit(ctx context.Context, muts []Mutation) error {
	proxy, err := tx.db.getCommitProxy()
	if err != nil {
		return &wire.FDBError{Code: ErrAllProxiesUnreachable}
	}

	conn, err := tx.db.getOrDial(ctx, proxy.Address)
	if err != nil {
		tx.db.handleDialError(ctx, proxy.Address)
		tx.db.kickTopology()
		commitErr := &wire.FDBError{Code: ErrCommitUnknownResult}
		if !tx.isDummy {
			tx.commitDummyTransaction(ctx)
		}
		return commitErr
	}

	replyToken, replyCh, replyHandle := conn.PrepareReply()
	defer replyHandle.Release()
	body, poolBuf := buildCommitTransactionRequest(tx, replyToken, muts)

	// Capture the proxy-change channel BEFORE sending the commit frame.
	// C++ captures onProxiesChanged before dispatch. If we captured after
	// SendFrame, a topology change between send and capture would close
	// the old channel and replace it — we'd get the fresh (unclosed)
	// channel and miss the change.
	proxiesChanged := tx.db.waitProxiesChanged()

	if err := conn.SendFrame(proxy.Token, body); err != nil {
		marshalBufPool.Put(poolBuf)
		replyHandle.Cancel()
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		commitErr := &wire.FDBError{Code: ErrCommitUnknownResult}
		if !tx.isDummy {
			tx.commitDummyTransaction(ctx)
		}
		return commitErr
	}
	// body is copied into WriteFrame's own buffer — safe to return to pool.
	marshalBufPool.Put(poolBuf)

	// Wait for reply or proxy-change (commit_unknown_result either way).
	resp, err := waitReplyOrProxiesChanged(replyCh, ctx, DefaultRPCTimeout, proxiesChanged)
	if err != nil {
		replyHandle.Cancel()
		commitErr := &wire.FDBError{Code: ErrCommitUnknownResult}
		if !tx.isDummy {
			tx.commitDummyTransaction(ctx)
		}
		return commitErr
	}
	if resp.Err != nil {
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		commitErr := &wire.FDBError{Code: ErrCommitUnknownResult}
		if !tx.isDummy {
			tx.commitDummyTransaction(ctx)
		}
		return commitErr
	}

	commitErr := tx.parseCommitReply(resp.Body)
	if commitErr != nil && !tx.isDummy {
		var fdbErr *wire.FDBError
		if errors.As(commitErr, &fdbErr) && (fdbErr.Code == ErrCommitUnknownResult || fdbErr.Code == ErrClusterVersionChanged) {
			tx.commitDummyTransaction(ctx)
		}
	}
	return commitErr
}

// commitDummyTransaction runs a dummy transaction as a synchronization barrier.
// Matches C++ NativeAPI.actor.cpp:commitDummyTransaction (line 6306), called
// from tryCommit (line 6750) after commit_unknown_result.
//
// Purpose: after commit_unknown_result, we don't know if the original commit
// landed. The dummy transaction conflicts with the original (shares a conflict
// key). When the dummy commits successfully, we know the original is no longer
// in-flight at the commit proxy — either it committed or was discarded.
// This is defense-in-depth on top of OnError's self-conflicting mechanism.
//
// The dummy uses the first write conflict key from the original transaction.
// OnError will later copy write→read conflicts, so this key will be in both
// the read and write conflict sets of the retry, ensuring detection.
func (tx *Transaction) commitDummyTransaction(ctx context.Context) {
	// Snapshot conflict slices under conflictMu (concurrent-use contract): a Get
	// future on another goroutine may still be appending read conflicts.
	// Append-only elements → the header snapshots are stable after release.
	tx.conflictMu.Lock()
	writeConflicts := tx.writeConflicts
	readConflicts := tx.readConflicts
	tx.conflictMu.Unlock()
	if len(writeConflicts) == 0 {
		return // no write conflicts → read-only, nothing to synchronize
	}

	// C++ (NativeAPI.actor.cpp:6744) picks a key from the intersection of
	// write and read conflict ranges to minimize false conflicts. If no
	// intersection exists (shouldn't happen since makeSelfConflicting adds
	// a shared range), fall back to the first write conflict key.
	key := intersectConflictRanges(writeConflicts, readConflicts)

	// Retry loop matching C++ commitDummyTransaction's catch/onError pattern.
	// Create a fresh dummy each iteration because OnError/reset clears conflict
	// ranges, and the dummy must always carry the conflict key to serve as a
	// synchronization barrier. Loops until success or context cancellation,
	// matching C++ which loops until the actor is cancelled.
	//
	// C++ uses tr->onError(e) which provides exponential backoff. We replicate
	// the same backoff curve: start at defaultBackoff (10ms), double each retry,
	// cap at DEFAULT_MAX_BACKOFF (1s).
	backoff := defaultBackoff
	dummyRetries := 0
	for {
		if ctx.Err() != nil {
			return // caller gave up, don't block forever
		}

		dummy := &Transaction{
			db:           tx.db,
			tenantId:     NoTenantID, // dummy uses raw access
			creationTime: time.Now(),
			isDummy:      true, // prevents recursive commitDummyTransaction
		}
		// C++ sets RAW_ACCESS, CAUSAL_WRITE_RISKY, LOCK_AWARE on the dummy.
		dummy.writeSystemKeys = true // RAW_ACCESS equivalent
		dummy.readSystemKeys = true  // RAW_ACCESS equivalent
		dummy.causalReadRisky = true // CAUSAL_WRITE_RISKY — faster GRV for dummy
		dummy.lockAware = true

		// C++ commitDummyTransaction (NativeAPI.actor.cpp:6328-6330) adds ONLY a
		// read + write conflict range over the key and commits — it does NOT write
		// any value. The write conflict range alone makes the txn non-no-op
		// (Commit's read-only fast path returns early only when BOTH len(muts)==0
		// AND nWriteConflicts==0, transaction.go:1160), so no value mutation is
		// needed as a "force non-no-op".
		//
		// A dummy.Set(key, "") here is a divergence that CORRUPTS data: `key` is a
		// user conflict key from the original transaction (e.g. a record's unsplit
		// key). Committing SET(key, "") overwrites that record with an empty value,
		// which a later read sees as present-empty — the root cause of the
		// concurrent-ingest "union descriptor does not contain any known record
		// type" deserialize failures.
		dummy.addReadConflictForKey(key)
		dummy.addWriteConflictForKey(key)

		// Use uppercase Commit() which calls ensureReadVersion() before
		// sending the request. Without a read version, ReadSnapshot=0
		// is sent to FDB which crashes the server.
		if err := dummy.Commit(ctx); err != nil {
			var fdbErr *wire.FDBError
			if errors.As(err, &fdbErr) && onErrorRetryable(fdbErr.Code) {
				// Count the dummy's retries like C++: its errors route
				// through tr.onError (NativeAPI.actor.cpp:6341), which ticks
				// the same per-code counters as any transaction. RFC-097.
				dummyRetries++
				tx.db.countRetryAndLog(ctx, fdbErr.Code, dummyRetries)
				if backoffSleep(ctx, jitterBackoff(backoff)) != nil {
					return // ctx cancelled — caller gave up
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			return // non-retryable error, give up
		}
		return // dummy committed successfully — original is no longer in-flight
	}
}

// jitterBackoff returns d scaled by a uniform-random factor in [0.9, 1.1].
// Under coordinated retry storms — multiple clients hitting the same hot
// range simultaneously and all hitting commit_unknown_result — deterministic
// exponential backoff causes thundering herds: every client sleeps the same
// duration and all retries land on the proxy at the same instant. ±10%
// jitter spreads them across a 200ms-wide window at d=1s, breaking the
// lockstep. Cheap (one rand.Float64 per retry) and per-call independent so
// goroutines on the same process also desync.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// Range [0.9, 1.1).
	factor := 0.9 + 0.2*rand.Float64()
	return time.Duration(float64(d) * factor)
}

// onErrorRetryable reports whether OnError retries `code`. It is the SINGLE
// source of the Go client's onError-retryable set — called by both
// Transaction.OnError (as its retryability guard) and commitDummyTransaction's
// retry — so the two can never drift (RFC-105). It equals C++ Transaction::onError's
// retry set (NativeAPI.actor.cpp:7743-7768) PLUS documented Go extensions:
//   - cluster_version_changed (1039): C++ retries it in the MULTI-VERSION layer
//     (MultiVersionTransaction.actor.cpp:1740, updateTransaction+retry), NOT
//     NativeAPI::onError; Go has no MVC layer so OnError owns it — MAYBE_COMMITTED,
//     made idempotency-safe by the self-conflicting deep-copy. Do NOT "fix" this
//     to the literal NativeAPI behavior; it would break cluster-version-change retry.
//   - all_proxies_unreachable (1200): Go-internal Layer-2 error (NOT C++ 1200).
//   - throttled_hot_shard (1235), range_locked (1242): FDB 7.4+, forward-compat.
//
// fdb.IsRetryable is a DIFFERENT predicate (fdb_error_predicate, 12 codes — it
// EXCLUDES 1079/1200/1235/1242 and includes only the C-API contract set); do not
// conflate the two.
func onErrorRetryable(code int) bool {
	switch code {
	case ErrTransactionTooOld, ErrFutureVersion,
		ErrNotCommitted, ErrDatabaseLocked, ErrProcessBehind,
		ErrBatchTransactionThrottled, ErrTagThrottled, ErrProxyTagThrottled,
		ErrThrottledHotShard, ErrRangeLocked, ErrBlobGranuleRequestFailed,
		ErrAllProxiesUnreachable, ErrCommitUnknownResult, ErrClusterVersionChanged,
		ErrProxyMemoryLimitExceeded, ErrGrvProxyMemoryLimit:
		return true
	default:
		return false
	}
}

// intersectConflictRanges finds the begin key of a range that exists in both
// write and read conflict sets. Returns it for use as the dummy transaction's
// conflict key. Falls back to writes[0].Begin if no intersection found
// (shouldn't happen with makeSelfConflicting). Matches C++ intersects()
// at NativeAPI.actor.cpp:6745.
func intersectConflictRanges(writes, reads []KeyRange) []byte {
	for _, w := range writes {
		for _, r := range reads {
			// Two ranges [a,b) and [c,d) overlap iff a < d && c < b.
			if bytes.Compare(w.Begin, r.End) < 0 && bytes.Compare(r.Begin, w.End) < 0 {
				// Return the max of the two begins (start of the overlap).
				if bytes.Compare(w.Begin, r.Begin) >= 0 {
					return w.Begin
				}
				return r.Begin
			}
		}
	}
	// No intersection — fall back to first write conflict key.
	return writes[0].Begin
}

// metadataVersionKey is \xff/metadataVersion — the only key exempt from tenant prefix.
var metadataVersionKey = []byte("\xff/metadataVersion")

// buildCommitTransactionRequest constructs the full request with
// typed mutations and conflict ranges — no pre-serialization blobs.
//
// The zero-copy unsafe cast in buildCommitTransactionRequest depends on
// Mutation and MutationRef having BIT-IDENTICAL memory layouts. A size
// match alone is insufficient — a field reorder that preserves total
// size would break silently. Pin per-field offsets at compile time so
// any future field addition / reorder / type swap fails the build.
var (
	_ [unsafe.Sizeof(Mutation{})]byte         = [unsafe.Sizeof(types.MutationRef{})]byte{}
	_ [unsafe.Offsetof(Mutation{}.Type)]byte  = [unsafe.Offsetof(types.MutationRef{}.MutType)]byte{}
	_ [unsafe.Offsetof(Mutation{}.Key)]byte   = [unsafe.Offsetof(types.MutationRef{}.Param1)]byte{}
	_ [unsafe.Offsetof(Mutation{}.Value)]byte = [unsafe.Offsetof(types.MutationRef{}.Param2)]byte{}
	_ [unsafe.Sizeof(Mutation{}.Type)]byte    = [unsafe.Sizeof(types.MutationRef{}.MutType)]byte{}
	_ [unsafe.Sizeof(Mutation{}.Key)]byte     = [unsafe.Sizeof(types.MutationRef{}.Param1)]byte{}
	_ [unsafe.Sizeof(Mutation{}.Value)]byte   = [unsafe.Sizeof(types.MutationRef{}.Param2)]byte{}
)

// Pool for conflict range slices. Avoids per-commit alloc.
var crSlicePool = sync.Pool{New: func() any { s := make([]types.KeyRangeRef, 0, 8); return &s }}

// Pool for the scratch mutation slice used by the tenant-prefix path. The
// no-tenant path reuses tx.mutations' backing array via the zero-copy cast and
// needs no scratch; only the tenant path must copy (see buildCommitTransactionRequest).
var mutSlicePool = sync.Pool{New: func() any { s := make([]types.MutationRef, 0, 8); return &s }}

// clearAndReturn zeroes a pooled slice and returns it to pool. The clear is
// mandatory for slices whose elements hold []byte references: the mutation
// scratch keeps the (prefixed) key and — for non-ClearRange mutations — the
// original application value buffer (up to 100KB); the conflict-range slices
// keep begin/end key bytes. Without it, sync.Pool pins those buffers reachable
// from the global pool long after the transaction is reset. Clearing the FULL
// capacity (not just len) drops references in slots a larger earlier commit
// populated, so a later smaller commit can't leave stale ones behind.
func clearAndReturn[T any](pool *sync.Pool, s *[]T) {
	full := (*s)[:cap(*s)]
	clear(full)
	*s = full[:0]
	pool.Put(s)
}

// marshalBufPool pools the serialization buffer for CommitTransactionRequest.
// Avoids ~11% of total commit-path allocations. Uses *[]byte to avoid
// interface boxing allocation (same pattern as writeFramePool).
var marshalBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

// buildCommitTransactionRequest constructs the full request. Returns the
// serialized body and a pool handle — caller MUST call releaseMarshalBuf
// after the body is no longer needed (after SendFrame).
func buildCommitTransactionRequest(tx *Transaction, replyToken transport.UID, muts []Mutation) (body []byte, poolBuf *[]byte) {
	// `muts` is the mutation snapshot Commit already validated — marshal exactly
	// it, so the shipped set is byte-identical to the validated set (a Set racing
	// Commit on another goroutine appends to tx.mutations BEYOND this snapshot and
	// is simply not in this commit; it can never be shipped unvalidated).
	//
	// Conflict ranges are not validated, so snapshot their headers here under
	// conflictMu: a Get future resolving on another goroutine appends to
	// readConflicts under this lock, so this reader must take it too. The slices
	// are append-only (elements never mutated in place) and conflictBuf only ever
	// reserves NEW regions or reallocates (never overwrites live bytes), so the
	// header snapshots stay valid after release — no need to hold the lock across
	// marshal. (The one buffer-overwriting op, reset's conflictBuf[:0], cannot
	// overlap a live snapshot in-contract: it runs sequentially after this
	// returns, or via a Reset() the caller must not issue concurrently with a
	// pending Commit — see RFC-049.) Mirrors C++ tryCommit building
	// CommitTransactionRequest once from a stable snapshot.
	tx.conflictMu.Lock()
	readSnap := tx.readConflicts
	writeSnap := tx.writeConflicts
	tx.conflictMu.Unlock()

	// Zero-copy reinterpret: Mutation and MutationRef have identical memory layout
	// (uint8 + []byte + []byte). Avoid copying 200+ mutations per batch.
	mutations := *(*[]types.MutationRef)(unsafe.Pointer(&muts))

	readCRSlice := crSlicePool.Get().(*[]types.KeyRangeRef)
	readCRs := (*readCRSlice)[:0]
	for _, kr := range readSnap {
		readCRs = append(readCRs, types.KeyRangeRef{Begin: kr.Begin, End: kr.End})
	}

	writeCRSlice := crSlicePool.Get().(*[]types.KeyRangeRef)
	writeCRs := (*writeCRSlice)[:0]
	for _, kr := range writeSnap {
		writeCRs = append(writeCRs, types.KeyRangeRef{Begin: kr.Begin, End: kr.End})
	}

	// C++ applyTenantPrefix: when a tenant is set, prepend 8-byte big-endian tenant ID
	// to all mutation keys, read/write conflict range keys. Skip metadataVersionKey.
	//
	// mutScratch is borrowed only on the tenant path. The zero-copy cast above
	// aliases tx.mutations' backing array, so prefixing m.Param1/Param2 in place
	// would corrupt the persistent buffer and double-prefix on any rebuild
	// (e.g. a re-Commit without an intervening reset). C++ avoids this by
	// building a fresh VectorRef<MutationRef> (applyTenantPrefix, NativeAPI.actor.cpp:6523:
	// `updatedMutations`, `withPrefix` allocating, then `req.transaction.mutations = updatedMutations`)
	// and by passing the request to tryCommit by value. We mirror that: copy the
	// mutation headers into a pooled scratch slice and prefix THAT. The conflict
	// ranges already build fresh KeyRangeRef copies above, so they are not aliased.
	var mutScratch *[]types.MutationRef
	if tx.tenantId >= 0 {
		mutScratch = mutSlicePool.Get().(*[]types.MutationRef)
		mutations = append((*mutScratch)[:0], mutations...)

		var prefix [8]byte
		binary.BigEndian.PutUint64(prefix[:], uint64(tx.tenantId))
		for i := range mutations {
			m := &mutations[i]
			if !bytes.Equal(m.Param1, metadataVersionKey) {
				m.Param1 = append(prefix[:], m.Param1...)
				if m.MutType == uint8(MutClearRange) {
					m.Param2 = append(prefix[:], m.Param2...)
				} else if m.MutType == uint8(MutSetVersionstampedKey) {
					// The last 4 bytes of the key are a LE uint32 offset where the
					// versionstamp should be placed. After prepending the tenant
					// prefix, the offset must be adjusted by the prefix length.
					// Matches C++ applyTenantPrefix (NativeAPI.actor.cpp:6533-6536).
					if len(m.Param1) >= 4 {
						off := binary.LittleEndian.Uint32(m.Param1[len(m.Param1)-4:])
						off += 8 // tenant prefix is 8 bytes
						binary.LittleEndian.PutUint32(m.Param1[len(m.Param1)-4:], off)
					}
				}
			}
		}
		for i := range readCRs {
			cr := &readCRs[i]
			if !bytes.Equal(cr.Begin, metadataVersionKey) {
				cr.Begin = append(prefix[:], cr.Begin...)
				cr.End = append(prefix[:], cr.End...)
			}
		}
		for i := range writeCRs {
			cr := &writeCRs[i]
			if !bytes.Equal(cr.Begin, metadataVersionKey) {
				cr.Begin = append(prefix[:], cr.Begin...)
				cr.End = append(prefix[:], cr.End...)
			}
		}
	}

	// C++ CommitTransactionRequest flags (CommitProxyInterface.h):
	//   FLAG_IS_LOCK_AWARE = 0x1 — allows system key writes through resolver
	//   FLAG_FIRST_IN_BATCH = 0x2
	//   FLAG_BYPASS_STORAGE_QUOTA = 0x4
	var flags uint32
	if tx.lockAware {
		flags |= 0x1 // FLAG_IS_LOCK_AWARE
	}
	req := types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{
			ReadConflictRanges:  readCRs,
			WriteConflictRanges: writeCRs,
			Mutations:           mutations,
			ReadSnapshot:        tx.readVersion,
			Lock_aware:          tx.lockAware,
		},
		Flags:       flags,
		Reply:       types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:  types.TenantInfo{TenantId: tx.tenantId},
		SpanContext: tx.spanContext, // RFC-115 §4
	}

	// Marshal with pooled buffer.
	bufp := marshalBufPool.Get().(*[]byte)
	result := req.MarshalFDBPooled(*bufp)
	*bufp = result // track capacity for pool reuse

	// Return pooled slices. Safe after marshal: MarshalFDBPooled copies bytes
	// into result, so the scratch/conflict slices are no longer referenced.
	if mutScratch != nil {
		*mutScratch = mutations
		clearAndReturn(&mutSlicePool, mutScratch)
	}
	*readCRSlice = readCRs
	clearAndReturn(&crSlicePool, readCRSlice)
	*writeCRSlice = writeCRs
	clearAndReturn(&crSlicePool, writeCRSlice)

	return result, bufp
}

// parseCommitReply parses an ErrorOr<CommitID> response.
func (tx *Transaction) parseCommitReply(data []byte) error {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	var reply types.CommitID
	reply.UnmarshalFromReader(&r)
	// A conflict can arrive IN-BAND: with report_conflicting_keys set, the
	// proxy replies CommitID{version: invalidVersion, conflictingKRIndices}
	// instead of an ErrorOr not_committed (CommitProxyServer.actor.cpp:
	// 2448-2466). C++ maps that shape to not_committed (NativeAPI.actor.cpp:
	// 6653 success-gate, :6726 throw). Without this check a conflict-shaped
	// CommitID would read as a SUCCESSFUL commit at version -1.
	if reply.Version == InvalidVersion {
		return &wire.FDBError{Code: ErrNotCommitted}
	}
	tx.committedVersion = reply.Version
	tx.txnBatchId = reply.TxnBatchId
	return nil
}
