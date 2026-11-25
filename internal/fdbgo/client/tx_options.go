package client

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// txOptions holds every option-backing field of a Transaction — the Go analog of C++
// TransactionOptions + the RYW-level option state (RFC-175 C2). One field per
// fdb_transaction_set_option-style accessor (SetTenantId, SetTimeout, SetRetryLimit, …).
// Embedded in Transaction, so field promotion keeps every existing access site unchanged.
//
// Synchronization contract (unchanged by the move): option setters are NOT
// concurrent-safe with operations (fdb/transaction.go) — fields here are plain unless
// a specific overlap forces more: timeoutNs/deadlineNs are atomics (read outside the
// setters while Reset's sanctioned watch-teardown overlap rewrites them — RFC-175 E1),
// spanParent is an atomic.Pointer (read by regenerateSpan on the commit auto-reset),
// and nextWriteNoConflict is read/cleared under conflictMu on the mutation path.
// Lifecycle: applyOptionDefaults resets the non-persistent fields on both reset paths
// and the persistent trio (timeout/retryLimit/maxRetryDelay) on user Reset only
// (RFC-171).
type txOptions struct {
	// tenantId: if not NoTenantID (-1), all operations are scoped to this
	// tenant's key space. Set via SetTenantId() before any reads/commits.
	tenantId int64

	// Timeout: if non-zero, operations fail with ErrTransactionTimedOut
	// after this duration from creation time (or last user Reset).
	// C++ semantics: the timeout is an overall budget across all retries,
	// NOT per-retry. Internal reset (OnError retry) does NOT restart the timer.
	// Only user-facing Reset() restarts it by updating creationTime.
	// timeoutNs is the budget in ns (0 = disabled); deadlineNs is a unixnano,
	// consulted only while timeoutNs > 0.
	timeoutNs  atomic.Int64
	deadlineNs atomic.Int64

	// Retry limit: if hasRetryLimit is true, OnError will not retry
	// when retryCount >= retryLimit.
	retryLimit    int
	hasRetryLimit bool

	// maxRetryDelay: if > 0, caps the exponential backoff. Default: 1s (maxBackoff).
	// Matches C++ FDB_TR_OPTION_MAX_RETRY_DELAY.
	maxRetryDelay time.Duration

	// nextWriteNoConflict: if true, the next mutation will NOT add a write
	// conflict range. Auto-resets after one mutation. Matches C++
	// FDB_TR_OPTION_NEXT_WRITE_NO_WRITE_CONFLICT_RANGE.
	nextWriteNoConflict bool

	// Transaction priority. Encoded in GRV request Flags field.
	priority TransactionPriority

	// causalReadRisky: if true, FLAG_CAUSAL_READ_RISKY is set in GRV Flags.
	causalReadRisky bool

	// useGrvCache: USE_GRV_CACHE (1101) — opt in to serving this transaction's
	// read version from the database's GRV cache. Default false, matching
	// libfdb_c (NativeAPI.actor.cpp:6148): a default transaction always issues a
	// fresh proxy GRV. skipGrvCache: SKIP_GRV_CACHE (1102) — force a fresh GRV
	// even if useGrvCache is set (skip wins). LOCAL options, never serialized
	// onto the wire. RFC-104.
	useGrvCache  bool
	skipGrvCache bool

	// C++ options.lockAware is lockAware || readLockAware here: both make reads and
	// the GRV lock-aware, and readOnly (READ_LOCK_AWARE without a prior LOCK_AWARE,
	// cleared by LOCK_AWARE) fails a commit with writes 2023 before the wire, so only
	// lockAware reaches the commit flag (NativeAPI.actor.cpp:7071-7091, :6810, :6878).
	lockAware     bool
	readLockAware bool
	readOnly      bool

	// sizeLimit: if > 0, enforced before commit (C++ FDB_TR_OPTION_SIZE_LIMIT). In
	// [32, 10_000_000] on db transactions; 0 only on the fence dummy and bare test txns.
	sizeLimit int64

	// writeConflictsDisabled: when true, ALL mutations skip adding write conflict
	// ranges. Used for insert-only batch writes where all keys are unique (no
	// write-write conflicts possible) and all atomics commute. Reduces commit
	// request size significantly. (Go-only convenience; no C++ option code.)
	writeConflictsDisabled bool

	// rywDisabled: when true, regular Get/GetRange bypass the RYW cache and
	// always read from the server. Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
	rywDisabled bool

	// snapshotRYWDisableCount: snapshot reads bypass the write overlay iff this is > 0.
	// Matches FDB_TR_OPTION_SNAPSHOT_RYW_{ENABLE,DISABLE}, which libfdb_c models as an
	// integer counter (ReadYourWrites.actor.cpp): ENABLE does enabledCount++, DISABLE does
	// enabledCount--, and a snapshot read bypasses RYW iff enabledCount <= 0. We store the
	// disabled-oriented inverse (disableCount = 1 - enabledCount for the modern-API default
	// of 1), so DISABLE does count++, ENABLE does count--, and bypass iff count > 0 —
	// equivalent at every integer while keeping the Go zero value (0) mean "enabled".
	snapshotRYWDisableCount int

	// System key access control. Matches C++ ReadYourWritesTransaction:
	// getMaxReadKey() returns \xff without readSystemKeys, \xff\xff with it.
	// getMaxWriteKey() returns \xff without writeSystemKeys, \xff\xff with it.
	readSystemKeys  bool // READ_SYSTEM_KEYS: allows reading \xff/* keys
	writeSystemKeys bool // ACCESS_SYSTEM_KEYS: allows reading AND writing \xff/* keys

	// reportConflictingKeys: REPORT_CONFLICTING_KEYS (712), shipped as the commit's
	// report_conflicting_keys (NativeAPI.actor.cpp:6887-6889). specialKeySpaceRelaxed:
	// SPECIAL_KEY_SPACE_RELAXED, special-key range reads may span modules (specialkeys.go).
	reportConflictingKeys  bool
	specialKeySpaceRelaxed bool

	// tags: transaction tags for tag-based throttling.
	// Set via SetTag(). Used in backoff calculation for tag_throttled errors.
	// C++ keeps tags across retries (not cleared by internal reset).
	tags []string

	// readTags: the AUTO_THROTTLE_TAG subset of tags. C++ keeps this as a
	// separate TagSet (trState->options.readTags) because only auto-throttle
	// tags ride along on per-storage-server read requests, on those that
	// sampleReadTags selects (read_tags.go).
	readTags []string

	// spanParent, set by SetSpanParent (FDBTransactionOptions::SPAN_PARENT), links the
	// transaction's wire span to a caller-injected parent trace, persisting that
	// linkage across retries (regenerateSpan honors it). atomic.Pointer with immutable
	// pointees — read by regenerateSpan on the commit auto-reset (RFC-115 §4).
	spanParent atomic.Pointer[types.SpanContext]
}

type txTestHooks struct {
	// rpcTimeoutOverride: if > 0, the per-RPC reply timeout for this
	// transaction's READS instead of DefaultRPCTimeout. Test-only knob to
	// drive the read path's reply-timeout retry deterministically; production
	// leaves it 0. (libfdb_c has no per-read client timeout at all; this knob
	// shrinks ours, it never lengthens the observable contract.)
	rpcTimeoutOverride time.Duration

	// afterPendingSend is a per-transaction lifecycle interleaving seam. It
	// runs without transaction locks, after enqueue and before registration.
	afterPendingSend func(*PendingGet)
	// beforeRangeGoroutine parks a started range read's goroutine before it claims the read.
	beforeRangeGoroutine func()
	// beforeResolveKeyResume observes a fallback resolver's unready startup gate.
	beforeResolveKeyResume func(KeySelector)
	// resolveKeyWorkerObserver observes the resolver before it starts its read.
	resolveKeyWorkerObserver func(KeySelector)
	// beforeResolveKeyReplyWait observes the resolver before its reply handoff.
	beforeResolveKeyReplyWait func(KeySelector, <-chan struct{})
	// afterRangeReply parks an issued range read after a storage reply, before
	// it merges the reply or records its conflict.
	afterRangeReply func()
	// beforeSpecialKeyRead parks a routed read after state admission, before its snapshot.
	beforeSpecialKeyRead func()
	// beforeReadConflict pauses a completed read before publishing its conflicts.
	beforeReadConflict func()
	// nativeRange, when set, replaces nativeGetRange under the RYW walk (a test seam
	// for hermetic walk and pin-lifecycle tests).
	nativeRange nativeRangeFunc
	// nativeGet, when set, replaces getValue under Get (a test seam for issued Gets).
	nativeGet func(ctx context.Context, key []byte) ([]byte, error)
	// walkOnRequest, when set, sees each storage request a getKey walk builds (a
	// test seam).
	walkOnRequest func(walkRequest)
	// resolveKeyFn, when set, replaces NativeAPI getKeyImpl (a test seam for
	// read-through GetKey and the range fallback's resolver goroutines).
	resolveKeyFn func(ctx context.Context, sel KeySelector, rv int64) ([]byte, error)
	// beforeIssuedReadWait observes Commit about to wait for n issued reads.
	beforeIssuedReadWait func(n int)
	// afterReadVersion observes a completed GRV before its caller returns.
	afterReadVersion func()
	// beforeReadVersionLock parks acquisition after entry validation.
	beforeReadVersionLock func()
	// beforeReadVersionReject parks rejection before publication under readVersionMu.
	beforeReadVersionReject func()
	// beforeTraceWait observes a captured pending start without transaction locks.
	beforeTraceWait func(context.Context)
	// beforeReadFailureDelivery observes terminal-state publication ordering.
	beforeReadFailureDelivery func(error)
	// afterTimeoutRecord runs after a timeout delivery records its cause and
	// before it cancels the incarnation.
	afterTimeoutRecord func()
	// beforeVersionstampLookup parks the synchronous getter after entry checks.
	beforeVersionstampLookup func()
	// beforeTurnoverRetirement parks conditional turnover before its leaf-lock
	// lifetime claim, with resetMu held but no execution lease or leaf lock.
	beforeTurnoverRetirement func()
	// beforeOnErrorWatchCleanup parks terminal cleanup before claiming watch
	// state; it runs without resetMu or readErrMu.
	beforeOnErrorWatchCleanup func()

	// backoffJitter: if non-nil, replaces rand.Float64() in nextBackoff's jitter.
	// Test-only knob to make the backoff delay deterministic (production leaves it
	// nil → real rand.Float64()).
	backoffJitter func() float64

	// onDesignWeakTimerStop observes weak timeout timer cleanup in tests.
	onDesignWeakTimerStop func(*time.Timer)
}

// TransactionPriority controls the GRV request priority.
// C++ GetReadVersionRequest::Flags encoding.
type TransactionPriority int

const (
	PriorityDefault TransactionPriority = iota
	PriorityBatch
	PrioritySystemImmediate
)

// GRV Flags priority encoding — C++ GetReadVersionRequest enum values.
const (
	grvPriorityDefault         uint32 = 8 << 24  // PRIORITY_DEFAULT
	grvPriorityBatch           uint32 = 1 << 24  // PRIORITY_BATCH
	grvPrioritySystemImmediate uint32 = 15 << 24 // PRIORITY_SYSTEM_IMMEDIATE
	grvFlagCausalReadRisky     uint32 = 1        // FLAG_CAUSAL_READ_RISKY
	grvPriorityMask            uint32 = 0xFF000000
)

func (tx *Transaction) stateSetReadSystemKeys() {
	tx.readSystemKeys = true
}

func (tx *Transaction) stateSetAccessSystemKeys() {
	tx.readSystemKeys = true
	tx.writeSystemKeys = true
}

func (tx *Transaction) stateSetReadVersion(version int64) {
	if tx.deferredErr.Load() != nil {
		return
	}
	tx.readVersionMu.Lock()
	defer tx.readVersionMu.Unlock()
	// Only setter rejection also gates GRV admission under readVersionMu;
	// unrelated deferred poison preserves the operation's captured admission.
	if tx.deferredErr.Load() != nil {
		return
	}
	// NativeAPI::setVersion rejects any admitted GRV future, even after failure,
	// before validating the supplied version. The void C API defers that error.
	if tx.hasReadVersion || tx.readVersionAdmitted || tx.pendingReadVersion != nil {
		if tx.hooks != nil && tx.hooks.beforeReadVersionReject != nil {
			tx.hooks.beforeReadVersionReject()
		}
		err := &wire.FDBError{Code: 2010}
		if tx.deferredErr.CompareAndSwap(nil, err) {
			tx.setterRejection = err
		}
		return
	}
	if version <= 0 {
		if tx.hooks != nil && tx.hooks.beforeReadVersionReject != nil {
			tx.hooks.beforeReadVersionReject()
		}
		err := &wire.FDBError{Code: 2011}
		if tx.deferredErr.CompareAndSwap(nil, err) {
			tx.setterRejection = err
		}
		return
	}
	tx.readVersion = version
	tx.hasReadVersion = true
	tx.userSetReadVersion = true
	tx.pendingReadVersion = nil
	// The first accepted pin has no client-observed MVCC anchor. Existing or
	// pending versions are rejected above, never overwritten by this setter.
	tx.readVersionInstant = time.Time{}
}

// stateReadVersionInstant reports only client-observed anchors for live versions.
// An accepted explicit pin has no anchor; reset retires it through hasReadVersion.
func (tx *Transaction) stateReadVersionInstant() (time.Time, bool) {
	tx.readVersionMu.Lock()
	defer tx.readVersionMu.Unlock()
	if !tx.hasReadVersion || tx.readVersionInstant.IsZero() {
		return time.Time{}, false
	}
	return tx.readVersionInstant, true
}

func (tx *Transaction) stateSetTimeout(ms int64) {
	// doOnMainThreadVoid skips an option on a poisoned transaction; RYW setOptionImpl
	// throws invalid_option_value outside [0, INT_MAX] (extractIntOption) before applying.
	if tx.deferredErr.Load() != nil {
		return
	}
	if ms < 0 || ms > math.MaxInt32 {
		tx.deferredErr.CompareAndSwap(nil, &wire.FDBError{Code: 2006})
		return
	}
	defer tx.configureReadTimeout()
	if ms == 0 {
		tx.timeoutNs.Store(0)
		tx.deadlineNs.Store(0)
		return
	}
	tx.timeoutNs.Store(int64(time.Duration(ms) * time.Millisecond))
	// Deadline is anchored to creationTime, matching C++:
	// timebomb(options.timeoutInSeconds + creationTime, resetPromise)
	if tx.creationTime.IsZero() {
		tx.creationTime = time.Now()
	}
	tx.deadlineNs.Store(tx.creationTime.Add(tx.timeoutDur()).UnixNano())
}

func (tx *Transaction) stateSetRetryLimit(retries int64) {
	if retries < 0 {
		tx.hasRetryLimit = false
		return
	}
	tx.retryLimit = int(retries)
	tx.hasRetryLimit = true
}

func (tx *Transaction) stateSetNextWriteNoWriteConflictRange() {
	tx.nextWriteNoConflict = true
}

func (tx *Transaction) stateSetPriority(p TransactionPriority) {
	tx.priority = p
}

func (tx *Transaction) stateSetCausalReadRisky(v bool) {
	tx.causalReadRisky = v
}

func (tx *Transaction) stateSetUseGrvCache() {
	tx.useGrvCache = true
}

func (tx *Transaction) stateSetSkipGrvCache() {
	tx.skipGrvCache = true
}

func (tx *Transaction) stateSetLockAware() {
	tx.lockAware = true
	tx.readOnly = false
}

func (tx *Transaction) stateSetReadLockAware() {
	tx.readLockAware = true
	if !tx.lockAware {
		tx.readOnly = true
	}
}

func (tx *Transaction) stateLockAware() bool {
	return tx.lockAware
}

func (tx *Transaction) stateReadLockAware() bool {
	return tx.readLockAware
}

func (tx *Transaction) stateSetSizeLimit(limit int64) {
	if limit < 32 || limit > transactionSizeLimit {
		tx.deferredErr.CompareAndSwap(nil, &wire.FDBError{Code: ErrInvalidOptionValue})
		return
	}
	tx.sizeLimit = limit
}

func (tx *Transaction) stateSetMaxRetryDelay(ms int64) {
	tx.maxRetryDelay = time.Duration(ms) * time.Millisecond
}

func (tx *Transaction) stateSetReadYourWritesDisable() {
	if tx.hadRead.Load() || !tx.ryw.isEmpty() {
		// CAS: first deferred error wins (the E2 contract above; C++
		// doOnMainThreadVoid never overwrites deferredError).
		tx.deferredErr.CompareAndSwap(nil, &wire.FDBError{Code: 2000})
		return
	}
	tx.rywDisabled = true
}

func (tx *Transaction) stateSetWriteConflictsDisabled() {
	tx.writeConflictsDisabled = true
}

func (tx *Transaction) stateEnsureMutationCapacity(n int) {
	tx.conflictMu.Lock()
	if cap(tx.mutations) < n {
		newMuts := make([]Mutation, len(tx.mutations), n)
		copy(newMuts, tx.mutations)
		tx.mutations = newMuts
	}
	if cap(tx.writeConflicts) < n {
		newConflicts := make([]KeyRange, len(tx.writeConflicts), n)
		copy(newConflicts, tx.writeConflicts)
		tx.writeConflicts = newConflicts
	}
	tx.conflictMu.Unlock()
}

func (tx *Transaction) stateSetSnapshotRYWDisable() {
	tx.snapshotRYWDisableCount++
}

func (tx *Transaction) stateSetSnapshotRYWEnable() {
	tx.snapshotRYWDisableCount--
}

func (tx *Transaction) stateSnapshotRYWDisableCount() int { return tx.snapshotRYWDisableCount }

func (tx *Transaction) stateSetSnapshotRYWDisableCount(n int) { tx.snapshotRYWDisableCount = n }

func (tx *Transaction) stateBypassUnreadable() bool { return tx.ryw.getBypassUnreadable() }

func (tx *Transaction) stateCausalReadRisky() bool { return tx.causalReadRisky }

func (tx *Transaction) stateSetTenantId(id int64) {
	tx.tenantId = id
}

func (tx *Transaction) stateTenantId() int64 {
	return tx.tenantId
}

// Tag limits, matching C++ CLIENT_KNOBS (ClientKnobs.cpp:290-291). TagSet::addTag
// asserts both fit in a single byte, since the wire encoding length-prefixes each
// tag with one byte and the server counts tags with one byte.
const (
	maxTagsPerTransaction   = 5
	maxTransactionTagLength = 255
)

func (tx *Transaction) stateSetTag(tag string) error {
	if len(tag) > maxTransactionTagLength {
		return &wire.FDBError{Code: 2110} // tag_too_long
	}
	if len(tx.tags) >= maxTagsPerTransaction {
		return &wire.FDBError{Code: 2109} // too_many_tags
	}
	for _, t := range tx.tags {
		if t == tag {
			return nil // already present; C++ pushes only when absent
		}
	}
	tx.tags = append(tx.tags, tag)
	return nil
}

func (tx *Transaction) stateSetAutoThrottleTag(tag string) error {
	if err := tx.stateSetTag(tag); err != nil {
		return err
	}
	// readTags is a distinct TagSet in C++ and gets its own limit check. It can
	// only trail tags, so the length check above already covers it, but the
	// duplicate suppression must be repeated here.
	for _, t := range tx.readTags {
		if t == tag {
			return nil
		}
	}
	tx.readTags = append(tx.readTags, tag)
	return nil
}

func (tx *Transaction) stateTags() []string { return append([]string(nil), tx.tags...) }

func (tx *Transaction) stateReadTags() []string { return append([]string(nil), tx.readTags...) }

// encodeTagSet serializes tags the way TagSet's dynamic_size_traits does
// (TagThrottle.actor.h): each tag as a single length byte followed by its bytes,
// concatenated, with no count prefix. Returns nil for an empty set, so a commit
// leaves its optional tagSet absent (NativeAPI.actor.cpp:6815-6816 assigns it only
// when non-empty); a sampled read sends the empty set present (readTagSample).
func encodeTagSet(tags []string) []byte {
	if len(tags) == 0 {
		return nil
	}
	n := 0
	for _, t := range tags {
		n += 1 + len(t)
	}
	out := make([]byte, 0, n)
	for _, t := range tags {
		out = append(out, byte(len(t)))
		out = append(out, t...)
	}
	return out
}

// grvFlags returns the Flags field for GetReadVersionRequest.
// Encodes priority and option flags into the uint32 bitmask.
func (tx *Transaction) grvFlags() uint32 {
	var flags uint32
	switch tx.priority {
	case PriorityBatch:
		flags |= grvPriorityBatch
	case PrioritySystemImmediate:
		flags |= grvPrioritySystemImmediate
	default:
		flags |= grvPriorityDefault
	}
	if tx.causalReadRisky {
		flags |= grvFlagCausalReadRisky
	}
	return flags
}

// applyOptionDefaults re-applies the Database-level option defaults after a reset (RFC-171 / #9,#14),
// porting C++ resetRyow's options.reset(tr) + applyPersistentOptions (ReadYourWrites.actor.cpp:2699-2727)
// and user reset()'s persistentOptions.clear() + getTransactionDefaults re-copy (:2744-2751). Only
// timeout/retry_limit/max_retry_delay/auth_token are persistent="true" in fdb.options; everything else is
// NON-persistent. Non-persistent options are cleared to DB defaults on BOTH paths; persistent options
// revert to DB defaults ONLY on a user Reset() (userReset), and are PRESERVED on the OnError retry.
// bypassUnreadable is cleared by tx.ryw.reset() (a non-persistent RYW option); deferredErr likewise.
func (tx *Transaction) applyOptionDefaults(userReset bool) {
	if tx.db == nil {
		return
	}
	td := tx.db.defaults()
	// Non-persistent → DB defaults on BOTH reset() and OnError retry. The RYW-level options
	// (readSystemKeys/writeSystemKeys) are zeroed by C++ options.reset(tr) (ReadYourWrites.actor.cpp:
	// 2078-2083); the NativeAPI-level ones (sizeLimit, priority, lockAware, tags, useGrvCache, …) by
	// TransactionOptions::clear on the fresh TransactionState (NativeAPI.actor.cpp:6131-6151). Both are the
	// non-persistent set — cleared regardless of userReset.
	tx.readSystemKeys = td.ReadSystemKeys
	tx.writeSystemKeys = td.AccessSysKeys
	tx.sizeLimit = transactionSizeLimit // C++ has no "disabled" state — default/ceiling is 10 MB
	if td.SizeLimit > 0 {
		tx.stateSetSizeLimit(td.SizeLimit)
	}
	tx.priority = 0
	tx.causalReadRisky = false
	tx.lockAware = false
	tx.readLockAware = false
	tx.readOnly = false
	tx.rywDisabled = false
	tx.snapshotRYWDisableCount = 0
	tx.tags = nil
	tx.readTags = nil
	// useGrvCache/skipGrvCache (USE_GRV_CACHE 1101 / SKIP_GRV_CACHE 1102) are non-persistent, reset by
	// TransactionOptions::clear (NativeAPI.actor.cpp:6148-6149) — leaving them set would serve a stale
	// cached GRV on a retried-or-reset txn where C++ starts fresh. writeConflictsDisabled has no C++
	// option code (a Go-only bulk-insert convenience); it must likewise reset because a fresh C++ txn
	// always adds write conflict ranges, so keeping it set across a retry/reset would silently drop
	// conflicts C++ would keep.
	tx.useGrvCache = false
	tx.skipGrvCache = false
	tx.writeConflictsDisabled = false
	// REPORT_CONFLICTING_KEYS is NativeAPI-level (TransactionOptions::clear, :6143) and
	// SPECIAL_KEY_SPACE_RELAXED RYW-level (options.reset, RYW:2079); the DB default
	// TRANSACTION_REPORT_CONFLICTING_KEYS is re-applied with the persistent options (:2722).
	tx.reportConflictingKeys = td.ReportConflictingKeys
	tx.specialKeySpaceRelaxed = false
	if !userReset {
		return // OnError retry: PRESERVE the per-txn persistent options (timeout / retryLimit / maxRetryDelay).
	}
	// user Reset(): persistent options revert to the DB defaults (C++ persistentOptions.clear() +
	// getTransactionDefaults re-copy, ReadYourWrites.actor.cpp:2744-2751). Route the retry limit through the
	// SAME setter CreateTransaction uses so an unlimited (negative) DB default matches: SetRetryLimit(-1) →
	// hasRetryLimit=false, whereas a raw copy leaves hasRetryLimit=true with retryLimit=-1 and makes the
	// next OnError stop retrying (0 >= -1).
	tx.timeoutNs.Store(int64(time.Duration(td.Timeout) * time.Millisecond)) // 0 → disabled (deadline recomputed by reset())
	if td.HasRetryLimit {
		tx.stateSetRetryLimit(int64(td.RetryLimit))
	} else {
		tx.hasRetryLimit = false // no DB retry-limit default → unlimited
	}
	tx.maxRetryDelay = time.Duration(td.MaxRetryDelay) * time.Millisecond
}

func (tx *Transaction) stateSetBypassUnreadable(v bool) {
	tx.ryw.setBypassUnreadable(v)
}
