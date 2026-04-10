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
	ErrWrongShardServer          = 1062 // wrong_shard_server (Layer 2 only)
	ErrTransactionTimedOut       = 1031 // transaction_timed_out (NEVER retryable)
	ErrProcessBehind             = 1037 // process_behind
	ErrDatabaseLocked            = 1038 // database_locked
	ErrClusterVersionChanged     = 1039 // cluster_version_changed (MAYBE_COMMITTED)
	ErrProxyMemoryLimitExceeded  = 1042 // proxy_memory_limit_exceeded
	ErrBatchTransactionThrottled = 1051 // batch_transaction_throttled
	ErrGrvProxyMemoryLimit       = 1078 // grv_proxy_memory_limit_exceeded
	ErrTagThrottled              = 1213 // tag_throttled
	ErrProxyTagThrottled         = 1223 // proxy_tag_throttled
	ErrThrottledHotShard         = 1235 // transaction_throttled_hot_shard
	ErrRangeLocked               = 1242 // transaction_rejected_range_locked
	ErrOperationFailed           = 4    // operation_failed (endpoint not supported)
	ErrAllAlternativesFailed     = 1006 // all_alternatives_failed (Layer 2 only)
	ErrAllProxiesUnreachable     = 1200 // Go-internal: all proxies failed at Layer 2
	ErrInvertedRange             = 2005 // inverted_range (begin > end)
)

// Client constants. These mirror CLIENT_KNOBS in NativeAPI.actor.cpp.
const (
	NoTenantID           int64 = -1
	UnlimitedBytes       int32 = 0x7FFFFFFF
	DefaultRPCTimeout          = 5 * time.Second
	CoordinatorTimeout         = 30 * time.Second // OpenDatabaseCoordRequest + GRV batch context
	BootstrapMaxBackoff        = 5 * time.Second  // bootstrap retry backoff cap
	MaxWrongShardRetries       = 5
)

// C++ version constants from flow/flow.h.
const (
	LatestVersion  int64 = -2 // C++ latestVersion — used in GetKeyServerLocationsRequest.MinTenantVersion
	InvalidVersion int64 = -1 // C++ invalidVersion — used in GetReadVersionRequest.MaxVersion
)

// Backoff constants — match C++ CLIENT_KNOBS.
const (
	defaultBackoff     = 10 * time.Millisecond // C++: DEFAULT_BACKOFF
	backoffGrowthRate  = 2.0                   // C++: BACKOFF_GROWTH_RATE
	maxBackoff         = 1 * time.Second       // C++: DEFAULT_MAX_BACKOFF
	futureVersionDelay = 10 * time.Millisecond // C++: FUTURE_VERSION_RETRY_DELAY
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

type txState int

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
	state txState

	readVersion      int64
	hasReadVersion   bool
	committedVersion int64
	hasCommitted     bool // true after at least one successful commit
	txnBatchId       uint16

	// conflictMu protects mutations, readConflicts, writeConflicts from concurrent
	// access. The Apple C binding uses a single-threaded actor model. Our Go futures
	// use goroutines, so concurrent Get/Set calls on the same transaction race.
	conflictMu     sync.Mutex
	mutations      []Mutation
	readConflicts  []KeyRange
	writeConflicts []KeyRange

	retryCount int
	backoff    time.Duration

	// tenantId: if not NoTenantID (-1), all operations are scoped to this
	// tenant's key space. Set via SetTenantId() before any reads/commits.
	tenantId int64

	// Timeout: if non-zero, operations fail with ErrTransactionTimedOut
	// after this duration from transaction creation (or last reset).
	timeout  time.Duration
	deadline time.Time

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

	// rywDisabled: when true, regular Get/GetRange bypass the RYW cache and
	// always read from the server. Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
	rywDisabled bool

	// snapshotRYWDisabled: when true, Snapshot.Get/GetRange bypass the RYW cache.
	// Matches FDB_TR_OPTION_SNAPSHOT_RYW_DISABLE. Note: by default, snapshot
	// reads DO go through the RYW cache (matching FDB_TR_OPTION_SNAPSHOT_RYW_ENABLE).
	snapshotRYWDisabled bool

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
	if s.tx.snapshotRYWDisabled {
		return s.tx.getValue(ctx, key)
	}
	return s.tx.ryw.get(ctx, key, s.tx.getValue)
}

// GetKey resolves a key selector without adding a read conflict range.
func (s *Snapshot) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	return s.tx.getKey(ctx, selectorKey, orEqual, offset)
}

// GetRange reads a range without adding a read conflict range.
// Snapshot reads go through the RYW cache unless snapshotRYWDisabled is set.
func (s *Snapshot) GetRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, bool, error) {
	if err := s.tx.ensureReadVersion(ctx); err != nil {
		return nil, false, err
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
	if tx.state == txStateCancelled {
		return fmt.Errorf("transaction cancelled")
	}
	if tx.state != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}
	if !tx.hasReadVersion {
		flags := tx.grvFlags()
		rv, err := tx.db.grvBatchers[grvBatcherIndex(flags)].getReadVersion(tx.db, ctx, flags)
		if err != nil {
			return err
		}
		tx.readVersion = rv
		tx.hasReadVersion = true
	}
	return nil
}

// Get reads a single key. Returns nil if the key doesn't exist.
func (tx *Transaction) Get(ctx context.Context, key []byte) ([]byte, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	// System keys (\xff\xff prefix) don't add read conflicts — C++ resolves
	// them internally without going through the resolver conflict map.
	if !isSystemKey(key) {
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
	if !isSystemKey(key) {
		tx.addReadConflictForKey(key)
	}

	// Check RYW cache.
	tx.ryw.mu.Lock()
	if entry, ok := tx.ryw.writes[string(key)]; ok {
		if !entry.hasAtomics {
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
		replyToken, replyCh, cancelReply := conn.PrepareReply()
		body := buildGetValueRequest(key, tx.readVersion, tx.lockAware || tx.readLockAware, tx.tenantId, replyToken, server.Token)
		if sendErr := conn.SendFrameDeferred(server.Token, body); sendErr != nil {
			cancelReply()
			tx.db.handleConnError(server.Address)
			continue
		}
		timer := getTimer(DefaultRPCTimeout)
		return nil, &PendingGet{replyCh: replyCh, cancelReply: cancelReply, conn: conn, ctx: ctx, timer: timer}, nil
	}
	return nil, nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// PendingGet represents a GetValue request that has been sent but not yet resolved.
type PendingGet struct {
	replyCh     <-chan transport.Response
	cancelReply func()
	conn        *transport.Conn
	ctx         context.Context
	timer       *time.Timer
	flushed     bool
}

// Resolve blocks until the response arrives or timeout.
// Flushes the write buffer on first call to ensure the request reaches the server.
func (p *PendingGet) Resolve() ([]byte, error) {
	if !p.flushed {
		p.flushed = true
		p.conn.Flush()
	}
	defer putTimer(p.timer)
	select {
	case resp := <-p.replyCh:
		if resp.Err != nil {
			return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
		}
		return parseGetValueReply(resp.Body)
	case <-p.timer.C:
		p.cancelReply()
		return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
	case <-p.ctx.Done():
		p.cancelReply()
		return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
	}
}

// GetKey resolves a key selector to the actual key in the database.
func (tx *Transaction) GetKey(ctx context.Context, selectorKey []byte, orEqual bool, offset int32) ([]byte, error) {
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, err
	}
	if !isSystemKey(selectorKey) {
		tx.addReadConflictForKey(selectorKey)
	}
	return tx.getKey(ctx, selectorKey, orEqual, offset)
}

// isSystemKey returns true for keys with the \xff\xff prefix (FDB system key space).
func isSystemKey(key []byte) bool {
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
	// Only add read conflict if range is valid (begin <= end) and not system keys.
	// C++ client validates inverted ranges and handles \xff\xff keys internally
	// without adding resolver conflict ranges.
	if bytes.Compare(begin, end) <= 0 && !isSystemKey(begin) && !isSystemKey(end) {
		tx.addReadConflict(begin, end)
	}

	if tx.rywDisabled {
		return tx.getRange(ctx, begin, end, limit, reverse)
	}
	return tx.ryw.getRange(ctx, begin, end, limit, reverse, tx.getRange)
}

// Set writes a key-value pair.
func (tx *Transaction) Set(key, value []byte) {
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutSetValue,
		Key:   key,
		Value: value,
	})
	tx.conflictMu.Unlock()
	tx.addWriteConflictForKey(key)
	tx.ryw.set(key, value)
}

// Clear deletes a key.
func (tx *Transaction) Clear(key []byte) {
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   key,
		Value: end,
	})
	tx.conflictMu.Unlock()
	tx.addWriteConflict(key, end)
	tx.ryw.clear(key)
}

// ClearRange deletes all keys in [begin, end).
// Returns inverted_range (2005) if begin > end. Matches C++ fdb_transaction_clear_range_impl.
// Zero-width ranges (begin == end) are silently ignored, matching C++.
func (tx *Transaction) ClearRange(begin, end []byte) error {
	cmp := bytes.Compare(begin, end)
	if cmp > 0 {
		return &wire.FDBError{Code: ErrInvertedRange}
	}
	if cmp == 0 {
		return nil // C++ ignores zero-width ClearRange.
	}
	tx.conflictMu.Lock()
	tx.mutations = append(tx.mutations, Mutation{
		Type:  MutClearRange,
		Key:   begin,
		Value: end,
	})
	tx.conflictMu.Unlock()
	tx.addWriteConflict(begin, end)
	tx.ryw.clearRange(begin, end)
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
	tx.conflictMu.Unlock()
	// Atomic ops add write conflict but NOT read conflict.
	tx.addWriteConflictForKey(key)
	tx.ryw.atomic(op, key, operand)
}

// Commit sends mutations to a commit proxy.
// After successful commit, the transaction is automatically reset for reuse
// (mutations and conflict ranges cleared, read version invalidated).
// This matches the C client's behavior where fdb_transaction_set() can be
// called after commit to start building a new transaction.
func (tx *Transaction) Commit(ctx context.Context) error {
	if tx.state != txStateActive {
		return fmt.Errorf("transaction not active")
	}
	if err := tx.checkTimeout(); err != nil {
		return err
	}

	// Validate size limit bounds. C++: valid range is [32, 10_000_000].
	// Out-of-range values return invalid_option_value (2006) at commit time.
	if tx.sizeLimit > 0 && (tx.sizeLimit < 32 || tx.sizeLimit > 10_000_000) {
		tx.state = txStateErrored
		return &wire.FDBError{Code: 2006} // invalid_option_value
	}

	// Enforce size limit if set. Matches C++ FDB_TR_OPTION_SIZE_LIMIT.
	if tx.sizeLimit > 0 && tx.GetApproximateSize() > tx.sizeLimit {
		tx.state = txStateErrored
		return &wire.FDBError{Code: 2101} // transaction_too_large
	}

	if len(tx.mutations) == 0 && len(tx.writeConflicts) == 0 {
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

	if err := tx.commit(ctx); err != nil {
		return err
	}

	// Feed committed version to GRV cache so subsequent reads see this write.
	if tx.committedVersion > 0 {
		tx.db.grvCache.update(time.Now(), tx.committedVersion)
	}

	tx.hasCommitted = true

	// Auto-reset for reuse — clear mutations and conflicts but preserve
	// committedVersion/txnBatchId for GetCommittedVersion/GetVersionstamp.
	tx.postCommitReset()
	return nil
}

// Cancel cancels the transaction. All subsequent operations will return an error.
// This is irreversible — a cancelled transaction cannot be reused.
func (tx *Transaction) Cancel() {
	tx.state = txStateCancelled
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

// OnError handles a transaction error. Returns nil if the error is retryable
// (the transaction has been reset for retry). Returns the error if non-retryable.
func (tx *Transaction) OnError(err error) error {
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		tx.state = txStateErrored
		return err
	}

	// Transaction timeout is NEVER retryable — matches C++ behavior where
	// OnError(1031) returns 1031 and the error escapes the retry loop.
	if fdbErr.Code == ErrTransactionTimedOut {
		tx.state = txStateErrored
		return err
	}

	// Check retry limit before allowing any retry.
	if tx.hasRetryLimit && tx.retryCount >= tx.retryLimit {
		tx.state = txStateErrored
		return err
	}

	switch fdbErr.Code {
	case ErrTransactionTooOld, ErrFutureVersion:
		// Version-related: fixed delay, no backoff growth.
		// C++ NativeAPI.actor.cpp: FUTURE_VERSION_RETRY_DELAY.
		tx.retryCount++
		time.Sleep(futureVersionDelay)
		tx.reset()
		return nil

	case ErrNotCommitted, ErrDatabaseLocked, ErrProxyMemoryLimitExceeded,
		ErrGrvProxyMemoryLimit, ErrProcessBehind, ErrBatchTransactionThrottled,
		ErrTagThrottled, ErrProxyTagThrottled, ErrThrottledHotShard,
		ErrRangeLocked, ErrAllProxiesUnreachable:
		// RETRYABLE_NOT_COMMITTED: exponential backoff.
		// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE_NOT_COMMITTED, code).
		tx.retryCount++
		time.Sleep(tx.nextBackoff())
		tx.reset()
		return nil

	case ErrCommitUnknownResult, ErrClusterVersionChanged:
		// MAYBE_COMMITTED: self-conflicting — copy write conflicts to read
		// conflicts so the retry detects if our prior commit actually landed.
		// C++ fdb_error_predicate(FDB_ERROR_PREDICATE_MAYBE_COMMITTED, code).
		selfConflicts := make([]KeyRange, len(tx.writeConflicts))
		copy(selfConflicts, tx.writeConflicts)
		tx.retryCount++
		time.Sleep(tx.nextBackoff())
		tx.reset()
		tx.readConflicts = append(tx.readConflicts, selfConflicts...)
		return nil

	default:
		tx.state = txStateErrored
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
	tx.readVersion = version
	tx.hasReadVersion = true
}

// SetTimeout sets a timeout in milliseconds for this transaction.
// If the transaction does not complete within this duration (from creation
// or last reset), operations return ErrTransactionTimedOut (1031).
// A value of 0 disables the timeout. Matches C++ FDB_TR_OPTION_TIMEOUT.
func (tx *Transaction) SetTimeout(ms int64) {
	if ms <= 0 {
		tx.timeout = 0
		tx.deadline = time.Time{}
		return
	}
	tx.timeout = time.Duration(ms) * time.Millisecond
	tx.deadline = time.Now().Add(tx.timeout)
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

// GetApproximateSize returns the approximate size of the transaction's mutations
// and conflict ranges in bytes. Note: does not include per-mutation framing
// overhead (~40 bytes/mutation in C++), so slightly underestimates near the
// SetSizeLimit threshold. A transaction passing this check could still be
// rejected server-side.
func (tx *Transaction) GetApproximateSize() int64 {
	var size int64
	for _, m := range tx.mutations {
		size += int64(len(m.Key)) + int64(len(m.Value))
	}
	for _, r := range tx.readConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End))
	}
	for _, r := range tx.writeConflicts {
		size += int64(len(r.Begin)) + int64(len(r.End))
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

// keyAfterBytes returns a copy of key with \x00 appended.
// Always allocates — safe for storing in conflict ranges.
func keyAfterBytes(key []byte) []byte {
	r := make([]byte, len(key)+1)
	copy(r, key)
	return r
}

// addReadConflictForKey adds a read conflict range [key, key\x00) with a
// single allocation for both begin and end slices. Saves 2 allocs vs
// keyAfterBytes + addReadConflict which allocate 3 buffers.
func (tx *Transaction) addReadConflictForKey(key []byte) {
	// One buffer: [begin(len)][end(len+1)] — end has implicit \x00 from make.
	n := len(key)
	buf := make([]byte, n+n+1)
	copy(buf, key)
	copy(buf[n:], key)
	// buf[2*n] is already 0 from make
	tx.conflictMu.Lock()
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: buf[:n], End: buf[n : n+n+1]})
	tx.conflictMu.Unlock()
}

// addReadConflict adds a read conflict range with defensive copies.
func (tx *Transaction) addReadConflict(begin, end []byte) {
	b := make([]byte, len(begin))
	copy(b, begin)
	e := make([]byte, len(end))
	copy(e, end)
	tx.conflictMu.Lock()
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: b, End: e})
	tx.conflictMu.Unlock()
}

// addWriteConflictForKey adds a write conflict range [key, key\x00) with a
// single allocation. Same optimization as addReadConflictForKey.
func (tx *Transaction) addWriteConflictForKey(key []byte) {
	if tx.nextWriteNoConflict {
		tx.nextWriteNoConflict = false
		return
	}
	n := len(key)
	buf := make([]byte, n+n+1)
	copy(buf, key)
	copy(buf[n:], key)
	tx.conflictMu.Lock()
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: buf[:n], End: buf[n : n+n+1]})
	tx.conflictMu.Unlock()
}

func (tx *Transaction) addWriteConflict(begin, end []byte) {
	if tx.nextWriteNoConflict {
		tx.nextWriteNoConflict = false
		return
	}
	b := make([]byte, len(begin))
	copy(b, begin)
	e := make([]byte, len(end))
	copy(e, end)
	tx.conflictMu.Lock()
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: b, End: e})
	tx.conflictMu.Unlock()
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

// SetSizeLimit sets the maximum transaction size in bytes.
// If the transaction exceeds this size, commit returns error 2101.
// Valid range: [32, 10_000_000]. Out-of-range values cause error 2006 at commit.
// A value of 0 disables the limit.
func (tx *Transaction) SetSizeLimit(limit int64) {
	tx.sizeLimit = limit
}

// SetReadYourWritesDisable disables RYW for regular (non-snapshot) reads.
// When set, Get/GetRange always read from the server, ignoring uncommitted writes.
// Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
func (tx *Transaction) SetReadYourWritesDisable() {
	tx.rywDisabled = true
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
	tx.state = txStateActive
	tx.hasReadVersion = false
	tx.readVersion = 0
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
	tx.ryw.reset()
	// committedVersion and txnBatchId preserved intentionally.
}

func (tx *Transaction) reset() {
	tx.state = txStateActive
	tx.hasReadVersion = false
	tx.readVersion = 0
	tx.committedVersion = 0
	tx.hasCommitted = false
	tx.txnBatchId = 0
	tx.mutations = tx.mutations[:0]
	tx.readConflicts = tx.readConflicts[:0]
	tx.writeConflicts = tx.writeConflicts[:0]
	tx.ryw.reset()
	// Re-compute deadline from timeout (matches C++ option re-application).
	if tx.timeout > 0 {
		tx.deadline = time.Now().Add(tx.timeout)
	}
	// Preserved across reset (match C++ option re-application on retry):
	// retryCount, backoff, timeout, retryLimit, priority, causalReadRisky,
	// lockAware, readLockAware, sizeLimit, rywDisabled, snapshotRYWDisabled, tenantId.
}

// nextBackoff returns the current backoff duration with jitter, then grows
// the backoff for the next call. Matches C++ getBackoff in NativeAPI.actor.cpp.
func (tx *Transaction) nextBackoff() time.Duration {
	if tx.backoff == 0 {
		tx.backoff = defaultBackoff
	}
	// C++ pattern: return current * jitter, then grow for next time.
	delay := time.Duration(float64(tx.backoff) * rand.Float64())
	tx.backoff = time.Duration(math.Min(float64(tx.backoff)*backoffGrowthRate, float64(maxBackoff)))
	return delay
}
