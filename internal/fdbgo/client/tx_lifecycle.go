package client

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// Client constants. These mirror CLIENT_KNOBS in NativeAPI.actor.cpp.
const (
	NoTenantID           int64 = -1
	UnlimitedBytes       int32 = 0x7FFFFFFF
	DefaultRPCTimeout          = 5 * time.Second
	CoordinatorTimeout         = 30 * time.Second // OpenDatabaseCoordRequest
	BootstrapMaxBackoff        = 5 * time.Second  // bootstrap retry backoff cap
	MaxWrongShardRetries       = 50               // C++ is unbounded (relies on tx 5s timeout); 50×10ms = 500ms, generous safety margin
)

// C++ version constants from flow/flow.h.
const (
	LatestVersion  int64 = -2 // C++ latestVersion — used in GetKeyServerLocationsRequest.MinTenantVersion
	InvalidVersion int64 = -1 // C++ invalidVersion — used in GetReadVersionRequest.MaxVersion
)

// Endpoint indices from C++ interface definitions.
// Indices are relative to each interface's base token via getAdjustedEndpoint().
//
// StorageServerInterface (StorageServerInterface.h):
//
//	getValue=0, getKey=1, getKeyValues=2, getShardState=3, waitMetrics=4,
//	splitMetrics=5, getStorageMetrics=6, waitFailure=7, getQueuingMetrics=8,
//	getKeyValueStoreType=9, watchValue=10, getReadHotRanges=11,
//	getRangeSplitPoints=12, getKeyValuesStream=13
//
// CommitProxyInterface (CommitProxyInterface.h):
//
//	commit=0, ..., getKeyServerLocations=2
const (
	EndpointGetValue             = 0  // StorageServerInterface::getValue
	EndpointGetKey               = 1  // StorageServerInterface::getKey
	EndpointGetKeyValues         = 2  // StorageServerInterface::getKeyValues
	EndpointGetShardState        = 3  // StorageServerInterface::getShardState
	EndpointWaitMetrics          = 4  // StorageServerInterface::waitMetrics
	EndpointSplitMetrics         = 5  // StorageServerInterface::splitMetrics
	EndpointGetStorageMetrics    = 6  // StorageServerInterface::getStorageMetrics
	EndpointWaitFailure          = 7  // StorageServerInterface::waitFailure
	EndpointGetQueuingMetrics    = 8  // StorageServerInterface::getQueuingMetrics
	EndpointGetKeyValueStoreType = 9  // StorageServerInterface::getKeyValueStoreType
	EndpointWatchValue           = 10 // StorageServerInterface::watchValue
	EndpointGetReadHotRanges     = 11 // StorageServerInterface::getReadHotRanges
	EndpointGetRangeSplitPoints  = 12 // StorageServerInterface::getRangeSplitPoints
	EndpointGetKeyValuesStream   = 13 // StorageServerInterface::getKeyValuesStream
	// getMappedKeyValues is DECLARED right after getKeyValues
	// (StorageServerInterface.h:103) but assigned getAdjustedEndpoint(14)
	// (:184-185) — it was appended after getKeyValuesStream so the pre-existing
	// indices stayed stable. Taking the index from the declaration order would
	// address getShardState (3) instead.
	EndpointGetMappedKeyValues    = 14 // StorageServerInterface::getMappedKeyValues
	EndpointGetKeyServerLocations = 2  // CommitProxyInterface::getKeyServerLocations
)

type txState int32

const (
	txStateActive txState = iota
	txStateCommitted
	txStateErrored
	txStateCancelled
)

// Transaction represents an FDB transaction.
// Mutations are buffered locally and sent on Commit().
type Transaction struct {
	// Identity & lifecycle — db/isDummy are immutable after construction; state is
	// atomic because Watch goroutines read it concurrently with Commit/Cancel.
	db      *database
	state   atomic.Int32 // txState values
	isDummy bool         // commitDummyTransaction marker; prevents recursive dummies

	// commitEpoch is the cluster epoch captured when commit() bound to a commit
	// proxy — the epoch the committed version actually belongs to, which is not
	// necessarily the one current when the caller entered Commit.
	commitEpoch atomic.Int64

	// Every option-backing field lives on the embedded txOptions (RFC-175 C2);
	// accessors and all field references work unchanged via promotion.
	txOptions

	*readVersionState // shared with native commit; replaced, never cleared, by Reset

	// Commit results — written by the commit path, consumed by
	// GetCommittedVersion/GetVersionstamp after a commit; preserved by
	// postCommitReset so a reused handle can still query them.
	committedVersion int64
	hasCommitted     bool
	txnBatchId       uint16
	lastVersionstamp *versionstampCompletion

	// Mutation & conflict buffers — conflictMu guards all six (the published
	// contract makes data ops concurrent-safe: a pipelined Get future resolving
	// on another goroutine appends read conflicts while Set/Commit run).
	// singleKeyClearCount backs GetApproximateSize's cheaper single-key-clear
	// charge (C++ models it as a range entry, RYW:2431). The slices are
	// append-only, so Commit's header snapshots stay valid after unlock.
	conflictMu          sync.Mutex
	mutations           []Mutation
	readConflicts       []KeyRange
	writeConflicts      []KeyRange
	singleKeyClearCount int
	// With RYW, read conflicts are the coalescing readMap and write conflicts
	// are the write map's node flags (conflict_map.go); rywConflictSize is what
	// GetApproximateSize charges for them, as it would for the lists.
	readMap          pseq[rywRange]
	readMapFinger    pfinger[rywRange]
	rywConflictSize  int64
	conflictBuf      []byte       // backing store for conflict keys and mutation operands
	conflictBufOwner *conflictBuf // pool handle, avoids alloc on Put

	// Retry-loop state — owned by the Run/OnError driver goroutine (never
	// touched by concurrent data ops). creationTime anchors the SetTimeout
	// budget: set on construction, by user Reset(), and stamped lazily by
	// SetTimeout when still zero; NOT reset on OnError retry.
	retryCount   int
	backoff      time.Duration
	creationTime time.Time

	// Wire tracing (RFC-115 §4) — spanContext is stamped on every outgoing
	// request; a fresh one per transaction and per attempt (≈ C++ generateSpanID
	// at cloneAndReset, NativeAPI.actor.cpp:3458). atomic.Pointer with immutable
	// pointees: the commit auto-reset rotates it concurrently with in-flight
	// sends, which capture by value via currentSpan() (RFC-175 E1). traceMu
	// guards the lazily-started OTel span and its parenting context. Tracer
	// callbacks run outside traceMu, readVersionMu, and turnover publication.
	spanContext atomic.Pointer[types.SpanContext]
	// initialSpan is CreateTransaction's span, written before tx is published
	// and never again; later spans are fresh pointees.
	initialSpan   types.SpanContext
	traceMu       sync.Mutex
	txSpan        oteltrace.Span
	traceCtx      context.Context
	traceStarting chan struct{}
	traceClosed   bool

	// deferredErr is THE transaction's deferred error — the exact analog of C++
	// ISingleThreadTransaction::deferredError (ISingleThreadTransaction.h:124): ONE slot
	// shared by every deferred-error source (RFC-175 E2). In libfdb_c, void ops marshal
	// onto the network thread via onMainThreadVoid(f, tr, &deferredError); a throw inside
	// the op is RECORDED first-error-wins (doOnMainThreadVoid skips the op entirely when
	// the slot is already set — ThreadHelper.actor.h:44-55), and every future-returning
	// op re-throws it at entry via checkDeferredError (ThreadSafeTransaction.cpp: get
	// :431, getKey :441, getRange, getReadVersion :421, watch :654, commit :669, the
	// metrics ops, getApproximateSize :715, getVersionstamp). Cleared only on reset
	// (resetRyow, ReadYourWrites.actor.cpp:2719).
	//
	// Go has no network thread — ops run on caller goroutines. The published contract
	// (fdb/transaction.go) makes DATA ops (Set/Get/Atomic/Commit) concurrent-safe while
	// option setters are not; the slot's writers span both classes (Atomic is a data op,
	// SetReadYourWritesDisable an option), so it is an atomic.Pointer[wire.FDBError] —
	// strictly safer than the narrower contract requires, and equivalent to C++'s
	// serialize-everything network thread: written at most once per incarnation (CAS /
	// store-if-unset = first error wins), Load()ed at every gate C++ guards with
	// checkDeferredError (ensureReadVersion covers Get/GetKey/GetRange/GetReadVersion/
	// GetPipelined/Watch; Commit, the metrics paths, GetApproximateSize and
	// GetVersionstamp gate explicitly), Store(nil)ed only on reset.
	//
	// Sources, matching C++: an illegal Set/Clear/Atomic (deferIllegalLocked: RYW
	// set/clear/atomicOp throw in the call, :2228-2241, 2301-2348, 2420); a ClearRange that is
	// inverted (2005, ThreadSafeTransaction.cpp:633-634) or out of range (2004, :2370-2372),
	// both from stateClearRange; an invalid Atomic op-code (2018, or 2004/2000 by key —
	// atomicOp :2226-2235); SetReadYourWritesDisable after a read/write (2000 — RYW setOptionImpl's throw,
	// :2534-2542; the option is NOT applied, matching the C++ throw-before-assign), and
	// SetSizeLimit out of [32, 1e7], including a DB default (2006, extractIntOption).
	// SetTimeout outside [0, INT_MAX] (2006 — extractIntOption's throw, also not applied).
	// An explicit write-conflict range or key past maxWriteKey (2004 — RYW
	// addWriteConflictRange's throw, :2466-2468; see deferWriteConflictRange), and an explicit
	// read-conflict range or key past maxReadKey other than the metadataVersionKey range (2004,
	// :1950-1955; see stateAddReadConflictRange/Key).
	// (readErr is a DIFFERENT C++ mechanism — ryw->reading, the read-future ledger —
	// and lives under readErrMu, which also guards readGen + pendingReads.)
	deferredErr atomic.Pointer[wire.FDBError]

	// readErr: the FIRST error of a TRACKED read this incarnation — the Go analog
	// of C++'s ryw->reading AndFuture. Contract: a failed tracked read — even one
	// whose error the caller swallowed — fails a later Commit with that same error
	// until reset (commit waits on reading before ANY commit work,
	// ReadYourWrites.actor.cpp:1359-1360; resetRyow swaps the AndFuture, :2715).
	// Which reads are tracked vs excluded (metrics ops, eager validation, ctx
	// cancellation) and why watch-setup failures never poison: RFC-098 ("a failed
	// read poisons the transaction's commit" + the completion-barrier addendum).
	//
	// readErrMu guards readErr, readGen and pendingReads: pipelined read
	// futures resolve on other goroutines, and the three fields must move
	// together (a mutex, not the atomic deferred-error contract above).
	resetMu   sync.Mutex
	readReady chan struct{}
	// Lock order: conflictMu -> readErrMu -> {timeoutQueue.mu, designTimerEntry.mu},
	// never the reverse. Both timer locks are leaves; delivery runs without them.
	readErrMu sync.Mutex
	// TIMEOUT is armed for the current read generation, whether or not its
	// incarnation exists (all under readErrMu). timeoutNode is this object's
	// queue entry; timeoutArm numbers arms so a stale delivery is rejected.
	timeoutNode         *timeoutNode
	timeoutArmed        bool
	timeoutNeedsArm     bool
	timeoutArm          uint64
	transactScoped      bool
	transactFinalCommit atomic.Bool
	turnoverErr         wire.FDBError
	turnoverErrUsed     bool
	inlineVs            versionstampCompletion
	inlineVsUsed        bool
	spanNeedsRegen      atomic.Bool
	// designTimer/designEntry/designWeak/designArm/designDeadline: design D4's timer per transaction (timeout_design.go).
	designTimer         *time.Timer
	designEntry         *designTimerEntry
	designWeak          weak.Pointer[Transaction]
	designCleanup       *designWeakCleanup
	designCleanupHandle runtime.Cleanup
	designArm           uint64
	designDeadline      int64
	readLife            *readIncarnation
	readErr             error
	// readGen is the read-tracking incarnation, bumped on every reset. C++
	// swaps the reading AndFuture on resetRyow (:2715): a read issued under an
	// old incarnation that fails AFTER a reset must not poison the new one.
	// PendingGet captures the gen at issue; trackReadErrorGen drops stale
	// recordings.
	readGen uint64
	// pendingReadFirst + pendingReads hold issued-but-unresolved pipelined reads
	// of the CURRENT incarnation. Single-read transactions use pendingReadFirst
	// without allocating a map.
	pendingReadFirst *PendingGet
	pendingReads     map[*PendingGet]struct{}

	// hadRead: set when any read is ISSUED (getValue / getRange / GetPipelined — the chokes
	// every read funnels through, which GetReadVersion and Commit do NOT). Together with a
	// non-empty write map it is the Go analog of C++'s
	// `reading.getFutureCount() > 0 || !cache.empty()` — the signal that
	// SetReadYourWritesDisable must poison. A serverCache check alone misses reads
	// still in flight, before their replies populate the cache.
	//
	// atomic.Bool: pipelined reads (e.g. loadRecordStoreState issuing a Get + a
	// Snapshot().GetRange together) resolve their futures on separate goroutines,
	// each setting hadRead — concurrent writes. The =false resets and the commit-
	// time read run single-goroutine between/after reads, but atomic keeps the
	// whole field race-free.
	hadRead atomic.Bool

	// proxyTagThrottledDuration — accumulated tag-throttle delay consumed by
	// GetTagThrottledDuration and OnError's backoff. Written ONLY by nextBackoff,
	// which adds the client-side proxyMaxTagThrottleDuration constant per
	// proxy_tag_throttled (1223) error (≈ NativeAPI.actor.cpp:7761); the C++
	// reply-side accumulation from the GRV reply's ProxyTagThrottledDuration
	// (NativeAPI.actor.cpp:7410) is NOT implemented — the GRV parsers discard
	// that field (registered gap, TODO.md). Reply-only in C++, never serialized
	// back (CommitProxyInterface.h:318). Single retry-driver goroutine.
	proxyTagThrottledDuration float64

	// Per-watch cancellation (watchMu-guarded). Each in-flight Watch() gets its OWN cancellable
	// context keyed by a monotonic id (newWatchCtx), so an individual watch future's Cancel() scopes
	// to that ONE watch, while Cancel()/reset() cancel only watches not yet handed off by commit
	// (NativeAPI.actor.cpp:6418-6435). The context is captured
	// SYNCHRONOUSLY in WatchSetup (the async WatchPoll only USES it, threaded through), so a concurrent
	// Cancel/reset cancels the exact context the poll holds (no lost-cancellation leak). Entries are
	// removed on watch completion (WatchPoll's deferred scoped cancel) and by the future's Cancel, so
	// the map does not grow across a reused (post-commit) transaction handle.
	watchMu      sync.Mutex
	watchCancels map[uint64]context.CancelFunc
	nextWatchID  uint64

	// watchAct is the per-incarnation commit-completion signal for pending watches (RFC-170 / #8). A watch
	// captures the CURRENT activation at setup; the async facade blocks on it (AwaitWatchCommit) and
	// registers the watch at the COMMITTED version instead of the read version — so a self-write-then-watch
	// stays pending until the NEXT external change rather than firing on the txn's own write. Commit
	// handoff (fireWatchActivation) and teardown (resolveWatchActivationLocked under watchMu) rotate it,
	// so a reset-away watch cannot be armed by a LATER incarnation's commit (the readGen-keying the RFC
	// requires). Lazily created on first Watch/commit via WatchActivation (nil until then — the common
	// watch-free txn pays nothing).
	watchAct atomic.Pointer[watchActivation]

	// ryw: read-your-writes cache. Intercepts reads and merges with pending
	// writes so that Get/GetRange within the same transaction see Set/Clear
	// mutations that haven't been committed yet.
	ryw rywCache

	// rangeReadPanics is used when db is nil (unit tests and benchmarks);
	// rangeReadPanicsInit names it once, as concurrent reads share it.
	rangeReadPanics     panicBackstop
	rangeReadPanicsInit sync.Once

	// conflictingKeys is TransactionState::conflictingKeys: replaced by each in-band
	// conflict reply (NativeAPI.actor.cpp:6700-6717), kept across reset and OnError
	// (cloneAndReset, :3531), read by \xff\xff/transaction/conflicting_keys/.
	conflictingKeys atomic.Pointer[conflictingKeyMap]

	hooks *txTestHooks // nil in production
}

func (tx *Transaction) testHooks() *txTestHooks {
	if tx.hooks == nil {
		tx.hooks = &txTestHooks{}
	}
	return tx.hooks
}

// timeoutDur returns the SetTimeout budget (0 = disabled). Atomic — see the field comment.
func (tx *Transaction) timeoutDur() time.Duration { return time.Duration(tx.timeoutNs.Load()) }

// deadlineTime returns the SetTimeout deadline. Only meaningful while timeoutNs > 0
// (callers gate on it); the zero deadlineNs maps to the epoch, never consulted.
func (tx *Transaction) deadlineTime() time.Time { return time.Unix(0, tx.deadlineNs.Load()) }

// Cancel cancels the transaction. All subsequent operations will return an error.
// This is irreversible — a cancelled transaction cannot be reused.
func (tx *Transaction) Cancel() {
	lease := tx.enterState()
	var span oteltrace.Span
	defer func() {
		lease.release()
		endSpan(span)
	}()
	// Publish the terminal state before either incarnation cancellation or
	// cancelWatches can wake an operation that maps its error through this handle.
	tx.state.Store(int32(txStateCancelled))
	tx.failCapturedIncarnation(lease.inc, &wire.FDBError{Code: ErrTransactionCancelled})
	// Store the cancelled state BEFORE cancelWatches so a watch setup read parked on watchCtx, which
	// cancelWatches is about to unblock, deterministically observes txStateCancelled when it re-checks
	// checkCancelled in watchSetupErr → transaction_cancelled (1025). The store is sequenced-before the
	// context cancellation, whose Done()-close is a happens-before edge to the read goroutine's
	// state.Load(), so the ordering is guaranteed, not racy (the prior order let the unblocked
	// read see the not-yet-cancelled state and surface context.Canceled instead of 1025).
	tx.cancelWatches()
	span = tx.detachTxSpan()
}

// Reset resets the transaction to a clean state, as if newly created from Database.
// Unlike the internal reset() used by OnError (which preserves retryCount/backoff),
// this clears retry state and restores options to database defaults, matching
// C++ ReadYourWritesTransaction::reset(). The last committed version is preserved.
// Updates creationTime so the timeout budget restarts (matches C++ reset() behavior).
//
// Uncommitted Watch() calls are cancelled by Reset(); committed watches survive. Cancelled watches
// surface as context.Canceled rather than the FDBError transaction_cancelled (1025) C++ raises via
// resetPromise.sendError — a known divergence (TODO "watch-path divergences" D5).
func (tx *Transaction) Reset() {
	tx.reset(true)
}

// postCommitReset clears mutation buffers and conflict ranges after a
// successful commit, preserving Go's auto-reuse behavior. C++ RYW API >=410
// does not reset here (ReadYourWrites.actor.cpp:1383-1387,1405-1410).
// Preserves committedVersion and txnBatchId for
// GetCommittedVersion/GetVersionstamp queries.
func (tx *Transaction) postCommitReset() {
	turnover := tx.startTurnover()
	defer endSpan(tx.detachTxSpan())
	defer turnover.finish()
	tx.postCommitResetFields()
}

func (tx *Transaction) postCommitResetFields() {
	tx.state.Store(int32(txStateActive))
	tx.resetReadVersionState(false)
	// Hold conflictMu while clearing ALL three buffers: Watch() goroutines may
	// be concurrently calling AddReadConflictKey() which appends under this lock,
	// and a concurrent Set/Commit reads mutations under it (concurrent-use
	// contract). The mutations clear must be inside too, not above the lock.
	tx.conflictMu.Lock()
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
	tx.singleKeyClearCount = 0 // cleared with mutations (GetApproximateSize accounting)
	tx.readMap, tx.readMapFinger, tx.rywConflictSize = pseq[rywRange]{}, pfinger[rywRange]{}, 0
	tx.dropConflictBufLocked()
	// The write map holds the RYW write conflicts: reset with the lists, under the same lock.
	tx.ryw.reset()
	tx.conflictMu.Unlock()
	tx.deferredErr.Store(nil) // C++ resetRyow clears deferredError (ReadYourWrites.actor.cpp:2719)
	tx.hadRead.Store(false)
	// The caller ends the detached span after publishing the successor.
	if tx.transactFinalCommit.Load() {
		tx.spanNeedsRegen.Store(true)
		tx.spanContext.Store(nil)
	} else {
		tx.regenerateSpan()
	}
	// committedVersion and txnBatchId preserved intentionally.
}

// reset restores the transaction to a fresh state for reuse. userReset distinguishes the two C++ paths
// (RFC-171 / #9/#14): true = user Reset() (persistentOptions.clear() + getTransactionDefaults re-copy →
// persistent options revert to DB defaults); false = OnError retry (resetRyow's applyPersistentOptions →
// per-txn persistent options PRESERVED). Non-persistent options are cleared to DB defaults on both.
func (tx *Transaction) reset(userReset bool) {
	turnover := tx.startTurnover()
	defer endSpan(tx.detachTxSpan())
	defer turnover.finish()
	tx.resetFields(userReset)
}

func (tx *Transaction) resetExpected(userReset bool, expected *readIncarnation, released *executionLease, conflicts []KeyRange) error {
	turnover, err := tx.beginTurnover(expected, released)
	if err != nil {
		return err
	}
	defer endSpan(tx.detachTxSpan())
	defer turnover.finish()
	tx.resetFields(userReset)
	tx.readConflicts = append(tx.readConflicts, conflicts...)
	return nil
}

func (tx *Transaction) resetFields(userReset bool) {
	if userReset {
		tx.retryCount = 0
		tx.backoff = 0
		tx.creationTime = time.Now()
	}
	tx.cancelWatches()
	tx.state.Store(int32(txStateActive))
	tx.resetReadVersionState(!userReset)
	// NativeAPI TransactionState::cloneAndReset preserves the last committed version.
	tx.hasCommitted = false
	tx.txnBatchId = 0
	tx.lastVersionstamp = nil
	// Hold conflictMu: Watch() goroutines may still be running after
	// cancelWatches() (cancel is async — goroutines drain on ctx.Done), and the
	// concurrent-use contract means Set/Commit may touch these under the lock.
	// mutations[:0] and nextWriteNoConflict are written here too — both are read
	// on the Set path under conflictMu, so their clears must be inside the lock.
	tx.conflictMu.Lock()
	tx.nextWriteNoConflict = false // C++ creates fresh state on reset
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
	tx.singleKeyClearCount = 0 // cleared with mutations (GetApproximateSize accounting)
	if tx.conflictBufBorrowedLocked() {
		// A captured commit still reads it: leave it to that commit and the GC.
		tx.conflictBuf, tx.conflictBufOwner = nil, nil
	} else {
		tx.conflictBuf = tx.conflictBuf[:0] // reuse buffer for retry
	}
	tx.readMap, tx.readMapFinger, tx.rywConflictSize = pseq[rywRange]{}, pfinger[rywRange]{}, 0
	// The write map holds the RYW write conflicts: reset with the lists, under the same lock.
	tx.ryw.reset()
	tx.conflictMu.Unlock()
	tx.deferredErr.Store(nil) // C++ resetRyow clears deferredError (ReadYourWrites.actor.cpp:2719)
	tx.hadRead.Store(false)
	// RFC-171 (#9/#14): re-apply the Database-level option defaults. NON-persistent options
	// (readSystemKeys, sizeLimit, priority, rywDisabled, tags, …) are cleared to their DB defaults on BOTH
	// paths; PERSISTENT options (timeout, retryLimit, maxRetryDelay) revert to DB defaults ONLY on a user
	// Reset() and are PRESERVED on the OnError retry — porting C++ resetRyow (options.reset(tr) +
	// applyPersistentOptions) vs user reset()'s persistentOptions.clear()+getTransactionDefaults re-copy.
	// Runs BEFORE the deadline recompute below, which reads the (possibly reset) tx.timeout.
	tx.applyOptionDefaults(userReset)
	// Re-apply timeout from creationTime (NOT time.Now()). C++ semantics:
	// onError does NOT update creationTime, so the timeout is an overall
	// budget across all retries. Only user-facing Reset() updates creationTime.
	// A user Reset() that cleared timeout to the (0) DB default clears the deadline too.
	if tx.timeoutNs.Load() > 0 {
		tx.deadlineNs.Store(tx.creationTime.Add(tx.timeoutDur()).UnixNano())
	} else {
		tx.deadlineNs.Store(0)
	}
	tx.configureReadTimeout()
	// Clear accumulated proxy tag throttle duration on retry (tags themselves are cleared to the DB
	// default by applyOptionDefaults above — they are NON-persistent, C++ TransactionOptions::clear).
	tx.proxyTagThrottledDuration = 0
	// SPAN_PARENT survives retries; the caller ends the old span after publication.
	tx.regenerateSpan()
	// Option lifecycle is now handled by applyOptionDefaults above (RFC-171 / #9,#14): NON-persistent
	// options (priority, causalReadRisky, lockAware, readLockAware, sizeLimit, rywDisabled,
	// snapshotRYWDisableCount, readSystemKeys, writeSystemKeys, bypassUnreadable, tags) are cleared to DB
	// defaults on BOTH paths; PERSISTENT options (timeout, retryLimit, maxRetryDelay) revert to DB defaults
	// on a user Reset() and are preserved on OnError. tenantId (construction-time), creationTime, retryCount
	// and backoff are managed by the callers (Reset()/OnError), not here.
}
