package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// ErrNeedFullRYW is returned by GetPipelined when the key has pending atomics
// that require a server read + merge through the full ryw.get() path.
var ErrNeedFullRYW = errors.New("need full RYW path")

// FDB error codes. Source of truth: flow/error_definitions.h + fdb_c.cpp fdb_error_predicate().
const (
	ErrNotCommitted              = 1020 // not_committed (conflict)
	ErrCommitUnknownResult       = 1021 // commit_unknown_result
	ErrTransactionTooOld         = 1007 // transaction_too_old
	ErrFutureVersion             = 1009 // future_version
	ErrWrongShardServer          = 1001 // wrong_shard_server (1062 is change_feed_cancelled — do not confuse)
	ErrTransactionTimedOut       = 1031 // transaction_timed_out (NEVER retryable)
	ErrAccessedUnreadable        = 1036 // accessed_unreadable — read of a pending versionstamped key (NOT retryable; RFC-098)
	ErrProcessBehind             = 1037 // process_behind
	ErrWatchCancelled            = 1029 // watch_cancelled — SS watch limit exceeded; client polls instead
	ErrTooManyWatches            = 1032 // too_many_watches — outstanding-watch cap exceeded (client-side)
	ErrTimedOut                  = 1004 // timed_out — the SS occasionally times out a watch; re-arm
	ErrDatabaseLocked            = 1038 // database_locked
	ErrClusterVersionChanged     = 1039 // cluster_version_changed (MAYBE_COMMITTED)
	ErrProxyMemoryLimitExceeded  = 1042 // commit_proxy_memory_limit_exceeded
	ErrBatchTransactionThrottled = 1051 // batch_transaction_throttled
	ErrGrvProxyMemoryLimit       = 1078 // grv_proxy_memory_limit_exceeded
	ErrBlobGranuleRequestFailed  = 1079 // blob_granule_request_failed (retryable, C++ NativeAPI.actor.cpp)
	ErrTagThrottled              = 1213 // tag_throttled
	ErrProxyTagThrottled         = 1223 // proxy_tag_throttled
	ErrThrottledHotShard         = 1235 // transaction_throttled_hot_shard (FDB 7.4+, future-proof)
	ErrRangeLocked               = 1242 // transaction_rejected_range_locked (FDB 7.4+, future-proof)
	ErrOperationFailed           = 4    // operation_failed (endpoint not supported)
	ErrAllAlternativesFailed     = 1006 // all_alternatives_failed (Layer 2 only)
	ErrAllProxiesUnreachable     = 1200 // Go-internal: all proxies failed at Layer 2 (NOT C++ 1200=recruitment_failed)
	ErrInvertedRange             = 2005 // inverted_range (begin > end)
	ErrRangeLimitsInvalid        = 2012 // range_limits_invalid (e.g. a row limit < -1)
	ErrInvalidMutationType       = 2018 // invalid_mutation_type (a non-atomic op passed to Atomic())
)

// Client constants. These mirror CLIENT_KNOBS in NativeAPI.actor.cpp.
const (
	NoTenantID           int64 = -1
	UnlimitedBytes       int32 = 0x7FFFFFFF
	DefaultRPCTimeout          = 5 * time.Second
	CoordinatorTimeout         = 30 * time.Second // OpenDatabaseCoordRequest + GRV batch context
	BootstrapMaxBackoff        = 5 * time.Second  // bootstrap retry backoff cap
	MaxWrongShardRetries       = 50               // C++ is unbounded (relies on tx 5s timeout); 50×10ms = 500ms, generous safety margin
)

// C++ version constants from flow/flow.h.
const (
	LatestVersion  int64 = -2 // C++ latestVersion — used in GetKeyServerLocationsRequest.MinTenantVersion
	InvalidVersion int64 = -1 // C++ invalidVersion — used in GetReadVersionRequest.MaxVersion
)

// Backoff constants — match C++ CLIENT_KNOBS.
const (
	defaultBackoff                = 10 * time.Millisecond // C++: DEFAULT_BACKOFF
	backoffGrowthRate             = 2.0                   // C++: BACKOFF_GROWTH_RATE
	maxBackoff                    = 1 * time.Second       // C++: DEFAULT_MAX_BACKOFF
	resourceConstrainedMaxBackoff = 30 * time.Second      // C++: RESOURCE_CONSTRAINED_MAX_BACKOFF
	futureVersionDelay            = 10 * time.Millisecond // C++: FUTURE_VERSION_RETRY_DELAY
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
	EndpointGetValue              = 0  // StorageServerInterface::getValue
	EndpointGetKey                = 1  // StorageServerInterface::getKey
	EndpointGetKeyValues          = 2  // StorageServerInterface::getKeyValues
	EndpointGetShardState         = 3  // StorageServerInterface::getShardState
	EndpointWaitMetrics           = 4  // StorageServerInterface::waitMetrics
	EndpointSplitMetrics          = 5  // StorageServerInterface::splitMetrics
	EndpointGetStorageMetrics     = 6  // StorageServerInterface::getStorageMetrics
	EndpointWaitFailure           = 7  // StorageServerInterface::waitFailure
	EndpointGetQueuingMetrics     = 8  // StorageServerInterface::getQueuingMetrics
	EndpointGetKeyValueStoreType  = 9  // StorageServerInterface::getKeyValueStoreType
	EndpointWatchValue            = 10 // StorageServerInterface::watchValue
	EndpointGetReadHotRanges      = 11 // StorageServerInterface::getReadHotRanges
	EndpointGetRangeSplitPoints   = 12 // StorageServerInterface::getRangeSplitPoints
	EndpointGetKeyValuesStream    = 13 // StorageServerInterface::getKeyValuesStream
	EndpointGetKeyServerLocations = 2  // CommitProxyInterface::getKeyServerLocations
)

type txState int32

const (
	txStateActive txState = iota
	txStateCommitted
	txStateErrored
	txStateCancelled
)

// Mutation represents a key-value mutation in a transaction.
type Mutation struct {
	Type  MutationType
	Key   []byte
	Value []byte
}

// MutationType is the type of mutation.
type MutationType uint8

// Mutation types — MUST match C++ MutationRef::Type enum values exactly.
// Wire format uses these values directly. See CommitTransaction.h.
const (
	MutSetValue               MutationType = 0
	MutClearRange             MutationType = 1
	MutAddValue               MutationType = 2
	MutAnd                    MutationType = 6  // C++: And (skips DebugKeyRange=3, DebugKey=4, NoOp=5)
	MutOr                     MutationType = 7  // C++: Or
	MutXor                    MutationType = 8  // C++: Xor
	MutAppendIfFits           MutationType = 9  // C++: AppendIfFits
	MutMax                    MutationType = 12 // C++: Max (skips AvailableForReuse=10, Reserved=11)
	MutMin                    MutationType = 13 // C++: Min
	MutSetVersionstampedKey   MutationType = 14 // C++: SetVersionstampedKey
	MutSetVersionstampedValue MutationType = 15 // C++: SetVersionstampedValue
	MutByteMin                MutationType = 16 // C++: ByteMin
	MutByteMax                MutationType = 17 // C++: ByteMax
	MutMinV2                  MutationType = 18 // C++: MinV2
	MutAndV2                  MutationType = 19 // C++: AndV2
	MutCompareAndClear        MutationType = 20 // C++: CompareAndClear
)

// atomicOpMask mirrors C++ MutationRef::ATOMIC_MASK (CommitTransaction.h:570-572): the bitset of
// op-codes that are valid atomic operations. Any op outside it (SetValue=0, ClearRange=1, the
// reserved/debug codes 3,4,5,10,11, or >=21) is NOT a valid argument to atomicOp.
const atomicOpMask uint32 = (1 << MutAddValue) | (1 << MutAnd) | (1 << MutOr) | (1 << MutXor) |
	(1 << MutAppendIfFits) | (1 << MutMax) | (1 << MutMin) | (1 << MutSetVersionstampedKey) |
	(1 << MutSetVersionstampedValue) | (1 << MutByteMin) | (1 << MutByteMax) | (1 << MutMinV2) |
	(1 << MutAndV2) | (1 << MutCompareAndClear)

// isAtomicOp reports whether op is a valid atomic operation — C++ isValidMutationType &&
// isAtomicOp (CommitTransaction.h:603-611). op-codes >= 32 cannot be in the mask (a non-atomic,
// out-of-range type). atomicOp() rejects anything this returns false for with invalid_mutation_type.
func isAtomicOp(op MutationType) bool {
	return op < 32 && atomicOpMask&(1<<op) != 0
}

// KeyRange represents a range [Begin, End).
type KeyRange struct {
	Begin []byte
	End   []byte
}

// Transaction represents an FDB transaction.
// Mutations are buffered locally and sent on Commit().
type Transaction struct {
	db    *database
	state atomic.Int32 // txState values; atomic because Watch goroutines read concurrently with Commit

	readVersion        int64
	hasReadVersion     bool
	readVersionMu      sync.Mutex // protects readVersion + hasReadVersion from concurrent ensureReadVersion
	userSetReadVersion bool       // true when SetReadVersion was called (needs validateVersion)
	committedVersion   int64
	hasCommitted       bool // true after at least one successful commit
	txnBatchId         uint16

	// conflictMu protects mutations, readConflicts, writeConflicts from concurrent
	// access. The Apple C binding uses a single-threaded actor model. Our Go futures
	// use goroutines, so concurrent Get/Set calls on the same transaction race.
	conflictMu     sync.Mutex
	mutations      []Mutation
	readConflicts  []KeyRange
	writeConflicts []KeyRange
	// singleKeyClearCount counts single-key Clear() calls (NOT ClearRange). C++ charges a
	// single-key clear's mutation part sizeof(KeyRangeRef), not sizeof(MutationRef) (RYW:2431);
	// a single-key clear is shape-indistinguishable from ClearRange(k, k+\0) in tx.mutations, so
	// GetApproximateSize uses this count to apply the cheaper charge. Guarded by conflictMu.
	singleKeyClearCount int
	conflictBuf         []byte       // batch-allocated backing store for conflict range keys
	conflictBufOwner    *conflictBuf // pool handle, avoids alloc on Put

	retryCount int
	backoff    time.Duration

	// tenantId: if not NoTenantID (-1), all operations are scoped to this
	// tenant's key space. Set via SetTenantId() before any reads/commits.
	tenantId int64

	// Timeout: if non-zero, operations fail with ErrTransactionTimedOut
	// after this duration from creation time (or last user Reset).
	// C++ semantics: the timeout is an overall budget across all retries,
	// NOT per-retry. Internal reset (OnError retry) does NOT restart the timer.
	// Only user-facing Reset() restarts it by updating creationTime.
	timeout      time.Duration
	deadline     time.Time
	creationTime time.Time // set on construction and user Reset(), NOT on OnError retry

	// metricStart anchors the RFC-114 total-transaction-latency sample. Kept SEPARATE
	// from creationTime (which anchors the timeout deadline) so the metric boundary
	// moves on commit-reuse without disturbing timeout semantics. Stamped lazily at the
	// transaction's first GRV (ensureReadVersion, ≈ C++ trState->startTime), CLEARED on
	// both reuse boundaries (postCommitReset and user Reset()) so the next transaction
	// re-stamps fresh, but — like creationTime — NOT reset on OnError, so total latency
	// still spans retries (the documented divergence from C++, which resets per attempt).
	metricStart time.Time

	// Distributed-tracing span (RFC-115 §4). spanContext is stamped on every outgoing
	// request (GRV, read, commit, watch); a fresh one is generated per transaction and
	// per attempt (≈ C++ generateSpanID at cloneAndReset, NativeAPI.actor.cpp:3458).
	// spanParent, set by SetSpanParent (FDBTransactionOptions::SPAN_PARENT), links the
	// span to a caller-injected parent trace, persisting that linkage across retries.
	// Written only at construction / reset() / SetSpanParent (never concurrently with a
	// send), and captured by value at send time — so no mutex (like lockAware/tenantId).
	spanContext types.SpanContext
	spanParent  *types.SpanContext

	// OpenTelemetry export (RFC-115 §4 Layer 2). The long-lived "Transaction" otel span
	// (C++ NativeAPI.actor.cpp:6186) + the context that carries it for parenting per-op
	// child spans. Lazily started at the first GRV of a SAMPLED transaction, ended on
	// commit success / reset / Reset / Cancel. Nil for an unsampled tx (the default) — so
	// unsampled txns never touch traceMu and allocate nothing (the C++ NoopTracer effect).
	// traceMu guards both fields: ensureTxSpan (under readVersionMu at first GRV) and
	// startOpSpan (concurrent pipelined reads) and endTxSpan all serialize through it;
	// lock order is readVersionMu → traceMu, never the reverse.
	traceMu  sync.Mutex
	txSpan   oteltrace.Span
	traceCtx context.Context

	// Retry limit: if hasRetryLimit is true, OnError will not retry
	// when retryCount >= retryLimit.
	retryLimit    int
	hasRetryLimit bool

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

	// lockAware: if true, lock_aware is set on both reads and commits.
	// readLockAware: if true, lock_aware is set on reads only (not commits).
	// C++: req.options.lockAware = tr->options.lockAware || tr->options.readLockAware
	//      tr.lock_aware = tr->options.lockAware  (commit path — no readLockAware)
	lockAware     bool
	readLockAware bool

	// sizeLimit: if > 0, enforced before commit. Matches C++ FDB_TR_OPTION_SIZE_LIMIT.
	// Valid range: [32, 10_000_000]. Out-of-range values cause error 2006 at commit.
	sizeLimit int64

	// maxRetryDelay: if > 0, caps the exponential backoff. Default: 1s (maxBackoff).
	// Matches C++ FDB_TR_OPTION_MAX_RETRY_DELAY.
	maxRetryDelay time.Duration

	// rpcTimeoutOverride: if > 0, the per-RPC reply timeout for this
	// transaction's READS instead of DefaultRPCTimeout. Test-only knob to
	// drive the read path's reply-timeout retry deterministically; production
	// leaves it 0. (libfdb_c has no per-read client timeout at all; this knob
	// shrinks ours, it never lengthens the observable contract.)
	rpcTimeoutOverride time.Duration

	// backoffJitter: if non-nil, replaces rand.Float64() in nextBackoff's jitter.
	// Test-only knob to make the backoff delay deterministic (production leaves it
	// nil → real rand.Float64()). Used to pin the cancel-during-backoff race in
	// TestOnError_RespectsContextCancellation without depending on a lucky rand draw
	// (a rand near 0 made the backoff complete before the cancel — a real flake).
	backoffJitter func() float64

	// writeConflictsDisabled: when true, ALL mutations skip adding write conflict
	// ranges. Used for insert-only batch writes where all keys are unique (no
	// write-write conflicts possible) and all atomics commute. Reduces commit
	// request size significantly.
	writeConflictsDisabled bool

	// rywDisabled: when true, regular Get/GetRange bypass the RYW cache and
	// always read from the server. Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
	rywDisabled bool

	// rywPoisonErr: set when SetReadYourWritesDisable is called AFTER a read or write
	// (the RYW layer is non-empty). libfdb_c's ReadYourWritesTransaction throws
	// client_invalid_operation on the network thread, captured into deferredError, so the
	// option call itself succeeds but EVERY subsequent read/commit surfaces the error
	// (RFC-059). When non-nil, every read entry point (via ensureReadVersion) + the metrics
	// path returns it. Cleared on reset (the option is reapplied over an empty layer with no
	// poison; 2000 is non-retryable, so a poisoned commit kills the txn). Read lock-free,
	// like rywDisabled (FDB transactions are not for concurrent use).
	rywPoisonErr error

	// invalidAtomicOpErr: set when Atomic() is called with a non-atomic / out-of-range op-code
	// (C++ atomicOp throws invalid_mutation_type, ReadYourWrites.actor.cpp:2234). The bad mutation
	// is NOT buffered; this deferred error fails the next Commit (2018/2004/2000). An atomic.Pointer
	// (not a plain field) because Atomic() — a data op the published contract allows concurrently
	// with Commit — writes it while Commit reads it (codex). CAS keeps the FIRST invalid op.
	invalidAtomicOpErr atomic.Pointer[wire.FDBError]

	// readErr: the first error returned by a TRACKED read of this transaction —
	// the Go analogue of C++'s ryw->reading AndFuture. commit() waits on reading
	// before any commit work (ReadYourWrites.actor.cpp:1358-1359), and an errored
	// read future stays in the AndFuture forever (add() keeps errored futures,
	// isReady() only pops successful ones — flow/genericactors.actor.h:1912-1942),
	// so a failed read — even one whose error the caller caught and swallowed —
	// fails a later Commit with that same error until the transaction is reset
	// (resetRyow() reading = AndFuture(), :2715). Tracked reads mirror C++'s
	// reading.add sites: get (:1691), getKey (:1707), getRange (:1767),
	// getAddressesForKey (:1849), watch setup (:1290). NOT tracked, matching C++:
	// getEstimatedRangeSizeBytes / getRangeSplitPoints (waitOrError, no
	// reading.add) and eager validation errors (key_outside_legal_range etc.
	// return before a read future exists). Context cancellation is also excluded:
	// a per-read ctx has no C++ analogue (C++ cancellation is whole-transaction
	// via resetPromise), so it must not poison a commit libfdb_c would allow.
	//
	// Watch setup failures are NOT tracked: the C++ watch actor sends
	// done.send(Void()) in every error path before rethrowing
	// (ReadYourWrites.actor.cpp:1299-1302, :1325-1329), so the done future in
	// reading completes SUCCESSFULLY — a failed watch read never poisons
	// commit; reading only barriers on watch-setup completion.
	//
	// readErrMu guards readErr, readGen and pendingReads: pipelined read
	// futures resolve on other goroutines (unlike rywPoisonErr, which only the
	// option path writes).
	readErrMu sync.Mutex
	readErr   error
	// readGen is the read-tracking incarnation, bumped on every reset. C++
	// swaps the reading AndFuture on resetRyow (:2715): a read issued under an
	// old incarnation that fails AFTER a reset must not poison the new one.
	// PendingGet captures the gen at issue; trackReadErrorGen drops stale
	// recordings.
	readGen uint64
	// pendingReads is the set of issued-but-unresolved pipelined reads of the
	// CURRENT incarnation. C++ commit() waits on ryw->reading (:1358) — a
	// completion barrier for in-flight reads, not just a sample of past
	// failures — so Commit drains these (Resolve is idempotent/memoized)
	// before checking readErr. Cleared on reset: C++ detaches old reads with
	// the AndFuture swap.
	pendingReads map[*PendingGet]struct{}

	// hadRead: set when any read is ISSUED (getValue / getRange / GetPipelined — the chokes
	// every read funnels through, which GetReadVersion and Commit do NOT). Together with a
	// non-empty write map it is the Go analog of C++'s
	// `reading.getFutureCount() > 0 || !cache.empty()` — the signal that
	// SetReadYourWritesDisable must poison. A serverCache check alone is insufficient: the
	// facade's Get uses GetPipelined, which does not populate serverCache (RFC-059).
	//
	// atomic.Bool: pipelined reads (e.g. loadRecordStoreState issuing a Get + a
	// Snapshot().GetRange together) resolve their futures on separate goroutines,
	// each setting hadRead — concurrent writes. The =false resets and the commit-
	// time read run single-goroutine between/after reads, but atomic keeps the
	// whole field race-free.
	hadRead atomic.Bool

	// snapshotRYWDisableCount: snapshot reads bypass the RYW cache iff this is > 0.
	// Matches FDB_TR_OPTION_SNAPSHOT_RYW_{ENABLE,DISABLE}, which libfdb_c models as an
	// integer counter (ReadYourWrites.actor.cpp): ENABLE does enabledCount++, DISABLE does
	// enabledCount--, and a snapshot read bypasses RYW iff enabledCount <= 0. We store the
	// disabled-oriented inverse (disableCount = 1 - enabledCount for the modern-API default of
	// 1), so DISABLE does count++, ENABLE does count--, and bypass iff count > 0 — exactly
	// equivalent at every integer (enabledCount <= 0 ⟺ disableCount > 0) while keeping the Go
	// zero value (0) mean "enabled", so a bare Transaction never silently bypasses RYW. This is
	// a persistent option: it is preserved across reset() (re-applied on retry, like C++
	// persistentOptions). By default (count 0) snapshot reads DO go through the RYW cache.
	snapshotRYWDisableCount int

	// System key access control. Matches C++ ReadYourWritesTransaction:
	// getMaxReadKey() returns \xff without readSystemKeys, \xff\xff with it.
	// getMaxWriteKey() returns \xff without writeSystemKeys, \xff\xff with it.
	readSystemKeys  bool // READ_SYSTEM_KEYS: allows reading \xff/* keys
	writeSystemKeys bool // ACCESS_SYSTEM_KEYS: allows reading AND writing \xff/* keys

	// tags: transaction tags for tag-based throttling.
	// Set via SetTag(). Used in backoff calculation for tag_throttled errors.
	// C++ keeps tags across retries (not cleared by internal reset).
	tags []string

	// proxyTagThrottledDuration: accumulated proxy tag throttle delay.
	// Incremented from GRV reply's ProxyTagThrottledDuration field.
	// Reply-only (proxy→client). C++ does not serialize this field in the
	// commit request: "Not serialized, because this field does not need to
	// be sent to master" (CommitProxyInterface.h:318).
	proxyTagThrottledDuration float64

	// isDummy: true for dummy transactions created by commitDummyTransaction.
	// Prevents recursive commitDummyTransaction calls when the dummy itself
	// encounters commit_unknown_result.
	isDummy bool

	// watchCtx/watchCancel: cancellable context for in-flight Watch calls.
	// Created lazily on first Watch(). Cancelled on Reset()/reset() to match
	// C++ resetRyow() which sends transaction_cancelled to pending watches.
	// watchMu guards both fields: getWatchCtx (the synchronous WatchSetup capture)
	// can run concurrently with cancelWatches (Cancel()/reset(), incl. the OnError
	// retry path). The async WatchPoll no longer touches these — it uses the context
	// captured synchronously by WatchSetup (threaded through), so a Cancel/reset
	// always cancels the very context the poll holds (no lost-cancellation leak).
	watchMu     sync.Mutex
	watchCtx    context.Context
	watchCancel context.CancelFunc

	// ryw: read-your-writes cache. Intercepts reads and merges with pending
	// writes so that Get/GetRange within the same transaction see Set/Clear
	// mutations that haven't been committed yet.
	ryw rywCache
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

// Snapshot returns a snapshot view of this transaction.
// Snapshot reads do not add read conflict ranges, so they don't cause
// conflicts with concurrent writers. Same read version, same connection.
func (tx *Transaction) Snapshot() *Snapshot {
	return &Snapshot{tx: tx}
}

// Snapshot wraps a Transaction for conflict-free reads.
// All reads go through the same transaction (same read version, same
// connection pool) but do not add read conflict ranges.
type Snapshot struct {
	tx *Transaction
}

// Get reads a key without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshot RYW is net-disabled
// (snapshotRYWDisableCount > 0).
func (s *Snapshot) Get(ctx context.Context, key []byte) ([]byte, error) {
	// Snapshot reads are tracked in C++ ryw->reading exactly like regular reads
	// (reading.add runs for Snapshot::True too) — a failed snapshot read poisons
	// a later Commit the same way.
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, s.tx.trackReadError(err)
	}
	// Same system key check as regular Get.
	if bytes.Compare(key, s.tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, &wire.FDBError{Code: 2004}
	}
	// Mirror C++ ReadYourWrites.actor.cpp:400-402: readYourWritesDisabled is checked FIRST (read
	// through, for ALL reads incl. snapshot), then the snapshot-RYW counter. Both map to a
	// storage read here (a snapshot read adds no conflict either way).
	if s.tx.rywDisabled || s.tx.snapshotRYWDisableCount > 0 {
		v, err := s.tx.getValue(ctx, key)
		return v, s.tx.trackReadError(err)
	}
	v, err := s.tx.ryw.get(ctx, key, s.tx.getValue)
	return v, s.tx.trackReadError(err)
}

// GetKey resolves a key selector without adding a read conflict range.
// Snapshot reads go through the snapshot cache, and — by default — SEE the txn's own
// pending writes (matching Snapshot.Get/GetRange and libfdb_c, where snapshot RYW is
// enabled unless net-disabled via SetSnapshotRYWDisable). When net-disabled
// (snapshotRYWDisableCount > 0) the write map is bypassed (snapshot cache only).
func (s *Snapshot) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, s.tx.trackReadError(err)
	}
	// Eager validation — NOT tracked (C++ returns before a read future
	// exists), matching Transaction.GetKey.
	if bytes.Compare(selectorKey, s.tx.maxReadKey()) > 0 {
		return nil, &wire.FDBError{Code: 2004}
	}
	// includeWrites mirrors C++ :400-402: consult the RYW write map only when readYourWrites is
	// NOT disabled AND snapshot RYW is net-enabled (count <= 0).
	k, err := s.tx.ryw.getKeyRYW(ctx, selectorKey, orEqual, offset, s.tx.maxReadKey(), !s.tx.rywDisabled && s.tx.snapshotRYWDisableCount <= 0, s.tx.getRange)
	return k, s.tx.trackReadError(err)
}

// GetRange reads a range without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshot RYW is net-disabled
// (snapshotRYWDisableCount > 0).
func (s *Snapshot) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, false, s.tx.trackReadError(err)
	}
	maxKey := s.tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	if limit < -1 { // range_limits_invalid — see getRangeDir
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	if s.tx.rywDisabled || s.tx.snapshotRYWDisableCount > 0 {
		kvs, more, err := s.tx.getRange(ctx, begin, end, limit, false)
		return kvs, more, s.tx.trackReadError(err)
	}
	kvs, more, err := s.tx.ryw.getRange(ctx, begin, end, limit, false, s.tx.getRange)
	return kvs, more, s.tx.trackReadError(err)
}

// GetRangeReverse reads a range in reverse without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshot RYW is net-disabled
// (snapshotRYWDisableCount > 0).
func (s *Snapshot) GetRangeReverse(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, false, s.tx.trackReadError(err)
	}
	maxKey := s.tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	if limit < -1 { // range_limits_invalid — see getRangeDir
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	if s.tx.rywDisabled || s.tx.snapshotRYWDisableCount > 0 {
		kvs, more, err := s.tx.getRange(ctx, begin, end, limit, true)
		return kvs, more, s.tx.trackReadError(err)
	}
	kvs, more, err := s.tx.ryw.getRange(ctx, begin, end, limit, true, s.tx.getRange)
	return kvs, more, s.tx.trackReadError(err)
}

// GetReadVersion returns the read version for this transaction via its snapshot view.
func (s *Snapshot) GetReadVersion(ctx context.Context) (int64, error) {
	return s.tx.GetReadVersion(ctx)
}

// checkCancelled returns transaction_cancelled (1025) if the transaction has been cancelled.
// C++ cancel() does resetPromise.sendError(transaction_cancelled) (ReadYourWrites.actor.cpp:2730)
// and EVERY RYW op races resetPromise, so a cancelled txn resolves every op with 1025. This is
// the Go analogue of that per-op resetPromise check: reads route through ensureReadVersion (which
// calls this), and ops that bypass ensureReadVersion (metrics, OnError, GetVersionstamp, Commit)
// call it directly at entry. Apps branch on err.Code == 1025 (RFC-068), so the code must match.
func (tx *Transaction) checkCancelled() error {
	if txState(tx.state.Load()) == txStateCancelled {
		return &wire.FDBError{Code: 1025} // transaction_cancelled
	}
	return nil
}

// trackReadError records err as this transaction's first failed read (see the
// readErr field — the C++ ryw->reading analogue, which fails a later Commit
// with the same error). Returns err unchanged so read tails can
// `return v, tx.trackReadError(err)`. Synchronous reads run inside the current
// incarnation by construction; asynchronous resolvers must use
// trackReadErrorGen with their captured generation instead.
func (tx *Transaction) trackReadError(err error) error {
	// Nil/ctx check BEFORE the lock: every successful read funnels through
	// here — taking readErrMu on the hot path would serialize concurrent
	// pipelined reads on the mutex that also guards pendingReads.
	if !isTrackableReadError(err) {
		return err
	}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if tx.readErr == nil {
		tx.readErr = err
	}
	return err
}

// trackReadErrorGen is trackReadError for reads issued under generation gen:
// the recording is dropped if the transaction has been reset since (C++ swaps
// the reading AndFuture on resetRyow, detaching in-flight reads).
func (tx *Transaction) trackReadErrorGen(err error, gen uint64) error {
	if !isTrackableReadError(err) {
		return err
	}
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if gen == tx.readGen && tx.readErr == nil {
		tx.readErr = err
	}
	return err
}

// isTrackableReadError: ctx cancellation has no C++ analogue (cancellation is
// whole-transaction via resetPromise) and must not poison commit.
func isTrackableReadError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (tx *Transaction) ensureReadVersion(parentCtx context.Context) error {
	if err := tx.checkCancelled(); err != nil {
		return err
	}
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	// Bound the GRV by the SetTimeout deadline too: the GRV is the first read RPC
	// every transaction issues, and a hung-but-alive GRV proxy must not run past
	// the timeout (RFC-112; the C++ analog is RYWImpl::getReadVersion's
	// `choose { getReadVersion() | resetPromise }`, ReadYourWrites.actor.cpp:1537).
	// A deadline-cancelled GRV is surfaced as transaction_timed_out via mapTimeout.
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	// A transaction poisoned by SetReadYourWritesDisable-after-an-op surfaces
	// client_invalid_operation on every subsequent read AND commit (RFC-059). This is the
	// single uniform gate: all reads (regular + snapshot), Commit, and GetReadVersion fetch a
	// read version through here — libfdb_c poisons all of them identically (verified
	// differentially, incl. GetReadVersion). The metrics path bypasses this and is gated
	// separately. (Cleared on reset.)
	if tx.rywPoisonErr != nil {
		return tx.rywPoisonErr
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}
	tx.readVersionMu.Lock()
	if tx.metricStart.IsZero() {
		// RFC-114 total-latency anchor: stamp at this transaction's FIRST GRV (the
		// first read, or the commit-path GRV for a write-only txn) — ≈ C++
		// trState->startTime, set at getReadVersion. Cleared on commit-reuse
		// (postCommitReset), NOT on OnError, so it spans retries but excludes the
		// idle gap before a reused handle's next transaction begins.
		tx.metricStart = time.Now()
		// RFC-115 §4 Layer 2: start the "Transaction" otel span at the same first-GRV
		// anchor (single-entry, under readVersionMu). No-op unless sampled + a real tracer.
		tx.ensureTxSpan()
	}
	if !tx.hasReadVersion {
		flags := tx.grvFlags()
		rv, locked, err := tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags, tx.spanContext, tx.useGrvCache, tx.skipGrvCache)
		if err != nil {
			tx.readVersionMu.Unlock()
			return tx.mapTimeout(parentCtx, err)
		}
		// Database-lock enforcement — the C++ extractReadVersion analog
		// (NativeAPI.actor.cpp:7425-7426): a locked database refuses reads
		// from transactions that are not lock-aware. Both LOCK_AWARE and
		// READ_LOCK_AWARE set C++'s options.lockAware (:7077-7091). Checked
		// BEFORE the version is adopted, like C++'s throw. database_locked
		// (1038) is OnError-retryable, so a Run loop retries (refetching a
		// GRV) until the lock is released or the budget ends — same as C++.
		if locked && !(tx.lockAware || tx.readLockAware) {
			tx.readVersionMu.Unlock()
			return &wire.FDBError{Code: ErrDatabaseLocked}
		}
		tx.readVersion = rv
		tx.hasReadVersion = true
	}
	// C++ DatabaseContext::validateVersion() on a user-set read version: reject a
	// version below the smallest-seen floor (genuinely ancient) or an absurd
	// future. Post-RFC-104 the floor is the SMALLEST version seen (see
	// updateMinAcceptable), so a recent pinned version is never spuriously
	// rejected. If the client hasn't seen any version yet, a GRV establishes the
	// floor first.
	userSet := tx.userSetReadVersion
	rv := tx.readVersion
	tx.readVersionMu.Unlock()
	if tx.db != nil && userSet {
		if tx.db.minAcceptableReadVersion.Load() == 0 {
			// Bootstrap: fetch a version to establish the baseline.
			flags := tx.grvFlags()
			_, _, _ = tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags, tx.spanContext, tx.useGrvCache, tx.skipGrvCache)
		}
		if err := tx.db.validateVersion(rv); err != nil {
			return err
		}
	}
	return nil
}

// Get reads a single key. Returns nil if the key doesn't exist.
func (tx *Transaction) Get(ctx context.Context, key []byte) ([]byte, error) {
	// GRV failures are tracked: in C++ the read version is acquired INSIDE the
	// read future that reading.add records, so a failed GRV poisons commit too.
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, tx.trackReadError(err)
	}
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// Special keys (\xff\xff prefix) don't add read conflicts — C++ resolves
	// them internally without going through the resolver conflict map. The conflict
	// is routed through the RYW filter so a read served by a local independent write
	// adds no read-conflict, matching libfdb_c (RFC-121 D2).
	if !isSpecialKey(key) {
		tx.addReadConflictForKeyRYW(key)
	}
	if tx.rywDisabled {
		v, err := tx.getValue(ctx, key)
		return v, tx.trackReadError(err)
	}
	v, err := tx.ryw.get(ctx, key, tx.getValue)
	return v, tx.trackReadError(err)
}

// GetPipelined sends a GetValue request and returns a PendingGet that can be
// resolved later. This enables true pipelining: send N requests without
// waiting, then collect all N responses. Matches C++ client pipelining.
//
// Returns (nil, nil, nil) for RYW cache hits (value is returned in val).
// Returns (nil, pending, nil) for server requests (call pending.Resolve() to get value).
// Returns (nil, nil, err) for errors during send.
func (tx *Transaction) GetPipelined(ctx context.Context, key []byte) (val []byte, pending *PendingGet, err error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, nil, err
	}
	tx.hadRead.Store(true) // a read was issued; this path does NOT populate serverCache (RFC-059)
	// Legal key range check BEFORE sending — matches Transaction.Get. The illegal
	// key must be rejected at enqueue, not after the frame is on the wire. RFC-010 #3.
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// Routed through the RYW filter (RFC-121 D2) — a read served by a local independent
	// write adds no read-conflict, matching libfdb_c updateConflictMap.
	if !isSpecialKey(key) {
		tx.addReadConflictForKeyRYW(key)
	}

	// Check RYW cache.
	tx.ryw.mu.Lock()
	// Unreadable gate (RFC-098) — mirrors rywCache.get(): a sticky-unreadable
	// entry (versionstamped op anywhere in its history, even if a later plain
	// Set resolved the value) or a key inside an SVK candidate stamp range
	// throws 1036 BEFORE any cache hit or server send. Without this, the
	// pipelined path returned the folded value (sticky case) or read through
	// to storage (SVK range case) — both silent wrong answers vs libfdb_c.
	// Under BYPASS_UNREADABLE a resolved entry's value IS the bypass answer
	// (returned below); unresolved chains take ErrNeedFullRYW into ryw.get(),
	// which owns the bypass resolution.
	if !tx.ryw.bypassUnreadable {
		if entry, ok := tx.ryw.writes[string(key)]; (ok && entry.unreadable) || tx.ryw.isUnreadableLocked(key) {
			tx.ryw.mu.Unlock()
			// Tracked: in C++ this 1036 is thrown from inside the read future
			// (RYWIterator), so it lands in ryw->reading and poisons commit.
			// The transient locate/send failures below are NOT tracked — the
			// caller re-drives them through the full read path, which records
			// its own final outcome (one C++ read future = GetPipelined +
			// Resolve/re-drive together).
			return nil, nil, tx.trackReadError(&wire.FDBError{Code: ErrAccessedUnreadable})
		}
	}
	if entry, ok := tx.ryw.writes[string(key)]; ok {
		if !entry.hasAtomics {
			if entry.absent {
				// Phantom (matched CompareAndClear): an is_kv slot for getKey but ABSENT for
				// a point read — like a cleared key (RFC-058).
				tx.ryw.mu.Unlock()
				return nil, nil, nil
			}
			v := entry.value
			tx.ryw.mu.Unlock()
			return v, nil, nil
		}
		// Has atomics — need full ryw.get() to merge server value with atomics.
		tx.ryw.mu.Unlock()
		return nil, nil, ErrNeedFullRYW
	}
	isClr := tx.ryw.isClearedLocked(key)
	tx.ryw.mu.Unlock()
	if isClr {
		return nil, nil, nil
	}

	// Bound the locate, the send-loop dials, AND (below) the deferred reply wait by
	// the SetTimeout deadline (RFC-112): the pipelined path is what the fdb facade
	// Get routes through, so a hung locate/dial/reply here must honor the timeout.
	// opCtx must cover the cache-miss locate too — a hung GetKeyServerLocations is
	// the first RPC of the send phase.
	opCtx, opCancel := tx.opContext(ctx)
	defer opCancel() // dials complete within this function; the reply wait uses the timer

	// Locate shard.
	loc, locErr := tx.db.locCache.locate(tx.db, opCtx, key, tx.tenantId, tx.spanContext)
	if locErr != nil {
		return nil, nil, tx.mapTimeout(ctx, fmt.Errorf("locate key: %w", locErr))
	}
	if len(loc.Servers) == 0 {
		return nil, nil, fmt.Errorf("no storage servers for key")
	}

	// Send request without waiting for response.
	for _, server := range loc.Servers {
		conn, dialErr := tx.db.getOrDial(opCtx, server.Address)
		if dialErr != nil {
			tx.db.handleDialError(opCtx, server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		body, poolBuf := buildGetValueRequest(key, tx.readVersion, tx.lockAware || tx.readLockAware, tx.tenantId, childSpanContext(tx.spanContext), replyToken, server.Token)
		// Note: can't pool body for SendFrameDeferred — writeLoop holds reference.
		_ = poolBuf
		// RFC-114: stamp sentAt BEFORE enqueueing the frame. SendFrameDeferred only
		// enqueues onto the write channel, so for a very fast read the write+read loops
		// can deliver the reply (stamping resp.RecvAt) before we'd otherwise record
		// sentAt — making RecvAt−sentAt negative and silently dropped by the sketch.
		sentAt := time.Now()
		if sendErr := conn.SendFrameDeferred(server.Token, body); sendErr != nil {
			replyHandle.Cancel()
			replyHandle.Release()
			tx.db.handleConnError(server.Address)
			continue
		}
		timer := getTimer(tx.pipelineReplyTimeout()) // capped by SetTimeout (RFC-112)
		p := &PendingGet{key: key, tx: tx, addr: server.Address, replyCh: replyCh, replyHandle: replyHandle, conn: conn, ctx: ctx, timer: timer, sentAt: sentAt}
		// Register under the current read incarnation: Commit drains
		// outstanding pipelined reads (the C++ wait(reading) completion
		// barrier) and a post-reset late Resolve must not poison the next
		// incarnation.
		tx.readErrMu.Lock()
		p.gen = tx.readGen
		if tx.pendingReads == nil {
			tx.pendingReads = make(map[*PendingGet]struct{})
		}
		tx.pendingReads[p] = struct{}{}
		tx.readErrMu.Unlock()
		return nil, p, nil
	}
	// If every dial above failed because the SetTimeout deadline expired (opCtx
	// cancelled), surface transaction_timed_out (1031) rather than the
	// non-retryable all_alternatives_failed (1006) — a cold-dial expiry is still a
	// timeout, matching C++ (the timebomb wins the loadBalance race). RFC-112 (codex).
	if err := opCtx.Err(); err != nil {
		return nil, nil, tx.mapTimeout(ctx, err)
	}
	return nil, nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// PendingGet represents a GetValue request that has been sent but not yet resolved.
type PendingGet struct {
	key         []byte
	tx          *Transaction
	addr        string // storage server the deferred frame was sent to
	replyCh     <-chan transport.Response
	replyHandle *transport.ReplyHandle
	conn        *transport.Conn
	ctx         context.Context
	timer       *time.Timer
	sentAt      time.Time // RFC-114: send time, for the pipelined read-latency sample
	flushed     bool
	gen         uint64 // read incarnation at issue (see Transaction.readGen)

	// Resolve is idempotent: the first caller (the future's .Get(), or
	// Commit's drain — whichever runs first) does the work; later callers get
	// the memo. mu also serializes the reply-channel consumption.
	mu      sync.Mutex
	done    bool
	memoVal []byte
	memoErr error
}

// Resolve blocks until the response arrives or timeout, then applies the SAME
// classify/invalidate/retry semantics as the synchronous getValue path: a
// wrong_shard_server or all_alternatives_failed reply (including the inline
// LoadBalancedReply.error, RFC-010 #1) invalidates the stale location and
// re-drives through the full read path; transport errors, a flush failure, or a
// timeout likewise fall through to the full path rather than surfacing a bare
// error or skipping the wrong-shard retry. Pipelining only defers the wait — it
// must not own a different error policy. RFC-010 #3.
//
// Flushes the write buffer on first call to ensure the request reaches the
// server (batched with any other deferred frames on the same connection).
func (p *PendingGet) Resolve() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return p.memoVal, p.memoErr
	}
	val, err := p.resolve()
	p.done = true
	p.memoVal, p.memoErr = val, err
	// Completed — drop from the incarnation's outstanding set so Commit's
	// drain doesn't re-touch it. trackReadError already ran (gen-guarded) on
	// whichever path produced the outcome.
	p.tx.readErrMu.Lock()
	delete(p.tx.pendingReads, p)
	p.tx.readErrMu.Unlock()
	return val, err
}

func (p *PendingGet) resolve() ([]byte, error) {
	if !p.flushed {
		p.flushed = true
		if err := p.conn.Flush(); err != nil {
			// The request never reached the server — mark the connection bad
			// (parity with the sync getValue/SendFrame path) and re-locate+retry.
			p.tx.db.handleConnError(p.addr)
			p.replyHandle.Cancel()
			p.replyHandle.Release()
			putTimer(p.timer)
			return p.resolveFull()
		}
	}
	defer putTimer(p.timer)
	defer p.replyHandle.Release()
	select {
	case resp := <-p.replyCh:
		if resp.Err != nil {
			// Transport/connection error — mark the connection bad before
			// retrying so server selection avoids it, matching sendGetValue. Then
			// re-drive through the full read path.
			p.tx.db.handleConnError(p.addr)
			return p.resolveFull()
		}
		val, _, err := parseGetValueReply(resp.Body)
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			p.tx.db.locCache.invalidate(p.key, p.tx.tenantId)
			return p.resolveFull()
		}
		// RFC-114: pipelined GetValue round-trip latency — the path the fdb facade
		// Get routes through. Measured from send (sentAt) to reply DELIVERY
		// (resp.RecvAt, stamped by the read loop), NOT to Resolve-call time — so a
		// caller that batches GetPipelined and resolves the futures later records the
		// true RPC round-trip, not its own future-wait (the async-facade case). Sampled
		// on a successful reply only (mirroring the sync getValue sample); the
		// wrong-shard/transport/flush/timeout arms re-drive through getValue, which
		// samples there, so a read is counted exactly once.
		if err == nil && p.tx.db != nil && !resp.RecvAt.IsZero() {
			p.tx.db.metrics.observeReadLatency(resp.RecvAt.Sub(p.sentAt))
		}
		// Tracked (C++ ryw->reading): GetPipelined+Resolve together model ONE
		// C++ read future; this is its final outcome.
		return val, p.tx.trackReadErrorGen(err, p.gen)
	case <-p.timer.C:
		p.replyHandle.Cancel()
		return p.resolveFull()
	case <-p.ctx.Done():
		p.replyHandle.Cancel()
		return nil, p.ctx.Err()
	}
}

// resolveFull re-drives a pipelined get through the full read path and records
// its final outcome in the transaction's read-error tracking (see readErr) —
// the re-drive, not the transient failure that triggered it, is what the C++
// read future would have resolved to.
func (p *PendingGet) resolveFull() ([]byte, error) {
	v, err := p.tx.getValue(p.ctx, p.key)
	return v, p.tx.trackReadErrorGen(err, p.gen)
}

// GetKey resolves a key selector to the actual key in the database.
func (tx *Transaction) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, tx.trackReadError(err)
	}
	// C++ RYW::getKey: if (key.getKey() > getMaxReadKey()) → key_outside_legal_range
	// Eager validation — NOT tracked (C++ returns it before the read future exists).
	if bytes.Compare(selectorKey, tx.maxReadKey()) > 0 {
		return nil, &wire.FDBError{Code: 2004}
	}
	// Resolve the selector. When RYW is enabled, resolve against the MERGED view
	// (pending writes + snapshot cache) — matching C++ resolveKeySelectorFromCache
	// (RFC-056). When RYW is disabled, the whole RYW layer is bypassed → storage only.
	var resolved []byte
	var err error
	if tx.rywDisabled {
		resolved, err = tx.getKey(ctx, selectorKey, orEqual, offset)
	} else {
		resolved, err = tx.ryw.getKeyRYW(ctx, selectorKey, orEqual, offset, tx.maxReadKey(), true, tx.getRange)
	}
	if err != nil {
		return nil, tx.trackReadError(err)
	}
	// Read-conflict range: getKey conflicts over the RANGE between the selector base
	// and the resolved key (C++ addConflictRange(GetKeyReq), ReadYourWrites.actor.cpp:230),
	// NOT a single key — a concurrent write anywhere in that span must conflict.
	if !isSpecialKey(selectorKey) {
		tx.addGetKeyConflictRange(selectorKey, orEqual, offset, resolved)
	}
	// rywDisabled clamp: the RYW-enabled path resolves through getKeyRYW (hi=maxReadKey,
	// already clamped via readThroughEnd). The disabled path bypasses the RYW layer and
	// resolves against storage, which can return a system key (> maxReadKey) for a selector
	// that walks off the end of the user keyspace. C++ readThrough(GetKeyReq) clamps the
	// RETURNED key to getMaxReadKey (ReadYourWrites.actor.cpp:182-183 — "Filter out results
	// in the system keys if they are not accessible", since NativeAPI doesn't clip). The
	// conflict range above stays on the UNCLAMPED resolved key, matching NativeAPI
	// getKeyAndConflictRange (NativeAPI.actor.cpp:5767), which conflicts on the real resolved
	// key before readThrough clamps the return value.
	if tx.rywDisabled && bytes.Compare(resolved, tx.maxReadKey()) > 0 {
		resolved = tx.maxReadKey()
	}
	return resolved, nil
}

// addGetKeyConflictRange adds the read-conflict range(s) for a getKey, spanning the
// selector base ↔ resolved key (orEqual-adjusted, oriented by offset sign) — a port of
// C++ addConflictRange(GetKeyReq) (ReadYourWrites.actor.cpp:230-243), THEN filtered by
// updateConflictMap (:335). The base↔resolved span is the same range C++ computes; the
// filter (conflictRangesLocked) then SUBTRACTS segments satisfied locally with no DB read —
// INDEPENDENT writes (plain Set / folded atomic / matched-CAC phantom) and cleared ranges —
// keeping only UNMODIFIED gaps + DEPENDENT writes (which did read the DB base). With op-type
// now preserved (RFC-058), this subtraction is SAFE: a Get-folded DEPENDENT atomic keeps its
// dependent flag, so it is NOT dropped (the unsafe under-conflict codex caught on #235 came
// from a naive !hasAtomics filter on the lossy pre-fold state — not possible now). Matches
// libfdb_c exactly instead of over-conflicting on the no-DB-read segments.
func (tx *Transaction) addGetKeyConflictRange(selKey []byte, orEqual bool, offset int32, resolved []byte) {
	var begin, end []byte
	if offset <= 0 {
		begin = resolved
		if orEqual {
			end = keyAfterBytes(selKey)
		} else {
			end = selKey
		}
	} else {
		if orEqual {
			begin = keyAfterBytes(selKey)
		} else {
			begin = selKey
		}
		end = keyAfterBytes(resolved)
	}
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	// When RYW is disabled, GetKey resolved against STORAGE only (Transaction.GetKey uses
	// tx.getKey, not getKeyRYW) — the local write-map did NOT satisfy the read, so every
	// segment in the span was a real DB read. Filtering through the (bypassed) write-map
	// would subtract a local Set/Clear segment and MISS a conflict from a concurrent insert
	// into that gap (codex). Add the full span, matching C++ (RYW-disabled reads go through
	// the underlying transaction, which records the full read-conflict; updateConflictMap is
	// an RYW-layer step that does not run).
	if tx.rywDisabled {
		tx.addReadConflict(begin, end)
		return
	}
	tx.ryw.mu.Lock()
	ranges := tx.ryw.conflictRangesLocked(begin, end)
	tx.ryw.mu.Unlock()
	for _, r := range ranges {
		tx.addReadConflict(r[0], r[1])
	}
}

// addReadConflictForKeyRYW records the read-conflict for a single-key Get, routed through the
// RYW write-map filter — mirroring C++ updateConflictMap(ryw, key, it)
// (ReadYourWrites.actor.cpp:322-332): a conflict is added only when the key sits in an UNMODIFIED
// range or a DEPENDENT operation, and SKIPPED when a local INDEPENDENT write (a plain Set or a
// folded atomic) already satisfies the read — so `Set(K); Get(K)` adds no read-conflict on K, the
// way libfdb_c behaves. When RYW is disabled there is no write map (the read went straight to
// storage, recording the full read-conflict), so the full single-key conflict is added; this is
// the exact rywDisabled/else split addGetKeyConflictRange already uses for GetKey. (RFC-121 D2.)
//
// Callers add this at the PRE-read position (unlike getRangeDir, which moved the conflict after the
// read to clamp the extent). That is intentional and equivalent for a single key: the classification
// is read-invariant (the read never mutates the write map), and a single-key read that errors poisons
// the transaction (trackReadError → commit fails / reset clears conflicts), so a conflict from a
// failed read can never survive to a successful commit — matching C++'s success-only add without
// pushing the conflict into GetPipelined's Resolve/cache-hit branches.
func (tx *Transaction) addReadConflictForKeyRYW(key []byte) {
	if tx.rywDisabled {
		tx.addReadConflictForKey(key)
		return
	}
	// Single-key classification via conflictForKeyLocked (one map lookup, no write-map re-sort) —
	// NOT conflictRangesLocked, whose ensureSortedLocked re-sorts the whole write map on every call.
	// Get is the hot path (every record load); routing it through the range walk made a write-heavy
	// txn (e.g. a 10K-record bulk save, each save doing a split-marker Get) O(n²·log n).
	tx.ryw.mu.Lock()
	conflict := tx.ryw.conflictForKeyLocked(key)
	tx.ryw.mu.Unlock()
	if conflict {
		tx.addReadConflictForKey(key)
	}
}

// rangeConflictExtent clamps a completed GetRange's read-conflict to the data actually returned,
// mirroring libfdb_c's post-read addConflictRange (ReadYourWrites.actor.cpp:245-319) and the
// RYW-disabled native path (NativeAPI.actor.cpp:4558-4587). For a plain [begin,end) — both
// selectors firstGreaterOrEqual, offset +1 — the two C++ clamp sites reduce to one rule:
//   - forward, more, non-empty → [begin, keyAfter(lastReturnedKey))
//   - reverse, more, non-empty → [firstReturnedKey, end)
//   - !more or empty           → [begin, end)
//
// lastReturnedKey/firstReturnedKey are both kvs[len-1].Key: forward results are ascending (so
// kvs[last] is the highest) and reverse results descending (so kvs[last] is the lowest =
// C++ result.end()[-1].key; readpath.go:781). A more=true read genuinely did not observe the
// unread tail (forward) / head (reverse), so narrowing there cannot under-conflict; an empty or
// fully-drained (!more) read keeps the full [begin,end) so a concurrent insert ANYWHERE in the
// requested range still trips a conflict (phantom protection). more⇒non-empty for Go's row-limited
// GetRange (readpath.go:752-757,774), so the !more/empty arm is what fires on an empty read. (RFC-121 D1.)
func rangeConflictExtent(begin, end []byte, kvs []KeyValue, more, reverse bool) (cBegin, cEnd []byte) {
	if !more || len(kvs) == 0 {
		return begin, end
	}
	last := kvs[len(kvs)-1].Key
	if reverse {
		return last, end
	}
	return begin, keyAfterBytes(last)
}

// maxReadKey returns the maximum readable key for this transaction.
// Without readSystemKeys/writeSystemKeys: \xff (user keys only).
// With: \xff\xff (system keys allowed, special keys still rejected).
// Matches C++ ReadYourWritesTransaction::getMaxReadKey().
func (tx *Transaction) maxReadKey() []byte {
	if tx.readSystemKeys || tx.writeSystemKeys {
		return []byte{0xff, 0xff}
	}
	return []byte{0xff}
}

// maxWriteKey returns the maximum writable key for this transaction.
func (tx *Transaction) maxWriteKey() []byte {
	if tx.writeSystemKeys {
		return []byte{0xff, 0xff}
	}
	return []byte{0xff}
}

// metadataVersionKeyBytes is \xff/metadataVersion — exempt from system key checks.
// C++ RYW: `key != metadataVersionKey` in getValue check.
var metadataVersionKeyBytes = []byte("\xff/metadataVersion")

// metadataVersionKeyEndBytes is \xff/metadataVersion\x00 (SystemData.cpp:1386) — the end of the
// metadataVersionKey range exempted from addReadConflictRange's maxReadKey check (ReadYourWrites.actor.cpp:1955).
var metadataVersionKeyEndBytes = []byte("\xff/metadataVersion\x00")

// metadataVersionRequiredValue is the ONLY operand libfdb_c accepts for a
// SetVersionstampedValue to metadataVersionKey (SystemData.cpp:1387 — 14 zero bytes: a
// 10-byte versionstamp placeholder + the 4-byte LE offset suffix 0). C++ RYW::atomicOp
// (:2226-2229) rejects any other operand — and any non-SVV op — with client_invalid_operation.
var metadataVersionRequiredValue = []byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")

// conflictBufPool reuses backing buffers for conflict range keys (both read
// and write). Each transaction needs ~200 conflict keys for a 50-record batch,
// totaling ~20KB. Without pooling, the buffer grows 4K→8K→16K→32K
// per transaction, creating intermediate garbage at each step.
// Stores *conflictBuf to avoid interface boxing allocation (SA6002).
var conflictBufPool = sync.Pool{
	New: func() any {
		return &conflictBuf{b: make([]byte, 0, 32768)}
	},
}

type conflictBuf struct {
	b []byte
}

// SetReadSystemKeys allows reading \xff prefix system keys.
func (tx *Transaction) SetReadSystemKeys() {
	tx.readSystemKeys = true
}

// SetAccessSystemKeys allows reading AND writing \xff prefix system keys.
func (tx *Transaction) SetAccessSystemKeys() {
	tx.readSystemKeys = true
	tx.writeSystemKeys = true
}

// isSpecialKey returns true for keys with the \xff\xff prefix (FDB special key
// space). Note this is the special-key module range (\xff\xff/...), NOT the
// system keyspace (single \xff/... prefix); the name reflects what the byte
// check actually tests.
func isSpecialKey(key []byte) bool {
	return len(key) >= 2 && key[0] == 0xff && key[1] == 0xff
}

// GetRange reads a range of keys [begin, end) in forward order.
func (tx *Transaction) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return tx.getRangeDir(ctx, begin, end, limit, false)
}

// GetRangeReverse reads a range of keys [begin, end) in reverse order.
// Matches C++ where negative limit = reverse scan.
func (tx *Transaction) GetRangeReverse(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	return tx.getRangeDir(ctx, begin, end, limit, true)
}

func (tx *Transaction) getRangeDir(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, tx.trackReadError(err)
	}
	// C++ RYW::getRange: if (begin > maxKey || end > maxKey) → key_outside_legal_range
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	// C++ RYW::getRange: !limits.isValid() → range_limits_invalid (ReadYourWrites.actor.cpp:1749).
	// GetRangeLimits::isValid (FDBTypes.h:754) accepts rows >= 0 || rows == ROW_LIMIT_UNLIMITED(-1),
	// so a row limit < -1 is invalid; -1/0/positive are valid. libfdb_c (api > 13, fdb_c.cpp:983 no
	// negative→reverse remap) rejects limit <= -2 here while Go used to map all <= 0 to unlimited.
	// key_outside_legal_range is checked first (C++ order :1740 before :1749).
	if limit < -1 {
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}

	var kvs []KeyValue
	var more bool
	var err error
	if tx.rywDisabled {
		kvs, more, err = tx.getRange(ctx, begin, end, limit, reverse)
	} else {
		kvs, more, err = tx.ryw.getRange(ctx, begin, end, limit, reverse, tx.getRange)
	}
	if err != nil {
		// C++ adds the read-conflict only in the read's SUCCESS branch
		// (ReadYourWrites.actor.cpp:388) — a failed read records no conflict.
		return kvs, more, tx.trackReadError(err)
	}

	// Read-conflict computed AFTER the read so it can be CLAMPED to the data actually
	// returned (RFC-121 D1) and FILTERED through the RYW write-map (RFC-121 D2, RYW path
	// only — rywDisabled reads went straight to storage, which recorded the full read-
	// conflict span). Mirrors libfdb_c's post-read addConflictRange. The begin<=end and
	// non-special-key guards match the C++ client: an inverted range adds no conflict, and
	// \xff\xff special keys resolve internally without a resolver conflict range.
	if bytes.Compare(begin, end) <= 0 && !isSpecialKey(begin) && !isSpecialKey(end) {
		cBegin, cEnd := rangeConflictExtent(begin, end, kvs, more, reverse)
		if bytes.Compare(cBegin, cEnd) < 0 {
			if tx.rywDisabled {
				tx.addReadConflict(cBegin, cEnd)
			} else {
				tx.ryw.mu.Lock()
				ranges := tx.ryw.conflictRangesLocked(cBegin, cEnd)
				tx.ryw.mu.Unlock()
				for _, r := range ranges {
					tx.addReadConflict(r[0], r[1])
				}
			}
		}
	}
	return kvs, more, nil
}

// Set writes a key-value pair.
func (tx *Transaction) Set(key, value []byte) {
	// The mutation and its write-conflict range must become visible to a
	// concurrent Commit snapshot as ONE atomic unit — otherwise the snapshot
	// could ship the mutation without its conflict range, so a concurrent
	// transaction that read the key would not be conflicted (a missed conflict,
	// not just a spurious one). Hold conflictMu across both appends.
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutSetValue,
		Key:   key,
		Value: value,
	})
	tx.addWriteConflictForKeyLocked(key)
	tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		tx.ryw.set(key, value)
	}
}

// Clear deletes a key.
// consumeNextWriteNoConflict resets the one-shot NEXT_WRITE_NO_WRITE_CONFLICT_RANGE
// flag without adding a conflict range. C++ RYW set/clear/atomicOp consume the flag
// (getAndResetWriteConflictDisabled) at the TOP, before any size-based no-op return
// (ReadYourWrites.actor.cpp:2407 for clear), so an oversized/empty clear still
// consumes it and it never leaks to the following write.
func (tx *Transaction) consumeNextWriteNoConflict() {
	tx.conflictMu.Lock()
	tx.nextWriteNoConflict = false
	tx.conflictMu.Unlock()
}

func (tx *Transaction) Clear(key []byte) {
	// C++ clear(KeyRef) drops an oversized single-key clear entirely
	// (NativeAPI.actor.cpp:6045-6047): no mutation, no conflict range, no RYW write —
	// but the no-conflict flag is still consumed (RYW :2407, above the size check).
	// BUT C++ RYW clear() checks the legal-range FIRST (ReadYourWrites.actor.cpp:2419-2424): an
	// out-of-legal-range key (e.g. an oversized \xff system key on a non-system txn) is
	// key_outside_legal_range (2004), NOT silently dropped by the size-clamp. So only size-drop a
	// key WITHIN the legal range; an illegal key falls through and is buffered, so the commit gate's
	// legal-range check reports 2004 (mirroring Set/ClearRange).
	legal := bytes.Compare(key, tx.maxWriteKey()) < 0 || bytes.Equal(key, metadataVersionKeyBytes)
	if legal && len(key) > getMaxClearKeySize(key) {
		tx.consumeNextWriteNoConflict()
		return
	}
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   key,
		Value: end,
	})
	tx.singleKeyClearCount++ // C++ charges this mutation sizeof(KeyRangeRef), not MutationRef (RYW:2431)
	tx.addWriteConflictLocked(key, end)
	tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		tx.ryw.clear(key)
	}
}

// ClearRange deletes all keys in [begin, end).
// Returns inverted_range (2005) if begin > end. Matches C++ fdb_transaction_clear_range_impl.
// Zero-width ranges (begin == end) are silently ignored, matching C++.
func (tx *Transaction) ClearRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	// C++ clear(KeyRangeRef) clamps oversized range keys to maxSize+1 bytes rather
	// than rejecting (NativeAPI.actor.cpp:6019-6028) — there are no stored keys
	// larger than the max, so a too-large bound is equivalent to its truncation.
	if bmax := getMaxClearKeySize(begin); len(begin) > bmax {
		begin = begin[:bmax+1]
	}
	if emax := getMaxClearKeySize(end); len(end) > emax {
		end = end[:emax+1]
	}
	// Zero-width (begin == end) or clamped-to-empty (begin >= end): C++ returns
	// without recording a mutation (r.empty()), but still consumes the no-conflict
	// flag (consumed at the top of RYW clear, above the empty check).
	if bytes.Compare(begin, end) >= 0 {
		tx.consumeNextWriteNoConflict()
		return nil
	}
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   begin,
		Value: end,
	})
	tx.addWriteConflictLocked(begin, end)
	tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		tx.ryw.clearRange(begin, end)
	}
	return nil
}

// Atomic performs an atomic mutation.
func (tx *Transaction) Atomic(op MutationType, key, operand []byte) {
	// Min→MinV2 / And→AndV2 op-code upgrade (C++ RYW::atomicOp,
	// ReadYourWrites.actor.cpp:2243-2248): apiVersionAtLeast(510) upgrades the
	// legacy codes to their V2 variants, which fold correctly on an absent key.
	// Done here (the RYW::atomicOp analog) not in the fdb facade, so every caller
	// — including cmd/fdb-stacktester — gets it, matching libfdb_c.
	if tx.db != nil && tx.db.apiVersionAtLeast(510) {
		switch op {
		case MutMin:
			op = MutMinV2
		case MutAnd:
			op = MutAndV2
		}
	}
	// C++ ReadYourWritesTransaction::atomicOp (ReadYourWrites.actor.cpp:2234) rejects any op that is
	// not isValidMutationType && isAtomicOp with invalid_mutation_type, BEFORE adding it to the write
	// map; libfdb_c's CATCH_AND_DIE (fdb_c.cpp:1149) then aborts the client. Go's Atomic() is void, so
	// we record a deferred error surfaced at Commit and — critically — do NOT buffer the mutation, so
	// a misused op-code (e.g. Atomic(MutClearRange,...), indistinguishable from a real Clear at commit
	// time) can never reach the shared cluster. The eager record matches "first invalid op wins" call
	// ordering. Reads are left intact (the non-aborting analog: the failed atomicOp didn't add a
	// mutation, but the transaction object is otherwise usable until the commit gate rejects it).
	if !isAtomicOp(op) {
		// C++ atomicOp checks metadataVersionKey (2000) / legal-range (2004) BEFORE the op-validity
		// check (2018) — ReadYourWrites.actor.cpp:2226-2234. Surface the same precedence eagerly so
		// Atomic(invalidOp, systemKey) reports key_outside_legal_range and Atomic(invalidOp,
		// metadataVersionKey) reports client_invalid_operation — not 2018.
		var code int
		switch {
		case bytes.Equal(key, metadataVersionKeyBytes):
			code = 2000 // client_invalid_operation
		case bytes.Compare(key, tx.maxWriteKey()) >= 0:
			code = 2004 // key_outside_legal_range
		default:
			code = ErrInvalidMutationType // 2018
		}
		// C++ throws the FIRST illegal op EAGERLY (ReadYourWrites.actor.cpp:2226-2234). A mutation
		// buffered BEFORE this bad Atomic that is itself illegal came first and out-ranks the bad-op
		// code (codex); only if every preceding mutation is legal is THIS bad op the first illegal
		// one. Compute under conflictMu for a consistent preceding-mutation snapshot; store-if-unset
		// keeps the first bad Atomic's verdict (race-free vs Commit's lock-free Load).
		tx.conflictMu.Lock()
		if tx.invalidAtomicOpErr.Load() == nil {
			poison := &wire.FDBError{Code: code}
			maxWrite := tx.maxWriteKey()
			for _, m := range tx.mutations {
				if e := tx.validateMutation(m, maxWrite); e != nil {
					var fe *wire.FDBError
					if errors.As(e, &fe) {
						poison = fe
					}
					break
				}
			}
			tx.invalidAtomicOpErr.Store(poison)
		}
		tx.conflictMu.Unlock()
		return
	}
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  op,
		Key:   key,
		Value: operand,
	})
	// Atomic ops add a write conflict range but NOT a read conflict range —
	// EXCEPT SetVersionstampedKey. Its key carries an incomplete versionstamp
	// (the 10-byte stamp is filled in server-side at commit), so a conflict range
	// over the placeholder bytes is meaningless and would spuriously conflict two
	// transactions that stamp "the same" logical key. C++ RYW atomicOp forces
	// AddConflictRange::False for SetVersionstampedKey (ReadYourWrites.actor.cpp:2268)
	// while STILL consuming the NEXT_WRITE_NO_WRITE_CONFLICT_RANGE flag (getAndReset
	// at :2220). Match both: consume the flag, skip the range.
	if op == MutSetVersionstampedKey {
		tx.nextWriteNoConflict = false
	} else {
		tx.addWriteConflictForKeyLocked(key)
	}
	tx.conflictMu.Unlock()
	if !tx.rywDisabled {
		if op == MutSetVersionstampedKey {
			// SVK is RANGE-unreadable in the RYW model (RFC-098; C++
			// ReadYourWrites.actor.cpp:2263-2277): the entire candidate stamp
			// range [key@minStamp, key@\xff…) becomes unreadable, and the
			// pending entry is stored at the key TRANSFORMED with the
			// min-bound stamp (Atomic.h:289) — keeping the 4-byte offset
			// suffix, exactly as C++ mutates k in place and inserts it
			// suffix-and-all. The COMMIT mutation (tx.mutations above) keeps
			// the user's original placeholder key; only the local read model
			// uses the transform. A malformed key (no room for offset/stamp)
			// falls through as a plain entry at the user key and the COMMIT
			// path's validation reports client_invalid_operation — the same
			// error C++ raises, but at a different time: C++ throws EAGERLY
			// from getVersionstampKeyRange inside atomicOp() (before touching
			// the write map), while Go's void Atomic() defers all mutation
			// validation to Commit (pre-existing design, see the commit-path
			// checks).
			minVersion := int64(0)
			tx.readVersionMu.Lock()
			if tx.hasReadVersion {
				minVersion = tx.readVersion // C++ tr.getCachedReadVersion().orDefault(0)
			}
			tx.readVersionMu.Unlock()
			if rangeBegin, rangeEnd, transformed, ok := versionstampKeyRange(key, minVersion, tx.maxReadKey()); ok {
				tx.ryw.addUnreadableRange(rangeBegin, rangeEnd)
				tx.ryw.atomic(op, transformed, operand)
				return
			}
		}
		tx.ryw.atomic(op, key, operand)
	}
}

// versionstampKeyRange ports C++ getVersionstampKeyRange + the in-place key
// transform (Atomic.h:258-300): key carries a trailing 4-byte LE offset of
// the 10-byte placeholder. Returns the candidate stamp range
// [key@stamp(minVersion,0), key@\xff×10 + \x00) clamped to maxKey, and the
// key transformed with the min-bound stamp (suffix preserved). ok=false on a
// malformed key (validated again by the commit path's eager checks).
func versionstampKeyRange(key []byte, minVersion int64, maxKey []byte) (begin, end, transformed []byte, ok bool) {
	if len(key) < 4 {
		return nil, nil, nil, false
	}
	pos := int(int32(binary.LittleEndian.Uint32(key[len(key)-4:])))
	// pos > len-14 (not pos+10 > len-4): the subtraction form can't overflow
	// for any int32 pos on a 32-bit int.
	if pos < 0 || pos > len(key)-4-10 {
		return nil, nil, nil, false
	}
	// begin = key[:len-4] with placeVersionstamp(minVersion, 0) at pos.
	begin = append([]byte(nil), key[:len(key)-4]...)
	placeVersionstamp(begin[pos:], minVersion, 0)
	// end = key[:len-3] with trailing byte 0x00 and \xff×10 at pos
	// (Atomic.h:277-284: substr(0, size-3) then last byte = 0).
	end = append([]byte(nil), key[:len(key)-3]...)
	end[len(end)-1] = 0x00
	for i := 0; i < 10; i++ {
		end[pos+i] = 0xff
	}
	if bytes.Compare(end, maxKey) > 0 {
		end = append([]byte(nil), maxKey...)
	}
	// transformed = full key (suffix INCLUDED) with the min-bound stamp.
	transformed = append([]byte(nil), key...)
	placeVersionstamp(transformed[pos:], minVersion, 0)
	return begin, end, transformed, true
}

// placeVersionstamp writes the 10-byte versionstamp: 8-byte BIG-endian
// version + 2-byte BIG-endian transaction number (Atomic.h:243-256).
func placeVersionstamp(dst []byte, version int64, txnNumber uint16) {
	binary.BigEndian.PutUint64(dst[:8], uint64(version))
	binary.BigEndian.PutUint16(dst[8:10], txnNumber)
}

// validateMutation returns the eager-validation error a buffered mutation would throw, or nil if it
// is legal. C++ set()/atomicOp()/clear() throw these EAGERLY at the call (ReadYourWrites.actor.cpp);
// our Set/Clear/Atomic are void, so we defer to commit — but the per-mutation checks and their order
// match libfdb_c, and the commit loop walks mutations in call order so the FIRST illegal op wins.
// Pure (no tx.state mutation): callers mark the txn errored. maxWrite is tx.maxWriteKey(), hoisted.
// Shared by the commit-time loop AND Atomic()'s invalid-op poison (which must defer to an earlier
// illegal buffered mutation — codex).
func (tx *Transaction) validateMutation(m Mutation, maxWrite []byte) error {
	if bytes.Equal(m.Key, metadataVersionKeyBytes) && m.Type != MutClearRange {
		// C++ RYW::atomicOp (:2226-2229) and set() (:2300): the ONLY legal mutation to
		// metadataVersionKey is SetVersionstampedValue with operand == metadataVersionRequiredValue.
		// A plain Set, any other atomic op (Add, SVK, …), or SVV with a different operand →
		// client_invalid_operation (2000), BEFORE the size/offset checks. The legal write is exempt
		// from the legal-range check (system key) but still subject to the size/versionstamp checks
		// below, which metadataVersionRequiredValue passes. clear()/clear(range) (C++ :2357, :2406)
		// have NO metadataVersionKey gate — a clear of metadataVersionKey falls to the legal-range
		// check (metadataVersionKey >= maxWriteKey → 2004). Hence the `!= MutClearRange` guard.
		if m.Type != MutSetVersionstampedValue || !bytes.Equal(m.Value, metadataVersionRequiredValue) {
			return &wire.FDBError{Code: 2000} // client_invalid_operation
		}
	} else if bytes.Compare(m.Key, maxWrite) >= 0 {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	if m.Type == MutClearRange {
		// ClearRange: also check end key (stored in Value). C++ clear(range): if (range.begin >
		// maxKey || range.end > maxKey) → reject. Clear key SIZES are clamped at build time
		// (Clear/ClearRange), matching C++ clear()'s translate-not-reject — so no key_too_large here.
		if bytes.Compare(m.Value, maxWrite) > 0 {
			return &wire.FDBError{Code: 2004}
		}
		return nil
	}
	// set()/atomicOp() reject oversized keys/values (the C binding aborts the process on the throw;
	// we reject the commit so the oversized data never reaches the shared cluster — see sizelimits.go).
	// hasRawAccess: C++ sets options.rawAccess for RAW_ACCESS/ACCESS_SYSTEM_KEYS/READ_SYSTEM_KEYS
	// (NativeAPI.actor.cpp:7159-7170); ACCESS==writeSystemKeys, READ==readSystemKeys, either raising
	// the non-system key limit by the tenant-prefix slack (KEY_SIZE_LIMIT+8). But that +8 slack is the
	// tenant-prefix allowance (getMaxWriteKeySize, NativeAPI:11630): when THIS client prepends the
	// prefix itself (tenantId >= 0), the user key must stay within KEY_SIZE_LIMIT, so the slack is
	// gated on the no-tenant case (C++ forbids raw-access options on tenant transactions for this).
	rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
	if len(m.Key) > getMaxWriteKeySize(m.Key, rawAccess) {
		return &wire.FDBError{Code: 2102} // key_too_large
	}
	if len(m.Value) > valueSizeLimit {
		return &wire.FDBError{Code: 2103} // value_too_large
	}
	// Versionstamp offset validation → client_invalid_operation (2000). C++ atomicOp validates this
	// EAGERLY; deferred here but kept AFTER the size checks (an oversized versionstamp key/value
	// reports 2102/2103, not 2000) and walked in call order so the FIRST eagerly-invalid op wins
	// (TestDifferential_VersionstampValidationOrder). Precedes the deferred transaction-size 2101.
	switch m.Type {
	case MutSetVersionstampedKey:
		if err := validateVersionstampOffset(m.Key); err != nil {
			return err
		}
	case MutSetVersionstampedValue:
		if err := validateVersionstampOffset(m.Value); err != nil {
			return err
		}
	}
	return nil
}

// Commit sends mutations to a commit proxy.
// After successful commit, the transaction is automatically reset for reuse
// (mutations and conflict ranges cleared, read version invalidated).
// This matches the C client's behavior where fdb_transaction_set() can be
// called after commit to start building a new transaction.
func (tx *Transaction) Commit(ctx context.Context) error {
	if err := tx.checkCancelled(); err != nil {
		return err // transaction_cancelled (1025), matching libfdb_c (RFC-068)
	}
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	// A poisoned transaction (SetReadYourWritesDisable after an op) fails commit with
	// client_invalid_operation. Checked HERE — before the read-only fast path below (a
	// read-only poisoned commit has no mutations, so it would otherwise skip
	// ensureReadVersion's gate and commit successfully) AND before checkTimeout: reads check
	// the poison before the timeout, and libfdb_c's checkDeferredError runs before any commit
	// logic, so the poison must out-rank a stale-timeout 1031 for parity. Returns without
	// resetting (2000 is non-retryable). codex, RFC-059.
	if tx.rywPoisonErr != nil {
		return tx.rywPoisonErr
	}
	// A non-atomic op-code passed to Atomic() poisons the commit with invalid_mutation_type (2018),
	// matching C++ atomicOp's eager throw (ReadYourWrites.actor.cpp:2234). The bad mutation was never
	// buffered, so nothing reaches the cluster. Non-retryable; returns without resetting.
	if e := tx.invalidAtomicOpErr.Load(); e != nil {
		return e
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}
	// C++ commit() waits on ryw->reading before ANY commit work — before the
	// RYW-disabled branch, the read-only fast path, and the size checks
	// (ReadYourWrites.actor.cpp:1358-1359). That wait is a COMPLETION BARRIER
	// for in-flight reads, not just a sample of past failures: drain any
	// outstanding pipelined reads first (Resolve is idempotent — a later
	// future .Get() returns the memo), so a read whose reply is still in
	// flight fails this commit exactly as it would fail libfdb_c's. Then a
	// read that failed earlier fails the commit with that same error, even if
	// the caller swallowed it (the errored future stays in the AndFuture until
	// reset). Checked after checkTimeout: a fired timebomb sits in
	// resetPromise, which the C++ commit wrapper surfaces before the actor's
	// wait(reading). Returns without resetting — the caller drives
	// OnError/Reset, which clears it (resetRyow :2715).
	tx.readErrMu.Lock()
	drain := make([]*PendingGet, 0, len(tx.pendingReads))
	for p := range tx.pendingReads {
		drain = append(drain, p)
	}
	tx.readErrMu.Unlock()
	for _, p := range drain {
		p.Resolve() //nolint:errcheck // outcome lands in readErr via its tracked tail
	}
	tx.readErrMu.Lock()
	readErr := tx.readErr
	tx.readErrMu.Unlock()
	if readErr != nil {
		return readErr
	}

	// Validate size limit bounds. C++: valid range is [32, 10_000_000].
	// Out-of-range values return invalid_option_value (2006) at commit time.
	if tx.sizeLimit > 0 && (tx.sizeLimit < 32 || tx.sizeLimit > 10_000_000) {
		tx.state.Store(int32(txStateErrored))
		return &wire.FDBError{Code: 2006} // invalid_option_value
	}

	// Snapshot the mutation/conflict buffers under conflictMu before reading
	// them. The published contract (fdb/transaction.go) makes Set/Get/Commit
	// safe for concurrent use: a Get future resolving on another goroutine
	// appends conflicts under this lock. The slices are append-only (elements
	// are never mutated in place), so the header snapshot stays valid after
	// release — we only iterate [0:len) and a concurrent append writes beyond it.
	tx.conflictMu.Lock()
	muts := tx.mutations
	nWriteConflicts := len(tx.writeConflicts)
	// Re-read the invalid-atomic poison UNDER the snapshot lock, linearized with `muts` (codex): the
	// entry check (above) can miss an Atomic(badOp) that races this Commit and stores the poison —
	// under conflictMu — AFTER that entry Load but BEFORE this snapshot. Reading it here, in the same
	// critical section as the mutation snapshot, makes the poison-vs-commit order consistent with the
	// mutation-vs-commit order: a bad Atomic ordered before this snapshot poisons the commit; one
	// ordered after is not in `muts` either, so the commit linearizes before it.
	poison := tx.invalidAtomicOpErr.Load()
	tx.conflictMu.Unlock()
	if poison != nil {
		tx.state.Store(int32(txStateErrored))
		return poison
	}

	// C++ RYW write checks (deferred to commit since our Set/Clear are void):
	// - set(): if key == metadataVersionKey → client_invalid_operation
	//          if key >= maxWriteKey → key_outside_legal_range
	// - atomicOp(): if key == metadataVersionKey → ONLY SetVersionstampedValue with operand ==
	//               metadataVersionRequiredValue is allowed; anything else →
	//               client_invalid_operation. metadataVersionKey skips the legal-range check.
	//               if key >= maxWriteKey → key_outside_legal_range
	maxWrite := tx.maxWriteKey()
	for _, m := range muts {
		if err := tx.validateMutation(m, maxWrite); err != nil {
			tx.state.Store(int32(txStateErrored))
			return err
		}
	}

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
	// empty fast path (:6800-6806, not counted — the non-empty guard here is
	// that fast path's condition) but BEFORE the size check (~:6835), so a
	// persistently oversized commit shows up as Started-without-Completed.
	// Started-Completed = failed/in-flight (intentional asymmetry). RFC-097.
	if tx.db != nil && (len(muts) > 0 || nWriteConflicts > 0) {
		tx.db.metrics.transactionsCommitStarted.Add(1)
	}

	if (len(muts) > 0 || nWriteConflicts > 0) && tx.sizeLimit > 0 && tx.approximateCommitSize(muts) > tx.sizeLimit {
		tx.state.Store(int32(txStateErrored))
		return &wire.FDBError{Code: 2101} // transaction_too_large
	}

	if len(muts) == 0 && nWriteConflicts == 0 {
		// Read-only transaction — no commit needed.
		// Still set hasCommitted so GetCommittedVersion returns 0 (not error 2015).
		// Reset for reuse (matches C client behavior).
		tx.hasCommitted = true
		tx.postCommitReset()
		return nil
	}

	// C++ tryCommit calls startTransaction(CAUSAL_READ_RISKY) to ensure a
	// read version exists before commit, even for write-only transactions.
	// Without this, ReadSnapshot=0 is sent which crashes the FDB server.
	if err := tx.ensureReadVersion(ctx); err != nil {
		return err
	}

	// Pass the SAME validated snapshot to the marshal so the shipped mutation
	// set is byte-identical to the set we just validated. Without this the
	// marshal re-reads tx.mutations independently, and a Set landing on another
	// goroutine between validation and marshal (allowed by the concurrent-use
	// contract) would ship an UNVALIDATED mutation to the commit proxy.
	//
	// RFC-090/093: detach ONLY the commit RPC + its commit_unknown_result
	// idempotency barrier (commitDummyTransaction) from the caller ctx. The GRV
	// above (ensureReadVersion, :1106) is deliberately left on the LIVE ctx
	// (RFC-093) so a cancel during the commit-path read version aborts promptly —
	// a GRV is a cancellable read, matching C++ NativeAPI.actor.cpp (the GRV future
	// is cancelled uniformly with the commit). Here we WithoutCancel so a late
	// caller-ctx cancel can neither yank an in-flight commit (already bounded by the
	// per-RPC timeout) nor make the barrier no-op on a cancelled ctx
	// (commitpath.go's `if ctx.Err()!=nil {return}`). For callers passing
	// context.Background() (nothing to strip), WithoutCancel is observably inert.
	commitStart := time.Now()
	if err := tx.commit(context.WithoutCancel(ctx), muts); err != nil {
		return err
	}

	// C++ ++transactionsCommitCompleted on tryCommit success (:6673). RFC-097.
	// RFC-114: on the same success, sample the commit round-trip (C++
	// commitLatencies, NativeAPI.actor.cpp:6681) and the total transaction latency
	// now-metricStart (C++ latencies, :6682). Read-only txns return at the fast
	// path above and never reach here, so they contribute to neither — matching C++.
	// Divergence (documented in RFC-114): metricStart is NOT reset on OnError retry,
	// so total latency spans all retries (whole-transaction wall-clock), whereas C++
	// resets trState->startTime per attempt and measures only the current attempt's
	// GRV→commit. Identical for a no-retry commit (the common case). metricStart is
	// re-anchored on commit-reuse (postCommitReset), so a reused handle measures only
	// the transaction that just committed, not the prior one.
	if tx.db != nil {
		tx.db.metrics.transactionsCommitCompleted.Add(1)
		now := time.Now()
		tx.db.metrics.observeCommitLatency(now.Sub(commitStart))
		if !tx.metricStart.IsZero() {
			tx.db.metrics.observeTotalLatency(now.Sub(tx.metricStart))
		}
	}

	// Feed committed version to GRV cache so a later opted-in read sees this
	// write. Advances version + freshness, matching C++ updateCachedReadVersion
	// at the commit site (NativeAPI.actor.cpp:6657, t=now()). Unconditional —
	// population runs for every transaction regardless of USE_GRV_CACHE; only
	// cache READS are opt-in (RFC-104).
	if tx.committedVersion > 0 {
		tx.db.grvCache.update(tx.committedVersion)
	}

	tx.hasCommitted = true

	// Auto-reset for reuse — clear mutations and conflicts but preserve
	// committedVersion/txnBatchId for GetCommittedVersion/GetVersionstamp.
	//
	// Divergence from C++ (audit item #6): the C++ NativeAPI client leaves
	// the transaction in a fully-committed state and requires the caller to
	// either Reset() or destroy it before the next use. We auto-reset here
	// to match the C-binding contract observed by the binding tester (which
	// reuses the same fdb_transaction_t handle across `_RESET` instructions
	// without explicit reset between commits) and the Go-idiomatic
	// `db.Run(func(tx)…)` callers that expect a single tx object to be
	// reusable. The accepted cost is one extra `slice = slice[:0]` and a
	// pool-return per commit, which is amortised by the savings on the
	// retry path. See TODO.md "Document accepted divergences inline" for
	// the open question of whether to also expose a no-reset variant.
	tx.postCommitReset()
	return nil
}

// Cancel cancels the transaction. All subsequent operations will return an error.
// This is irreversible — a cancelled transaction cannot be reused.
func (tx *Transaction) Cancel() {
	tx.cancelWatches()
	tx.endTxSpan() // RFC-115 §4 Layer 2: end the "Transaction" otel span on teardown
	tx.state.Store(int32(txStateCancelled))
}

// Reset resets the transaction to a clean state, as if newly created from Database.
// Unlike the internal reset() used by OnError (which preserves retryCount/backoff),
// this clears everything including retry state. Options set via Set*() are preserved
// across Reset, matching C++ ReadYourWritesTransaction::reset() + applyPersistentOptions.
// Updates creationTime so the timeout budget restarts (matches C++ reset() behavior).
//
// In-flight Watch() calls ARE cancelled by Reset() (via reset()→cancelWatches(), below), but they
// surface as context.Canceled rather than the FDBError transaction_cancelled (1025) C++ raises via
// resetPromise.sendError — a known divergence (TODO "watch-path divergences" D5).
func (tx *Transaction) Reset() {
	tx.retryCount = 0
	tx.backoff = 0
	// C++ reset() updates creationTime = now(), restarting timeout window.
	tx.creationTime = time.Now()
	// RFC-114: Reset() begins a NEW logical transaction, so clear the total-latency
	// anchor here too (re-stamped at the next first GRV) — otherwise a handle that
	// reads/abandons work then Reset()s without committing would fold that pre-Reset
	// work + idle into the next commit's total latency. This clear lives in Reset(),
	// NOT in the OnError-shared reset() (which must preserve metricStart so latency
	// spans retries).
	tx.metricStart = time.Time{}
	tx.reset()
}

// regenerateSpan refreshes the transaction's trace span — a fresh span per
// transaction/attempt, matching C++ generateSpanID at cloneAndReset
// (NativeAPI.actor.cpp:3458). With an injected SPAN_PARENT the parent's traceID +
// flags are inherited and only the spanID is fresh (a child span on the caller's
// trace); otherwise a brand-new random span, sampled per the DB's sample rate
// (default unsampled). RFC-115 §4.
func (tx *Transaction) regenerateSpan() {
	if tx.spanParent != nil {
		tx.spanContext = childSpanContext(*tx.spanParent)
		return
	}
	// tx.db is nil for directly-constructed Transactions in some unit tests (which call
	// reset()/postCommitReset() without a Database) — default to the unsampled rate.
	var rate float64
	if tx.db != nil {
		rate = tx.db.tracingSampleRate
	}
	tx.spanContext = newSpanContext(rate)
}

// ensureTxSpan lazily starts the "Transaction" otel span for a SAMPLED transaction
// (C++ NativeAPI.actor.cpp:6186), seeded with the tx's wire traceID (otelSpanContext)
// so the otel tree and FDB server-side spans share one trace. No-op when unsampled (so
// unsampled txns allocate nothing) or when the Transaction span already exists. Called
// at the first GRV under readVersionMu — the single-entry tx-start point.
func (tx *Transaction) ensureTxSpan() {
	if !isSampled(tx.spanContext) {
		return
	}
	tx.traceMu.Lock()
	already := tx.txSpan != nil
	tx.traceMu.Unlock()
	if already {
		return
	}
	// Start OUTSIDE traceMu (mirroring startOpSpan) so a re-entrant tracer can't deadlock;
	// safe against a double-start because ensureTxSpan runs only at the first GRV under
	// readVersionMu (single-entry), so no concurrent ensureTxSpan can race it.
	parent := oteltrace.ContextWithSpanContext(context.Background(), otelSpanContext(tx.spanContext))
	ctx, span := tx.db.tracer.Start(parent, "Transaction", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	tx.traceMu.Lock()
	tx.traceCtx, tx.txSpan = ctx, span
	tx.traceMu.Unlock()
}

// startOpSpan starts a per-operation child span under the "Transaction" span (the C++
// NAPI:* children, e.g. NativeAPI.actor.cpp:3623). Returns nil when the tx is unsampled
// or the Transaction span hasn't started yet — callers use:
//
//	if sp := tx.startOpSpan("fdbgo.getValue"); sp != nil { defer sp.End() }
//
// The per-op otel span carries an otel-minted spanID under the shared traceID; the wire
// SpanContext is generated independently by Layer 1 (single ID authority).
func (tx *Transaction) startOpSpan(name string) oteltrace.Span {
	if !isSampled(tx.spanContext) {
		return nil
	}
	tx.traceMu.Lock()
	parent := tx.traceCtx
	tx.traceMu.Unlock()
	if parent == nil {
		return nil
	}
	_, span := tx.db.tracer.Start(parent, name, oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	return span
}

// endTxSpan ends the "Transaction" otel span (C++ ~Span on tx end). Idempotent; called
// on commit success (postCommitReset), on OnError retry / user Reset (reset), and on
// Cancel. NOTE: there is no Transaction.Close(); a raw CreateTransaction handle that is
// first-GRV'd and then abandoned WITHOUT commit/Reset/Cancel never ends its span (a leak
// — only with a real WithTracer + a sampled tx). The common Transact/TransactCtx path is
// always safe (it commits or resets). See WithTracer / CreateTransaction for the caveat.
func (tx *Transaction) endTxSpan() {
	tx.traceMu.Lock()
	defer tx.traceMu.Unlock()
	if tx.txSpan != nil {
		tx.txSpan.End()
		tx.txSpan = nil
		tx.traceCtx = nil
	}
}

// SetSpanParent injects a parent trace context (FDBTransactionOptions::SPAN_PARENT,
// NativeAPI.actor.cpp:7126): a 33-byte IncludeVersion-serialized SpanContext. The
// transaction's span becomes a child of it (inherit traceID + flags, fresh spanID),
// and the linkage persists across retries (regenerateSpan honors spanParent).
func (tx *Transaction) SetSpanParent(b []byte) error {
	parent, err := parseSpanParent(b)
	if err != nil {
		return err
	}
	tx.spanParent = &parent
	tx.spanContext = childSpanContext(parent)
	return nil
}

// cancelWatches cancels any in-flight Watch() calls by cancelling the
// watch context. Matches C++ resetRyow() which sends transaction_cancelled
// through resetPromise to cancel pending watches.
func (tx *Transaction) cancelWatches() {
	tx.watchMu.Lock()
	defer tx.watchMu.Unlock()
	if tx.watchCancel != nil {
		tx.watchCancel()
		tx.watchCtx = nil
		tx.watchCancel = nil
	}
}

// getWatchCtx returns a context for Watch() calls that is cancelled on
// reset/Reset. Created lazily — if no Watch is ever called, no context
// is allocated.
func (tx *Transaction) getWatchCtx(parent context.Context) context.Context {
	tx.watchMu.Lock()
	defer tx.watchMu.Unlock()
	if tx.watchCtx == nil {
		tx.watchCtx, tx.watchCancel = context.WithCancel(parent)
	}
	return tx.watchCtx
}

// GetCommittedVersion returns the version at which this transaction committed.
func (tx *Transaction) GetCommittedVersion() (int64, error) {
	if !tx.hasCommitted {
		return 0, &wire.FDBError{Code: 2015} // used_during_commit / not yet committed
	}
	return tx.committedVersion, nil
}

// GetVersionstamp returns the 10-byte versionstamp from the committed transaction.
// Format: [version 8 bytes big-endian][txnBatchId 2 bytes big-endian].
// Must be called after a successful Commit.
func (tx *Transaction) GetVersionstamp() ([]byte, error) {
	if err := tx.checkCancelled(); err != nil {
		return nil, err // transaction_cancelled (1025) out-ranks the not-yet-committed 2015 (RFC-068)
	}
	if !tx.hasCommitted {
		return nil, &wire.FDBError{Code: 2015}
	}
	vs := make([]byte, 10)
	binary.BigEndian.PutUint64(vs[0:8], uint64(tx.committedVersion))
	binary.BigEndian.PutUint16(vs[8:10], tx.txnBatchId)
	return vs, nil
}

// backoffSleep waits for `d` or until ctx fires, whichever comes first.
// Returns ctx.Err() on cancellation so callers can propagate it; nil on
// natural elapse. Used by OnError and commitDummyTransaction so a cancelled
// context doesn't keep clients pinned through a 1–30s backoff window.
func backoffSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffSleepBounded sleeps for `delay`, but never past the SetTimeout deadline: a backoff that
// would cross creationTime+timeout is cut short and surfaces transaction_timed_out (1031), matching
// C++ RYWImpl::onError which races the backoff delay() against the timebomb (resetPromise,
// ReadYourWrites.actor.cpp:1517) so the wait aborts the moment the deadline passes. With no timeout
// set it is exactly backoffSleep. A genuine parent-ctx cancellation still surfaces ctx.Err().
func (tx *Transaction) backoffSleepBounded(ctx context.Context, delay time.Duration) error {
	if tx.timeout <= 0 {
		return backoffSleep(ctx, delay)
	}
	bctx, cancel := context.WithDeadline(ctx, tx.deadline)
	defer cancel()
	if err := backoffSleep(bctx, delay); err != nil {
		// The deadline fired (not the caller's ctx) → 1031, matching the timebomb race.
		if ctx.Err() == nil && time.Now().After(tx.deadline) {
			return &wire.FDBError{Code: ErrTransactionTimedOut}
		}
		return err
	}
	return nil
}

// OnError handles a transaction error. Returns nil if the error is retryable
// (the transaction has been reset for retry). Returns the error if non-retryable
// or ctx.Err() if ctx fires during the backoff sleep.
func (tx *Transaction) OnError(ctx context.Context, err error) error {
	// A cancelled txn can never be retried — C++ OnError races resetPromise and returns
	// transaction_cancelled (1025) (ReadYourWrites.actor.cpp). Without this, OnError on a
	// cancelled txn would reset-and-retry a retryable input error (return nil), reusing a
	// cancelled handle — a real divergence (RFC-068).
	if cerr := tx.checkCancelled(); cerr != nil {
		return cerr // transaction_cancelled (1025)
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		// A non-FDB application error (e.g. a Database.Transact callback returning errors.New(...))
		// is the caller's own, NOT an FDB retry concern — it escapes unchanged, even past the
		// deadline. The timeout gate below applies ONLY to FDB errors (the retry path), so it must
		// sit AFTER this branch (codex: otherwise an expired deadline would replace the application
		// error with 1031). Pre-bug-hunt behavior, restored.
		tx.state.Store(int32(txStateErrored))
		return err
	}
	// The SetTimeout deadline out-ranks the retry for an FDB error: C++ RYWImpl::onError throws
	// transaction_timed_out at entry if the timebomb already fired (ReadYourWrites.actor.cpp:1506) —
	// before classifying the FDB error and before any backoff. Without this, a SetTimeout txn under
	// contention sleeps a full (growing) backoff and does one extra reset+retry before the NEXT op's
	// checkTimeout surfaces 1031, overshooting the declared timeout. Non-retryable; mark errored.
	if cerr := tx.checkTimeout(); cerr != nil {
		tx.state.Store(int32(txStateErrored))
		return cerr // transaction_timed_out (1031)
	}

	// Transaction timeout is NEVER retryable — matches C++ behavior where
	// OnError(1031) returns 1031 and the error escapes the retry loop.
	if fdbErr.Code == ErrTransactionTimedOut {
		tx.state.Store(int32(txStateErrored))
		return err
	}

	// Check retry limit before allowing any retry.
	if tx.hasRetryLimit && tx.retryCount >= tx.retryLimit {
		tx.state.Store(int32(txStateErrored))
		return err
	}

	// Single source of WHETHER to retry (RFC-105 — derive, don't mirror):
	// onErrorRetryable is the one onError-retryable predicate, also used by
	// commitDummyTransaction, so the two can never drift. A code it rejects errors
	// out here; the switch below only refines the BACKOFF CLASS for retryable
	// codes (its default arm covers the RETRYABLE_NOT_COMMITTED majority), so a
	// retryable code can never be silently dropped by a missing case.
	if !onErrorRetryable(fdbErr.Code) {
		tx.state.Store(int32(txStateErrored))
		return err
	}

	switch fdbErr.Code {
	case ErrTransactionTooOld, ErrFutureVersion:
		// Version-related: fixed delay, no backoff growth.
		// C++ NativeAPI.actor.cpp: min(FUTURE_VERSION_RETRY_DELAY, maxBackoff).
		delay := futureVersionDelay
		if tx.maxRetryDelay > 0 && tx.maxRetryDelay < delay {
			delay = tx.maxRetryDelay
		}
		tx.retryCount++
		if tx.db != nil {
			tx.db.countRetryAndLog(ctx, fdbErr.Code, tx.retryCount)
		}
		if cerr := tx.backoffSleepBounded(ctx, delay); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		return nil

	case ErrProxyMemoryLimitExceeded, ErrGrvProxyMemoryLimit,
		ErrThrottledHotShard, ErrRangeLocked:
		// Resource-constrained: higher backoff cap (30s vs 1s).
		// C++ RESOURCE_CONSTRAINED_MAX_BACKOFF for all four codes.
		// hot_shard and range_locked use the same 30s cap to avoid
		// hammering the hot shard with aggressive retries.
		tx.retryCount++
		if tx.db != nil {
			tx.db.countRetryAndLog(ctx, fdbErr.Code, tx.retryCount)
		}
		if cerr := tx.backoffSleepBounded(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		return nil

	case ErrCommitUnknownResult, ErrClusterVersionChanged:
		// MAYBE_COMMITTED: self-conflicting — deep-copy write conflicts to read
		// conflicts so the retry detects if our prior commit actually landed.
		// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_MAYBE_COMMITTED, code).
		//
		// Deep copy: KeyRange.Begin/End are sub-slices of conflictBuf, which
		// reset() reuses. Without a deep copy, retry writes overwrite the same
		// buffer positions, corrupting the selfConflicts data.
		tx.conflictMu.Lock()
		selfConflicts := make([]KeyRange, len(tx.writeConflicts))
		for i, kr := range tx.writeConflicts {
			buf := make([]byte, len(kr.Begin)+len(kr.End))
			copy(buf, kr.Begin)
			copy(buf[len(kr.Begin):], kr.End)
			selfConflicts[i] = KeyRange{Begin: buf[:len(kr.Begin)], End: buf[len(kr.Begin):]}
		}
		tx.conflictMu.Unlock()
		tx.retryCount++
		if tx.db != nil {
			tx.db.countRetryAndLog(ctx, fdbErr.Code, tx.retryCount)
		}
		if cerr := tx.backoffSleepBounded(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		tx.conflictMu.Lock()
		tx.readConflicts = append(tx.readConflicts, selfConflicts...)
		tx.conflictMu.Unlock()
		return nil

	default:
		// RETRYABLE_NOT_COMMITTED (not_committed, database_locked, process_behind,
		// the throttles, blob_granule, all_proxies): exponential backoff. The guard
		// above guarantees this arm is reached only by an onError-retryable code, so
		// "default = retry" can never mis-retry a non-retryable error, and a future
		// retryable code added to onErrorRetryable is handled here by default.
		// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE_NOT_COMMITTED, code).
		tx.retryCount++
		if tx.db != nil {
			tx.db.countRetryAndLog(ctx, fdbErr.Code, tx.retryCount)
		}
		if cerr := tx.backoffSleepBounded(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		return nil
	}
}

// GetReadVersion returns the read version for this transaction, fetching it
// from a GRV proxy if not already set. Matches C++ fdb_transaction_get_read_version.
func (tx *Transaction) GetReadVersion(ctx context.Context) (int64, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return 0, err
	}
	return tx.readVersion, nil
}

// SetReadVersion sets the read version manually.
func (tx *Transaction) SetReadVersion(version int64) {
	tx.readVersionMu.Lock()
	tx.readVersion = version
	tx.hasReadVersion = true
	tx.userSetReadVersion = true
	tx.readVersionMu.Unlock()
}

// SetTimeout sets a timeout in milliseconds for this transaction.
// The timeout is an overall budget from creation time (or last user Reset),
// NOT per-retry. OnError retries share the same deadline.
// A value of 0 disables the timeout. Matches C++ FDB_TR_OPTION_TIMEOUT.
func (tx *Transaction) SetTimeout(ms int64) {
	if ms <= 0 {
		tx.timeout = 0
		tx.deadline = time.Time{}
		return
	}
	tx.timeout = time.Duration(ms) * time.Millisecond
	// Deadline is anchored to creationTime, matching C++:
	// timebomb(options.timeoutInSeconds + creationTime, resetPromise)
	if tx.creationTime.IsZero() {
		tx.creationTime = time.Now()
	}
	tx.deadline = tx.creationTime.Add(tx.timeout)
}

// SetRetryLimit limits the number of retries in OnError.
// A value of 0 means "don't retry at all" (first error escapes).
// A value of -1 means "unlimited" (default behavior).
// Matches C++ FDB_TR_OPTION_RETRY_LIMIT.
func (tx *Transaction) SetRetryLimit(retries int64) {
	if retries < 0 {
		tx.hasRetryLimit = false
		return
	}
	tx.retryLimit = int(retries)
	tx.hasRetryLimit = true
}

// C++ sizeof constants for approximate size calculation. flow/Arena.h:370 wraps StringRef in
// `#pragma pack(push, 4)`, so StringRef is 12 bytes (8-byte data pointer + 4-byte length, NO tail
// padding up to 16) — NOT 16. Therefore, under that 4-byte packing:
//
//	sizeof(KeyRangeRef) = 2×StringRef          = 24   (FDBTypes.h:315 — {const KeyRef begin, end})
//	sizeof(MutationRef) = uint8 type + 2×StringRef(12) + Optional<uint32>(8) + Optional<uint16>(4)
//	                      + bool, all 4-aligned  = 44   (CommitTransaction.h:67)
//
// The earlier 48/32 assumed natural 8-byte StringRef alignment and missed the pack(4); both
// over-counted. Verified byte-exact against libfdb_c by bench TestDifferential_ApproximateSize.
const (
	sizeofMutationRef = 44
	sizeofKeyRangeRef = 24
)

// GetApproximateSize returns the approximate size of the transaction's mutations
// and conflict ranges in bytes. Matches C++ ReadYourWritesTransaction accounting:
// each mutation includes sizeof(MutationRef), each conflict range includes
// sizeof(KeyRangeRef). For set/atomic with write conflicts, C++ also adds the
// key length again for the auto-generated write conflict range.
func (tx *Transaction) GetApproximateSize() int64 {
	// Iterate the buffers UNDER conflictMu, not a released snapshot. This is a
	// public method callers may poll concurrently with Commit on another
	// goroutine, and Commit's auto-reset (postCommitReset) reuses the buffer
	// backing arrays via [:0] — a released snapshot would then read memory a
	// post-reset Set overwrites. Holding the lock across the iteration prevents
	// reset/append from running concurrently with the read. The body is pure CPU
	// (no I/O), so the critical section is short.
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	var size int64
	for _, m := range tx.mutations {
		size += int64(len(m.Key)) + int64(len(m.Value)) + sizeofMutationRef
	}
	for _, r := range tx.readConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	for _, r := range tx.writeConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	// C++ charges a single-key clear's MUTATION part sizeof(KeyRangeRef), not sizeof(MutationRef)
	// (ReadYourWrites.actor.cpp:2431 — it is modeled as a range entry in the write map). The mutation
	// loop above charged every MutClearRange sizeofMutationRef; a single-key clear is shape-
	// indistinguishable from a ClearRange(k, k+\x00), so correct the overcharge here from the Clear()-
	// call count (the write-conflict half already matched: both charge sizeof(KeyRangeRef)).
	size -= int64(tx.singleKeyClearCount) * (sizeofMutationRef - sizeofKeyRangeRef)
	return size
}

// approximateCommitSize sizes the request that WILL be committed: the validated mutation
// snapshot `muts` (the exact set Commit marshals — NOT the live tx.mutations) plus the current
// conflict ranges. The commit-time size check (transaction_too_large, 2101) must use this, not
// GetApproximateSize(): under the concurrent-use contract a Set racing Commit on another
// goroutine appends to tx.mutations BEYOND `muts`, so GetApproximateSize() (which reads the live
// buffer) could fail a small commit for a mutation that is never shipped. `muts` is an
// append-only snapshot (elements never mutated in place) captured by Commit before this runs, so
// iterating it needs no lock; the conflict buffers are read live under conflictMu (the marshal
// likewise ships them from a live snapshot, so counting live conflicts matches what is sent).
func (tx *Transaction) approximateCommitSize(muts []Mutation) int64 {
	// The transaction_too_large (2101) check uses the NATIVE commit accounting: each mutation charged
	// sizeof(MutationRef) and each conflict range sizeof(KeyRangeRef) (the 44/24 under pack(4)). This
	// deliberately does NOT apply GetApproximateSize's single-key-clear adjustment: in the native
	// commit a single-key clear is a ClearRange mutation charged sizeof(MutationRef) [44], not the RYW
	// sizeof(KeyRangeRef) [24]. Verified byte-exact against libfdb_c's 2101 boundary (Set AND
	// single-key-clear workloads) by bench TestDifferential_TransactionSizeLimit — go and cgo reject at
	// the identical limit. (The old 48/32 over-counted here too, rejecting large txns slightly earlier
	// than libfdb_c; the 44/24 fix closed it.)
	var size int64
	for _, m := range muts {
		size += int64(len(m.Key)) + int64(len(m.Value)) + sizeofMutationRef
	}
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	for _, r := range tx.readConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	for _, r := range tx.writeConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End)) + sizeofKeyRangeRef
	}
	return size
}

// GetLocations returns all shard location entries overlapping [begin, end).
func (tx *Transaction) GetLocations(parentCtx context.Context, begin, end []byte, limit int) ([]LocationResult, error) {
	ctx, cancel := tx.opContext(parentCtx) // bound by SetTimeout (RFC-112)
	defer cancel()
	locs, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, limit, false, tx.tenantId, tx.spanContext)
	return locs, tx.mapTimeout(parentCtx, err)
}

// GetAddressesForKey returns the addresses of storage servers responsible for
// the given key. Uses the location cache (queries cluster on miss).
func (tx *Transaction) GetAddressesForKey(parentCtx context.Context, key []byte) ([]string, error) {
	// A cancelled txn returns transaction_cancelled (1025) — C++ getAddressesForKey races
	// resetPromise at op entry (ReadYourWrites.actor.cpp:1837); this path bypasses
	// ensureReadVersion, so gate explicitly (RFC-068).
	if err := tx.checkCancelled(); err != nil {
		return nil, err
	}
	// C++ getAddressesForKey is also bounded by the timebomb (resetPromise,
	// ReadYourWrites.actor.cpp:1843-1848) — bound the locate by SetTimeout (RFC-112).
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.spanContext)
	if err != nil {
		// Tracked (C++ ryw->reading): getAddressesForKey is reading.add'd
		// (ReadYourWrites.actor.cpp:1849), so its failure poisons commit too.
		return nil, tx.trackReadError(tx.mapTimeout(parentCtx, fmt.Errorf("locate key: %w", err)))
	}
	addrs := make([]string, len(loc.Servers))
	for i, s := range loc.Servers {
		addrs[i] = s.Address
		if s.TLS {
			// C++ getAddressesForKey returns address().toString(), which appends ":tls"
			// (NativeAPI.actor.cpp:5747 → flow/network.cpp:215). Address stays clean for dialing.
			addrs[i] += ":tls"
		}
	}
	return addrs, nil
}

// checkTimeout returns a timeout error if the deadline has passed.
func (tx *Transaction) checkTimeout() error {
	if tx.timeout > 0 && time.Now().After(tx.deadline) {
		return &wire.FDBError{Code: ErrTransactionTimedOut}
	}
	return nil
}

// addConflictMu protects readConflicts/writeConflicts from concurrent append.
// The Apple C binding uses a single-threaded actor model so doesn't need this.
// Our Go futures use goroutines, so concurrent Get/Set calls on the same
// transaction race on the conflict slices.

// validateVersionstampOffset checks that a SET_VERSIONSTAMPED_KEY or
// SET_VERSIONSTAMPED_VALUE operand has a valid 4-byte LE offset suffix
// and that the 10-byte versionstamp fits within the operand.
// Matches C++ ReadYourWritesTransaction::atomicOp validation.
func validateVersionstampOffset(data []byte) error {
	if len(data) < 4 {
		return &wire.FDBError{Code: 2000} // client_invalid_operation
	}
	offset := int32(binary.LittleEndian.Uint32(data[len(data)-4:]))
	bodyLen := int32(len(data) - 4) // length without the offset suffix
	if offset < 0 || offset+10 > bodyLen {
		return &wire.FDBError{Code: 2000} // client_invalid_operation
	}
	return nil
}

// keyAfterBytes returns a copy of key with \x00 appended.
// Always allocates — safe for storing in conflict ranges.
func keyAfterBytes(key []byte) []byte {
	r := make([]byte, len(key)+1)
	copy(r, key)
	return r
}

// addReadConflictForKey adds a read conflict range [key, key\x00) using the
// shared conflictBuf. Zero allocs on the hot path (buffer pooled across txns).
func (tx *Transaction) addReadConflictForKey(key []byte) {
	n := len(key)
	tx.conflictMu.Lock()
	buf := tx.conflictBufAlloc(n + n + 1)
	copy(buf, key)
	copy(buf[n:], key)
	buf[2*n] = 0 // explicit zero — pooled buffer may have stale data
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: buf[:n], End: buf[n : n+n+1]})
	tx.conflictMu.Unlock()
}

// addReadConflict adds a read conflict range using the shared conflictBuf.
func (tx *Transaction) addReadConflict(begin, end []byte) {
	tx.conflictMu.Lock()
	buf := tx.conflictBufAlloc(len(begin) + len(end))
	nb := len(begin)
	copy(buf, begin)
	copy(buf[nb:], end)
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: buf[:nb], End: buf[nb:]})
	tx.conflictMu.Unlock()
}

// conflictBufAlloc reserves n bytes from the shared conflict buffer.
// Must be called with conflictMu held.
func (tx *Transaction) conflictBufAlloc(n int) []byte {
	if cap(tx.conflictBuf)-len(tx.conflictBuf) < n {
		reused := false
		// Try reusing a pooled buffer when starting fresh (avoids 4K→8K→16K→32K growth).
		if len(tx.conflictBuf) == 0 {
			cb := conflictBufPool.Get().(*conflictBuf)
			if cap(cb.b) >= n {
				tx.conflictBuf = cb.b[:0]
				tx.conflictBufOwner = cb
				reused = true
			} else {
				conflictBufPool.Put(cb) // too small, return it
			}
		}
		if !reused {
			newCap := max(2*cap(tx.conflictBuf), len(tx.conflictBuf)+n)
			if newCap < 4096 {
				newCap = 4096 // ~30 typical keys
			}
			newBuf := make([]byte, len(tx.conflictBuf), newCap)
			copy(newBuf, tx.conflictBuf)
			tx.conflictBuf = newBuf
		}
	}
	start := len(tx.conflictBuf)
	tx.conflictBuf = tx.conflictBuf[:start+n]
	return tx.conflictBuf[start : start+n]
}

// addWriteConflictForKey adds a write conflict range [key, key\x00). Takes
// conflictMu itself — used by the public AddWriteConflictKey and the dummy-tx
// builder. The Set/Clear/Atomic paths instead call addWriteConflictForKeyLocked
// while already holding the lock, so the mutation and its conflict append
// atomically (see Set).
func (tx *Transaction) addWriteConflictForKey(key []byte) {
	tx.conflictMu.Lock()
	tx.addWriteConflictForKeyLocked(key)
	tx.conflictMu.Unlock()
}

// addWriteConflictForKeyLocked appends a write conflict range [key, key\x00)
// using the shared conflictBuf. Caller MUST hold conflictMu.
//
// nextWriteNoConflict is read AND cleared here on the Set/Clear/Atomic path, so
// two concurrent writes would race on it without the lock. writeConflictsDisabled
// short-circuits WITHOUT touching nextWriteNoConflict — exact prior semantics.
func (tx *Transaction) addWriteConflictForKeyLocked(key []byte) {
	if tx.writeConflictsDisabled {
		return
	}
	if tx.nextWriteNoConflict {
		tx.nextWriteNoConflict = false
		return
	}
	n := len(key)
	buf := tx.conflictBufAlloc(n + n + 1)
	copy(buf, key)
	copy(buf[n:], key)
	buf[2*n] = 0 // explicit zero — pooled buffer may have stale data
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: buf[:n], End: buf[n : n+n+1]})
}

// addWriteConflict adds a write conflict range [begin, end). Takes conflictMu
// itself (public AddWriteConflictRange); the Clear/ClearRange paths call the
// Locked variant under the already-held lock.
func (tx *Transaction) addWriteConflict(begin, end []byte) {
	tx.conflictMu.Lock()
	tx.addWriteConflictLocked(begin, end)
	tx.conflictMu.Unlock()
}

// addWriteConflictLocked appends a write conflict range [begin, end). Caller
// MUST hold conflictMu.
func (tx *Transaction) addWriteConflictLocked(begin, end []byte) {
	if tx.writeConflictsDisabled {
		return
	}
	if tx.nextWriteNoConflict {
		tx.nextWriteNoConflict = false
		return
	}
	buf := tx.conflictBufAlloc(len(begin) + len(end))
	nb := len(begin)
	copy(buf, begin)
	copy(buf[nb:], end)
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: buf[:nb], End: buf[nb:]})
}

// SetNextWriteNoWriteConflictRange causes the next mutation to NOT add a write
// conflict range. Auto-resets after one mutation. Matches C++
// FDB_TR_OPTION_NEXT_WRITE_NO_WRITE_CONFLICT_RANGE.
func (tx *Transaction) SetNextWriteNoWriteConflictRange() {
	tx.nextWriteNoConflict = true
}

// SetPriority sets the transaction priority for GRV requests.
func (tx *Transaction) SetPriority(p TransactionPriority) {
	tx.priority = p
}

// SetCausalReadRisky sets the causal-read-risky flag.
// When set, the read version may not reflect the latest committed writes.
func (tx *Transaction) SetCausalReadRisky(v bool) {
	tx.causalReadRisky = v
}

// SetUseGrvCache opts this transaction in to serving its read version from the
// database's GRV cache (USE_GRV_CACHE, 1101). Off by default — a default
// transaction issues a fresh proxy GRV, matching libfdb_c. RFC-104.
func (tx *Transaction) SetUseGrvCache() {
	tx.useGrvCache = true
}

// SetSkipGrvCache forces this transaction to bypass the GRV cache even if
// SetUseGrvCache was also set (SKIP_GRV_CACHE, 1102 — skip wins). RFC-104.
func (tx *Transaction) SetSkipGrvCache() {
	tx.skipGrvCache = true
}

// SetLockAware sets the lock-aware flag on the commit request.
func (tx *Transaction) SetLockAware(v bool) {
	tx.lockAware = v
}

// SetReadLockAware allows reads on locked databases without granting
// commit access. C++: options.readLockAware — only affects read path.
func (tx *Transaction) SetReadLockAware(v bool) {
	tx.readLockAware = v
}

// LockAware reports whether the commit is lock-aware (sets FLAG_IS_LOCK_AWARE,
// bypassing the locked-database check). Mirrors SetLockAware.
func (tx *Transaction) LockAware() bool {
	return tx.lockAware
}

// ReadLockAware reports whether reads bypass the locked-database check.
// Mirrors SetReadLockAware.
func (tx *Transaction) ReadLockAware() bool {
	return tx.readLockAware
}

// SetSizeLimit sets the maximum transaction size in bytes.
// If the transaction exceeds this size, commit returns error 2101.
// Valid range: [32, 10_000_000]. Out-of-range values cause error 2006 at commit.
// A value of 0 disables the limit.
func (tx *Transaction) SetSizeLimit(limit int64) {
	tx.sizeLimit = limit
}

// SetMaxRetryDelay caps the exponential backoff between retries.
// Value in milliseconds. Matches C++ FDB_TR_OPTION_MAX_RETRY_DELAY.
func (tx *Transaction) SetMaxRetryDelay(ms int64) {
	tx.maxRetryDelay = time.Duration(ms) * time.Millisecond
}

// SetReadYourWritesDisable disables RYW for regular (non-snapshot) reads.
// When set, Get/GetRange always read from the server, ignoring uncommitted writes.
// Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
//
// libfdb_c forbids setting this option after any read or write: it throws
// client_invalid_operation, deferred to the next operation (the option call itself succeeds).
// So if the RYW layer is non-empty (a prior read cached, or a pending write), we POISON the
// transaction — every subsequent read and commit returns 2000 — rather than silently
// disabling RYW mid-transaction (RFC-059). A clean (pre-op) disable is unaffected.
func (tx *Transaction) SetReadYourWritesDisable() {
	tx.rywDisabled = true
	if tx.hadRead.Load() || !tx.ryw.isEmpty() {
		tx.rywPoisonErr = &wire.FDBError{Code: 2000} // client_invalid_operation, surfaces on next op
	}
}

// SetWriteConflictsDisabled disables write conflict ranges for all subsequent
// mutations. Use for insert-only batch writes where keys are guaranteed unique
// and all atomic operations commute (ADD, MAX, MIN). Significantly reduces
// commit request size and eliminates conflict buffer allocations.
func (tx *Transaction) SetWriteConflictsDisabled() {
	tx.writeConflictsDisabled = true
}

// EnsureMutationCapacity pre-sizes the mutations and writeConflicts slices
// to avoid growth allocations during batch writes. Call before a large batch.
func (tx *Transaction) EnsureMutationCapacity(n int) {
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

// SetSnapshotRYWDisable decrements the snapshot-RYW enable count (stored as the
// disabled-oriented inverse, so it increments here). Matches FDB_TR_OPTION_SNAPSHOT_RYW_DISABLE
// (libfdb_c does enabledCount--). When the net count is > 0 (more disables than enables),
// Snapshot.Get/GetRange/GetKey read from the server, bypassing the RYW cache.
func (tx *Transaction) SetSnapshotRYWDisable() {
	tx.snapshotRYWDisableCount++
}

// SetSnapshotRYWEnable re-enables RYW for snapshot reads, undoing one prior
// SetSnapshotRYWDisable. Matches FDB_TR_OPTION_SNAPSHOT_RYW_ENABLE (libfdb_c does
// enabledCount++). The option is a counter, not a toggle: two disables require two enables to
// re-enable, and an enable from the default pushes the count negative (still enabled).
func (tx *Transaction) SetSnapshotRYWEnable() {
	tx.snapshotRYWDisableCount--
}

// SnapshotRYWDisableCount reports the net snapshot-RYW-disable count (> 0 means snapshot reads
// bypass the RYW cache). Read-only accessor used to verify database-level option propagation.
func (tx *Transaction) SnapshotRYWDisableCount() int { return tx.snapshotRYWDisableCount }

// SetSnapshotRYWDisableCount SETS the snapshot-RYW disable counter to n (vs the ++/-- of
// SetSnapshotRYWDisable/Enable). It seeds the per-tx counter to a database default. Setting (not
// incrementing) is idempotent under the retry replay of applyTxDefaults — matching libfdb_c, whose
// reset() re-seeds snapshotRywEnabled = db->snapshotRywEnabled each attempt rather than accumulating.
func (tx *Transaction) SetSnapshotRYWDisableCount(n int) { tx.snapshotRYWDisableCount = n }

// BypassUnreadable reports whether FDB_TR_OPTION_BYPASS_UNREADABLE is set. Read-only accessor used
// to verify database-level option propagation.
func (tx *Transaction) BypassUnreadable() bool { return tx.ryw.bypassUnreadable }

// CausalReadRisky reports whether the GRV causal-read-risky flag is set. Read-only accessor used to
// verify database-level option propagation.
func (tx *Transaction) CausalReadRisky() bool { return tx.causalReadRisky }

// SetTenantId sets the tenant for this transaction. All operations will
// be scoped to the tenant's key space. Use NoTenantID (-1) for no tenant.
func (tx *Transaction) SetTenantId(id int64) {
	tx.tenantId = id
}

// TenantId returns the current tenant ID for this transaction.
// Returns NoTenantID (-1) if no tenant is set.
func (tx *Transaction) TenantId() int64 {
	return tx.tenantId
}

// SetTag adds a tag to this transaction for tag-based throttling.
// Tags are used for throttle backoff calculation on tag_throttled errors.
// Matches C++ FDB_TR_OPTION_AUTO_THROTTLE_TAG / FDB_TR_OPTION_DEBUG_TRANSACTION_IDENTIFIER usage.
func (tx *Transaction) SetTag(tag string) {
	tx.tags = append(tx.tags, tag)
}

// GetTagThrottledDuration returns the total time this transaction was delayed
// by proxy tag throttling across all GRV requests. Matches C++
// Transaction::getTagThrottledDuration() (NativeAPI.actor.cpp:7594).
func (tx *Transaction) GetTagThrottledDuration() float64 {
	return tx.proxyTagThrottledDuration
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

// AddReadConflictRange adds an explicit read conflict range [begin, end).
// If any key in this range is modified by another transaction between
// this transaction's read version and commit, the commit will fail.
// Returns inverted_range (2005) if begin > end. Matches C++ fdb_transaction_add_conflict_range.
func (tx *Transaction) AddReadConflictRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	// C++ addReadConflictRange (ReadYourWrites.actor.cpp:1954-1957, apiVersionAtLeast(300)): reject a
	// range whose begin/end exceeds getMaxReadKey(), EXCEPT the exact metadataVersionKey range. Inverted
	// is checked first (C++ throws it from KeyRangeRef construction before this check).
	maxKey := tx.maxReadKey()
	if (bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0) &&
		!(bytes.Equal(begin, metadataVersionKeyBytes) && bytes.Equal(end, metadataVersionKeyEndBytes)) {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// C++ addReadConflictRange (ReadYourWrites.actor.cpp:1977-1986): when RYW is DISABLED, add the
	// full range directly (tr.addReadConflictRange, :1979); otherwise run updateConflictMap (:1986) —
	// the write-map filter that SUBTRACTS locally-written independent segments (a plain Set before
	// this explicit add was satisfied with no DB read, so it must not conflict). Mirrors the read
	// path (getRangeDir) and addGetKeyConflictRange; without it Go over-conflicts → spurious 1020.
	if tx.rywDisabled {
		tx.addReadConflict(begin, end)
		return nil
	}
	tx.ryw.mu.Lock()
	ranges := tx.ryw.conflictRangesLocked(begin, end)
	tx.ryw.mu.Unlock()
	for _, r := range ranges {
		tx.addReadConflict(r[0], r[1])
	}
	return nil
}

// AddReadConflictKey adds a read conflict on a single key. Like AddReadConflictRange it is filtered
// through the RYW write-map (C++ addReadConflictRange over [key, keyAfter(key)) → updateConflictMap,
// ReadYourWrites.actor.cpp:1986): a self-written independent key adds no conflict; rywDisabled adds
// the full single-key conflict directly (:1979). Identical to the Get-path helper, so delegate.
func (tx *Transaction) AddReadConflictKey(key []byte) {
	tx.addReadConflictForKeyRYW(key)
}

// AddWriteConflictRange adds an explicit write conflict range [begin, end).
// Returns inverted_range (2005) if begin > end. Matches C++ fdb_transaction_add_conflict_range.
func (tx *Transaction) AddWriteConflictRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	// C++ addWriteConflictRange (ReadYourWrites.actor.cpp:2466-2468, apiVersionAtLeast(300)): reject a
	// range whose begin/end exceeds getMaxWriteKey() — note getMaxWriteKey (gated on writeSystemKeys),
	// NOT getMaxReadKey, and NO metadataVersionKey exception (asymmetric with the read path above).
	maxKey := tx.maxWriteKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	tx.addWriteConflict(begin, end)
	return nil
}

// AddWriteConflictKey adds a write conflict on a single key.
func (tx *Transaction) AddWriteConflictKey(key []byte) {
	tx.addWriteConflictForKey(key)
}

// reset clears transaction state for retry, preserving retryCount, backoff,
// timeout, and retryLimit. Matches C++ TransactionState::reset() which
// re-applies options. The deadline is re-computed from timeout so each
// retry gets a fresh timeout window (matches C++ where set_option is
// re-applied on reset, restarting the timer).
// postCommitReset clears mutation buffers and conflict ranges after a
// successful commit, allowing the transaction to be reused. Matches the C++
// client's tryCommit() which does `tr.transaction = CommitTransactionRef()`
// after successful commit. Preserves committedVersion and txnBatchId for
// GetCommittedVersion/GetVersionstamp queries.
func (tx *Transaction) postCommitReset() {
	tx.state.Store(int32(txStateActive))
	tx.readVersionMu.Lock()
	tx.hasReadVersion = false
	tx.userSetReadVersion = false
	tx.readVersion = 0
	tx.readVersionMu.Unlock()
	// Hold conflictMu while clearing ALL three buffers: Watch() goroutines may
	// be concurrently calling AddReadConflictKey() which appends under this lock,
	// and a concurrent Set/Commit reads mutations under it (concurrent-use
	// contract). The mutations clear must be inside too, not above the lock.
	tx.conflictMu.Lock()
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
	tx.singleKeyClearCount = 0 // cleared with mutations (GetApproximateSize accounting)
	// Return conflict buffer to pool for reuse by next transaction.
	if tx.conflictBufOwner != nil {
		tx.conflictBufOwner.b = tx.conflictBuf[:0]
		conflictBufPool.Put(tx.conflictBufOwner)
		tx.conflictBufOwner = nil
		tx.conflictBuf = nil
	} else if cap(tx.conflictBuf) > 0 {
		// Buffer was allocated outside pool (growth path) — wrap and return.
		conflictBufPool.Put(&conflictBuf{b: tx.conflictBuf[:0]})
		tx.conflictBuf = nil
	}
	tx.conflictMu.Unlock()
	tx.ryw.reset()
	tx.rywPoisonErr = nil            // RFC-059: a fresh layer reapplies the option with no poison
	tx.invalidAtomicOpErr.Store(nil) // a fresh transaction can issue valid atomic ops again
	tx.readErrMu.Lock()
	tx.readErr = nil // necessarily nil here (commit succeeded), cleared for reuse symmetry
	tx.readGen++     // detach in-flight reads (C++ resetRyow swaps the reading AndFuture)
	tx.pendingReads = nil
	tx.readErrMu.Unlock()
	tx.hadRead.Store(false)
	// RFC-114: a committed, auto-reset handle begins a NEW logical transaction, so
	// CLEAR the total-latency anchor — the next transaction re-stamps it at ITS first
	// GRV (ensureReadVersion), excluding any idle gap before its first op. (creationTime/
	// deadline are deliberately left alone — this is the metric boundary, not a timeout
	// reset.) Without this, a reused handle's next commit would measure from the prior
	// transaction's start, folding in its work + the idle time between them.
	tx.metricStart = time.Time{}
	// RFC-115 §4 Layer 2: end this committed transaction's "Transaction" otel span and
	// re-anchor a fresh wire span — a reused handle begins a NEW logical transaction, so
	// it must NOT carry the just-committed transaction's traceID. The next op's first GRV
	// starts a fresh Transaction span via ensureTxSpan.
	tx.endTxSpan()
	tx.regenerateSpan()
	// committedVersion and txnBatchId preserved intentionally.
}

func (tx *Transaction) reset() {
	tx.cancelWatches()
	tx.state.Store(int32(txStateActive))
	tx.readVersionMu.Lock()
	tx.hasReadVersion = false
	tx.userSetReadVersion = false // C++ creates fresh state on reset
	tx.readVersion = 0
	tx.readVersionMu.Unlock()
	tx.committedVersion = 0
	tx.hasCommitted = false
	tx.txnBatchId = 0
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
	tx.singleKeyClearCount = 0          // cleared with mutations (GetApproximateSize accounting)
	tx.conflictBuf = tx.conflictBuf[:0] // reuse buffer for retry
	tx.conflictMu.Unlock()
	tx.ryw.reset()
	tx.rywPoisonErr = nil            // RFC-059: a fresh layer reapplies the option with no poison
	tx.invalidAtomicOpErr.Store(nil) // a fresh transaction can issue valid atomic ops again
	tx.readErrMu.Lock()
	tx.readErr = nil // C++ resetRyow(): reading = AndFuture() (:2715)
	tx.readGen++     // detach in-flight reads (C++ resetRyow swaps the reading AndFuture)
	tx.pendingReads = nil
	tx.readErrMu.Unlock()
	tx.hadRead.Store(false)
	// Re-apply timeout from creationTime (NOT time.Now()). C++ semantics:
	// onError does NOT update creationTime, so the timeout is an overall
	// budget across all retries. Only user-facing Reset() updates creationTime.
	if tx.timeout > 0 {
		tx.deadline = tx.creationTime.Add(tx.timeout)
	}
	// Clear accumulated proxy tag throttle duration on retry.
	// Tags themselves are preserved across reset (C++ keeps tags across retries).
	tx.proxyTagThrottledDuration = 0
	// End the prior attempt's "Transaction" otel span and generate a fresh wire span
	// per attempt (≈ C++ generateSpanID at cloneAndReset, RFC-115 §4). An injected
	// SPAN_PARENT linkage is preserved (regenerateSpan honors spanParent). The next
	// attempt's first GRV re-starts a fresh Transaction span (ensureTxSpan).
	tx.endTxSpan()
	tx.regenerateSpan()
	// Preserved across reset (match C++ persistent option re-application):
	// retryCount, backoff, timeout, retryLimit, priority, causalReadRisky,
	// lockAware, readLockAware, sizeLimit, maxRetryDelay, rywDisabled,
	// snapshotRYWDisableCount, tenantId, creationTime, tags.
}

// nextBackoff returns the current backoff duration with jitter, then grows
// the backoff for the next call. Matches C++ getBackoff in NativeAPI.actor.cpp.
// errCode determines the backoff cap: the resource-constrained bucket —
// commit_proxy_memory_limit_exceeded (1042), grv_proxy_memory_limit_exceeded
// (1078), throttled_hot_shard (1235), and range_locked (1242) — uses
// RESOURCE_CONSTRAINED_MAX_BACKOFF (30s) and IGNORES the user's maxRetryDelay;
// all other errors use the user's maxRetryDelay or DEFAULT_MAX_BACKOFF (1s). The
// two branches are mutually exclusive (see the cap selection below).
func (tx *Transaction) nextBackoff(errCode int) time.Duration {
	if tx.backoff == 0 {
		tx.backoff = defaultBackoff
	}
	// C++ pattern: return current * jitter, then grow for next time.
	jitter := rand.Float64()
	if tx.backoffJitter != nil {
		jitter = tx.backoffJitter() // test-only deterministic override
	}
	delay := time.Duration(float64(tx.backoff) * jitter)

	// Tag throttle: use server-supplied duration for tag_throttled errors.
	// C++ getBackoff(): max(backoff, min(TAG_THROTTLE_RECHECK_INTERVAL, tagThrottleDuration))
	if errCode == ErrTagThrottled && len(tx.tags) > 0 && tx.db != nil {
		tagDur := tx.db.tagThrottles.maxDuration(tx.priority, tx.tags)
		if tagDur > 0 {
			capped := tagDur
			if capped > tagThrottleRecheckInterval {
				capped = tagThrottleRecheckInterval
			}
			if capped > delay {
				delay = capped
			}
		}
	}

	// For proxy_tag_throttled, accumulate duration.
	// C++ NativeAPI.actor.cpp: trState->cx->throttledTags check in getBackoff.
	if errCode == ErrProxyTagThrottled {
		tx.proxyTagThrottledDuration += proxyMaxTagThrottleDuration.Seconds()
	}

	// C++ getBackoff(): the resource-constrained bucket (the four codes below) uses
	// RESOURCE_CONSTRAINED_MAX_BACKOFF exclusively (user's maxRetryDelay is IGNORED);
	// all other errors use the user's maxRetryDelay (or DEFAULT_MAX_BACKOFF). The two
	// branches are mutually exclusive — no min/max combining. (Code list + values are
	// in the function doc-comment, the single authoritative description.)
	var cap time.Duration
	if errCode == ErrProxyMemoryLimitExceeded || errCode == ErrGrvProxyMemoryLimit ||
		errCode == ErrThrottledHotShard || errCode == ErrRangeLocked {
		cap = resourceConstrainedMaxBackoff
	} else {
		cap = maxBackoff
		if tx.maxRetryDelay > 0 {
			cap = tx.maxRetryDelay
		}
	}
	tx.backoff = time.Duration(math.Min(float64(tx.backoff)*backoffGrowthRate, float64(cap)))
	return delay
}

// SetBypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE
// (ReadYourWrites.actor.cpp:2611-2613): reads of keys with pending
// versionstamped writes return the write-map value with the placeholder
// bytes as written instead of failing with accessed_unreadable (1036);
// SVK's unmodified-unreadable candidate range reads through to storage.
// RFC-098.
func (tx *Transaction) SetBypassUnreadable(v bool) {
	tx.ryw.setBypassUnreadable(v)
}
