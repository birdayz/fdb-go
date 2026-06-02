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

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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
	ErrProcessBehind             = 1037 // process_behind
	ErrDatabaseLocked            = 1038 // database_locked
	ErrClusterVersionChanged     = 1039 // cluster_version_changed (MAYBE_COMMITTED)
	ErrProxyMemoryLimitExceeded  = 1042 // proxy_memory_limit_exceeded
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
	conflictMu       sync.Mutex
	mutations        []Mutation
	readConflicts    []KeyRange
	writeConflicts   []KeyRange
	conflictBuf      []byte       // batch-allocated backing store for conflict range keys
	conflictBufOwner *conflictBuf // pool handle, avoids alloc on Put

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

	// writeConflictsDisabled: when true, ALL mutations skip adding write conflict
	// ranges. Used for insert-only batch writes where all keys are unique (no
	// write-write conflicts possible) and all atomics commute. Reduces commit
	// request size significantly.
	writeConflictsDisabled bool

	// rywDisabled: when true, regular Get/GetRange bypass the RYW cache and
	// always read from the server. Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
	rywDisabled bool

	// snapshotRYWDisabled: when true, Snapshot.Get/GetRange bypass the RYW cache.
	// Matches FDB_TR_OPTION_SNAPSHOT_RYW_DISABLE. Note: by default, snapshot
	// reads DO go through the RYW cache (matching FDB_TR_OPTION_SNAPSHOT_RYW_ENABLE).
	snapshotRYWDisabled bool

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
// Snapshot reads go through the RYW cache unless snapshotRYWDisabled is set.
func (s *Snapshot) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	// Same system key check as regular Get.
	if bytes.Compare(key, s.tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, &wire.FDBError{Code: 2004}
	}
	if s.tx.snapshotRYWDisabled {
		return s.tx.getValue(ctx, key)
	}
	return s.tx.ryw.get(ctx, key, s.tx.getValue)
}

// GetKey resolves a key selector without adding a read conflict range.
// Snapshot reads go through the snapshot cache, and — by default — SEE the txn's own
// pending writes (matching Snapshot.Get/GetRange and libfdb_c, where snapshot RYW is
// enabled unless SetSnapshotRYWDisable). With snapshotRYWDisabled the write map is
// bypassed (snapshot cache only).
func (s *Snapshot) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	if bytes.Compare(selectorKey, s.tx.maxReadKey()) > 0 {
		return nil, &wire.FDBError{Code: 2004}
	}
	return s.tx.ryw.getKeyRYW(ctx, selectorKey, orEqual, offset, s.tx.maxReadKey(), !s.tx.snapshotRYWDisabled, s.tx.getRange)
}

// GetRange reads a range without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshotRYWDisabled is set.
func (s *Snapshot) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, false, err
	}
	maxKey := s.tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	if s.tx.snapshotRYWDisabled {
		return s.tx.getRange(ctx, begin, end, limit, false)
	}
	return s.tx.ryw.getRange(ctx, begin, end, limit, false, s.tx.getRange)
}

// GetRangeReverse reads a range in reverse without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshotRYWDisabled is set.
func (s *Snapshot) GetRangeReverse(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, false, err
	}
	maxKey := s.tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	if s.tx.snapshotRYWDisabled {
		return s.tx.getRange(ctx, begin, end, limit, true)
	}
	return s.tx.ryw.getRange(ctx, begin, end, limit, true, s.tx.getRange)
}

// GetReadVersion returns the read version for this transaction via its snapshot view.
func (s *Snapshot) GetReadVersion(ctx context.Context) (int64, error) {
	return s.tx.GetReadVersion(ctx)
}

func (tx *Transaction) ensureReadVersion(ctx context.Context) error {
	if txState(tx.state.Load()) == txStateCancelled {
		return fmt.Errorf("transaction cancelled")
	}
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}
	tx.readVersionMu.Lock()
	if !tx.hasReadVersion {
		flags := tx.grvFlags()
		rv, err := tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags)
		if err != nil {
			tx.readVersionMu.Unlock()
			return err
		}
		tx.readVersion = rv
		tx.hasReadVersion = true
	}
	userSet := tx.userSetReadVersion
	rv := tx.readVersion
	tx.readVersionMu.Unlock()
	// C++ DatabaseContext::validateVersion(): reject user-set read versions
	// outside the acceptable range. If the client hasn't seen a version yet
	// (minAcceptableReadVersion==0), a GRV is needed first to establish the
	// baseline. C++ does this in startTransaction() before validateVersion().
	if tx.db != nil && userSet {
		if tx.db.minAcceptableReadVersion.Load() == 0 {
			// Bootstrap: fetch a version to establish the minimum.
			flags := tx.grvFlags()
			_, _ = tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags)
		}
		if err := tx.db.validateVersion(rv); err != nil {
			return err
		}
	}
	return nil
}

// Get reads a single key. Returns nil if the key doesn't exist.
func (tx *Transaction) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// Special keys (\xff\xff prefix) don't add read conflicts — C++ resolves
	// them internally without going through the resolver conflict map.
	if !isSpecialKey(key) {
		tx.addReadConflictForKey(key)
	}
	if tx.rywDisabled {
		return tx.getValue(ctx, key)
	}
	return tx.ryw.get(ctx, key, tx.getValue)
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
	// Legal key range check BEFORE sending — matches Transaction.Get. The illegal
	// key must be rejected at enqueue, not after the frame is on the wire. RFC-010 #3.
	// C++ RYW::getValue: if (key >= getMaxReadKey() && key != metadataVersionKey)
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	if !isSpecialKey(key) {
		tx.addReadConflictForKey(key)
	}

	// Check RYW cache.
	tx.ryw.mu.Lock()
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

	// Locate shard.
	loc, locErr := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId)
	if locErr != nil {
		return nil, nil, fmt.Errorf("locate key: %w", locErr)
	}
	if len(loc.Servers) == 0 {
		return nil, nil, fmt.Errorf("no storage servers for key")
	}

	// Send request without waiting for response.
	for _, server := range loc.Servers {
		conn, dialErr := tx.db.getOrDial(ctx, server.Address)
		if dialErr != nil {
			tx.db.handleConnError(server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		body, poolBuf := buildGetValueRequest(key, tx.readVersion, tx.lockAware || tx.readLockAware, tx.tenantId, replyToken, server.Token)
		// Note: can't pool body for SendFrameDeferred — writeLoop holds reference.
		_ = poolBuf
		if sendErr := conn.SendFrameDeferred(server.Token, body); sendErr != nil {
			replyHandle.Cancel()
			replyHandle.Release()
			tx.db.handleConnError(server.Address)
			continue
		}
		timer := getTimer(DefaultRPCTimeout)
		return nil, &PendingGet{key: key, tx: tx, addr: server.Address, replyCh: replyCh, replyHandle: replyHandle, conn: conn, ctx: ctx, timer: timer}, nil
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
	flushed     bool
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
	if !p.flushed {
		p.flushed = true
		if err := p.conn.Flush(); err != nil {
			// The request never reached the server — mark the connection bad
			// (parity with the sync getValue/SendFrame path) and re-locate+retry.
			p.tx.db.handleConnError(p.addr)
			p.replyHandle.Cancel()
			p.replyHandle.Release()
			putTimer(p.timer)
			return p.tx.getValue(p.ctx, p.key)
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
			return p.tx.getValue(p.ctx, p.key)
		}
		val, _, err := parseGetValueReply(resp.Body)
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			p.tx.db.locCache.invalidate(p.key, p.tx.tenantId)
			return p.tx.getValue(p.ctx, p.key)
		}
		return val, err
	case <-p.timer.C:
		p.replyHandle.Cancel()
		return p.tx.getValue(p.ctx, p.key)
	case <-p.ctx.Done():
		p.replyHandle.Cancel()
		return nil, p.ctx.Err()
	}
}

// GetKey resolves a key selector to the actual key in the database.
func (tx *Transaction) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	// C++ RYW::getKey: if (key.getKey() > getMaxReadKey()) → key_outside_legal_range
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
		return nil, err
	}
	// Read-conflict range: getKey conflicts over the RANGE between the selector base
	// and the resolved key (C++ addConflictRange(GetKeyReq), ReadYourWrites.actor.cpp:230),
	// NOT a single key — a concurrent write anywhere in that span must conflict.
	if !isSpecialKey(selectorKey) {
		tx.addGetKeyConflictRange(selectorKey, orEqual, offset, resolved)
	}
	return resolved, nil
}

// addGetKeyConflictRange adds the read-conflict range for a getKey, spanning the
// selector base ↔ resolved key (orEqual-adjusted, oriented by offset sign) — a port of
// C++ addConflictRange(GetKeyReq) (ReadYourWrites.actor.cpp:230-243). This fixes the
// prior single-key conflict (which under-conflicted: a concurrent write between the
// base and the resolved key would not conflict, an unsafe divergence).
//
// C++ then runs updateConflictMap (:335) to SUBTRACT segments satisfied locally with no
// DB read (INDEPENDENT writes + cleared ranges). We DON'T do that subtraction: the
// rywCache eagerly folds resolved atomics into plain entries (hasAtomics→false) and
// moves a matched CompareAndClear into the cleared list, so it no longer preserves
// which keys were DEPENDENT (read the base). Filtering on the post-fold state would drop
// a required read conflict for a folded dependent atomic — an UNSAFE under-conflict
// (codex). So we keep the FULL range: it over-conflicts on the no-DB-read segments
// (extra retries, always SAFE) rather than risk a missed conflict. The exact
// updateConflictMap filtering needs the rywCache to preserve per-key op-type (the same
// gap as the deferred phantom-slot) and is tracked under RFC-056.
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
	if bytes.Compare(begin, end) < 0 {
		tx.addReadConflict(begin, end)
	}
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
		return nil, false, err
	}
	// C++ RYW::getRange: if (begin > maxKey || end > maxKey) → key_outside_legal_range
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	// Only add read conflict if range is valid (begin <= end) and not special keys.
	// C++ client validates inverted ranges and handles \xff\xff keys internally
	// without adding resolver conflict ranges.
	if bytes.Compare(begin, end) <= 0 && !isSpecialKey(begin) && !isSpecialKey(end) {
		tx.addReadConflict(begin, end)
	}

	if tx.rywDisabled {
		return tx.getRange(ctx, begin, end, limit, reverse)
	}
	return tx.ryw.getRange(ctx, begin, end, limit, reverse, tx.getRange)
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
	if len(key) > getMaxClearKeySize(key) {
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
		tx.ryw.atomic(op, key, operand)
	}
}

// Commit sends mutations to a commit proxy.
// After successful commit, the transaction is automatically reset for reuse
// (mutations and conflict ranges cleared, read version invalidated).
// This matches the C client's behavior where fdb_transaction_set() can be
// called after commit to start building a new transaction.
func (tx *Transaction) Commit(ctx context.Context) error {
	if txState(tx.state.Load()) != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}

	// Validate size limit bounds. C++: valid range is [32, 10_000_000].
	// Out-of-range values return invalid_option_value (2006) at commit time.
	if tx.sizeLimit > 0 && (tx.sizeLimit < 32 || tx.sizeLimit > 10_000_000) {
		tx.state.Store(int32(txStateErrored))
		return &wire.FDBError{Code: 2006} // invalid_option_value
	}

	// Enforce size limit if set. Matches C++ FDB_TR_OPTION_SIZE_LIMIT.
	if tx.sizeLimit > 0 && tx.GetApproximateSize() > tx.sizeLimit {
		tx.state.Store(int32(txStateErrored))
		return &wire.FDBError{Code: 2101} // transaction_too_large
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
	tx.conflictMu.Unlock()

	// C++ RYW write checks (deferred to commit since our Set/Clear are void):
	// - set(): if key == metadataVersionKey → client_invalid_operation
	//          if key >= maxWriteKey → key_outside_legal_range
	// - atomicOp(): if key == metadataVersionKey → allow (only SVV)
	//               if key >= maxWriteKey → key_outside_legal_range
	maxWrite := tx.maxWriteKey()
	for _, m := range muts {
		if bytes.Equal(m.Key, metadataVersionKeyBytes) {
			// metadataVersionKey is exempt — only allowed via SetVersionstampedValue
			continue
		}
		if bytes.Compare(m.Key, maxWrite) >= 0 {
			tx.state.Store(int32(txStateErrored))
			return &wire.FDBError{Code: 2004} // key_outside_legal_range
		}
		if m.Type == MutClearRange {
			// ClearRange: also check end key (stored in Value).
			// C++ clear(range): if (range.begin > maxKey || range.end > maxKey) → reject
			if bytes.Compare(m.Value, maxWrite) > 0 {
				tx.state.Store(int32(txStateErrored))
				return &wire.FDBError{Code: 2004}
			}
			// Clear key SIZES are clamped at build time (Clear/ClearRange), matching
			// C++ clear() which translates an oversized range rather than rejecting —
			// so no key_too_large check here.
			continue
		}
		// set()/atomicOp() reject oversized keys/values. The C binding aborts the
		// process (CATCH_AND_DIE on the key_too_large/value_too_large throw); we
		// reject the commit instead so the oversized data never reaches the shared
		// cluster. See sizelimits.go.
		//
		// hasRawAccess: C++ sets options.rawAccess = true for ANY of RAW_ACCESS,
		// ACCESS_SYSTEM_KEYS, or READ_SYSTEM_KEYS (NativeAPI.actor.cpp:7159-7170 —
		// the three cases fall through to one assignment). We don't model the bare
		// RAW_ACCESS option, but ACCESS_SYSTEM_KEYS == writeSystemKeys and
		// READ_SYSTEM_KEYS == readSystemKeys, and either raises the non-system key
		// limit by the tenant-prefix slack (KEY_SIZE_LIMIT+8). Passing false here
		// would reject 10001-10008-byte keys that libfdb_c accepts.
		//
		// BUT the +8 slack IS the tenant-prefix allowance (getMaxWriteKeySize's
		// `tenantSize = hasRawAccess ? PREFIX_SIZE : 0`, NativeAPI.actor.cpp:11630):
		// it exists for raw access where the CALLER already included the 8-byte
		// prefix. When THIS client will prepend the prefix itself (tenantId >= 0,
		// commitpath.go), the user key must stay within KEY_SIZE_LIMIT so the
		// prefixed physical key is ≤ KEY_SIZE_LIMIT+8 — otherwise a 10001-10008-byte
		// user key serializes to a 10009-10016-byte physical key. C++ forbids
		// raw-access options on tenant transactions for exactly this reason; we gate
		// the slack on the no-tenant case to the same effect.
		rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
		if len(m.Key) > getMaxWriteKeySize(m.Key, rawAccess) {
			tx.state.Store(int32(txStateErrored))
			return &wire.FDBError{Code: 2102} // key_too_large
		}
		if len(m.Value) > valueSizeLimit {
			tx.state.Store(int32(txStateErrored))
			return &wire.FDBError{Code: 2103} // value_too_large
		}
	}

	// Validate versionstamp offsets. C++ ReadYourWritesTransaction::atomicOp
	// validates immediately; we defer to commit time since Atomic() is void.
	// Error: client_invalid_operation (2000).
	for _, m := range muts {
		switch m.Type {
		case MutSetVersionstampedKey:
			if err := validateVersionstampOffset(m.Key); err != nil {
				tx.state.Store(int32(txStateErrored))
				return err
			}
		case MutSetVersionstampedValue:
			if err := validateVersionstampOffset(m.Value); err != nil {
				tx.state.Store(int32(txStateErrored))
				return err
			}
		}
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
	if err := tx.commit(ctx, muts); err != nil {
		return err
	}

	// Feed committed version to GRV cache so subsequent reads see this write.
	if tx.committedVersion > 0 {
		tx.db.grvCache.update(time.Now(), tx.committedVersion)
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
	tx.state.Store(int32(txStateCancelled))
}

// Reset resets the transaction to a clean state, as if newly created from Database.
// Unlike the internal reset() used by OnError (which preserves retryCount/backoff),
// this clears everything including retry state. Options set via Set*() are preserved
// across Reset, matching C++ ReadYourWritesTransaction::reset() + applyPersistentOptions.
// Updates creationTime so the timeout budget restarts (matches C++ reset() behavior).
//
// Note: in-flight Watch() calls are NOT cancelled by Reset(). Use context cancellation
// to cancel pending watches. This differs from C++ where reset triggers
// resetPromise.sendError(transaction_cancelled).
func (tx *Transaction) Reset() {
	tx.retryCount = 0
	tx.backoff = 0
	// C++ reset() updates creationTime = now(), restarting timeout window.
	tx.creationTime = time.Now()
	tx.reset()
}

// cancelWatches cancels any in-flight Watch() calls by cancelling the
// watch context. Matches C++ resetRyow() which sends transaction_cancelled
// through resetPromise to cancel pending watches.
func (tx *Transaction) cancelWatches() {
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

// OnError handles a transaction error. Returns nil if the error is retryable
// (the transaction has been reset for retry). Returns the error if non-retryable
// or ctx.Err() if ctx fires during the backoff sleep.
func (tx *Transaction) OnError(ctx context.Context, err error) error {
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		tx.state.Store(int32(txStateErrored))
		return err
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

	switch fdbErr.Code {
	case ErrTransactionTooOld, ErrFutureVersion:
		// Version-related: fixed delay, no backoff growth.
		// C++ NativeAPI.actor.cpp: min(FUTURE_VERSION_RETRY_DELAY, maxBackoff).
		delay := futureVersionDelay
		if tx.maxRetryDelay > 0 && tx.maxRetryDelay < delay {
			delay = tx.maxRetryDelay
		}
		tx.retryCount++
		if cerr := backoffSleep(ctx, delay); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		return nil

	case ErrNotCommitted, ErrDatabaseLocked, ErrProcessBehind,
		ErrBatchTransactionThrottled, ErrTagThrottled, ErrProxyTagThrottled,
		ErrBlobGranuleRequestFailed, ErrAllProxiesUnreachable:
		// RETRYABLE_NOT_COMMITTED: exponential backoff.
		// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE_NOT_COMMITTED, code).
		tx.retryCount++
		if cerr := backoffSleep(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
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
		if cerr := backoffSleep(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
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
		if cerr := backoffSleep(ctx, tx.nextBackoff(fdbErr.Code)); cerr != nil {
			tx.state.Store(int32(txStateErrored))
			return cerr
		}
		tx.reset()
		tx.conflictMu.Lock()
		tx.readConflicts = append(tx.readConflicts, selfConflicts...)
		tx.conflictMu.Unlock()
		return nil

	default:
		tx.state.Store(int32(txStateErrored))
		return err
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

// C++ sizeof constants for approximate size calculation.
// MutationRef: uint8 type + 2×StringRef (16 bytes each) + Optional<uint32> + Optional<uint16> + bool ≈ 48 bytes.
// KeyRangeRef: 2×StringRef (16 bytes each) = 32 bytes.
const (
	sizeofMutationRef = 48
	sizeofKeyRangeRef = 32
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
	return size
}

// GetLocations returns all shard location entries overlapping [begin, end).
func (tx *Transaction) GetLocations(ctx context.Context, begin, end []byte, limit int) ([]LocationResult, error) {
	return tx.db.locCache.locateRange(tx.db, ctx, begin, end, limit, false, tx.tenantId)
}

// GetAddressesForKey returns the addresses of storage servers responsible for
// the given key. Uses the location cache (queries cluster on miss).
func (tx *Transaction) GetAddressesForKey(ctx context.Context, key []byte) ([]string, error) {
	loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId)
	if err != nil {
		return nil, fmt.Errorf("locate key: %w", err)
	}
	addrs := make([]string, len(loc.Servers))
	for i, s := range loc.Servers {
		addrs[i] = s.Address
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
func (tx *Transaction) SetReadYourWritesDisable() {
	tx.rywDisabled = true
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

// SetSnapshotRYWDisable disables RYW for snapshot reads.
// When set, Snapshot.Get/GetRange always read from the server.
// Matches FDB_TR_OPTION_SNAPSHOT_RYW_DISABLE.
func (tx *Transaction) SetSnapshotRYWDisable() {
	tx.snapshotRYWDisabled = true
}

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
	tx.addReadConflict(begin, end)
	return nil
}

// AddReadConflictKey adds a read conflict on a single key.
func (tx *Transaction) AddReadConflictKey(key []byte) {
	tx.addReadConflictForKey(key)
}

// AddWriteConflictRange adds an explicit write conflict range [begin, end).
// Returns inverted_range (2005) if begin > end. Matches C++ fdb_transaction_add_conflict_range.
func (tx *Transaction) AddWriteConflictRange(begin, end []byte) error {
	if bytes.Compare(begin, end) > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
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
	tx.conflictBuf = tx.conflictBuf[:0] // reuse buffer for retry
	tx.conflictMu.Unlock()
	tx.ryw.reset()
	// Re-apply timeout from creationTime (NOT time.Now()). C++ semantics:
	// onError does NOT update creationTime, so the timeout is an overall
	// budget across all retries. Only user-facing Reset() updates creationTime.
	if tx.timeout > 0 {
		tx.deadline = tx.creationTime.Add(tx.timeout)
	}
	// Clear accumulated proxy tag throttle duration on retry.
	// Tags themselves are preserved across reset (C++ keeps tags across retries).
	tx.proxyTagThrottledDuration = 0
	// Preserved across reset (match C++ persistent option re-application):
	// retryCount, backoff, timeout, retryLimit, priority, causalReadRisky,
	// lockAware, readLockAware, sizeLimit, maxRetryDelay, rywDisabled,
	// snapshotRYWDisabled, tenantId, creationTime, tags.
}

// nextBackoff returns the current backoff duration with jitter, then grows
// the backoff for the next call. Matches C++ getBackoff in NativeAPI.actor.cpp.
// errCode determines the backoff cap: proxy memory errors (1042, 1078) use
// RESOURCE_CONSTRAINED_MAX_BACKOFF (30s), all others use DEFAULT_MAX_BACKOFF (1s).
func (tx *Transaction) nextBackoff(errCode int) time.Duration {
	if tx.backoff == 0 {
		tx.backoff = defaultBackoff
	}
	// C++ pattern: return current * jitter, then grow for next time.
	delay := time.Duration(float64(tx.backoff) * rand.Float64())

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

	// C++ getBackoff(): proxy memory errors use RESOURCE_CONSTRAINED_MAX_BACKOFF
	// exclusively (user's maxRetryDelay is IGNORED). All other errors use
	// user's maxRetryDelay (or DEFAULT_MAX_BACKOFF). The two branches are
	// mutually exclusive — no min/max combining.
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
