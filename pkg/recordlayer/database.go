package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// WeakReadSemantics configures relaxed read consistency for a transaction.
// When provided, the transaction may read at a slightly stale version,
// reducing GRV latency at the cost of freshness.
// Matches Java's FDBDatabase.WeakReadSemantics.
type WeakReadSemantics struct {
	// MinVersion is the minimum read version acceptable. If the cached read
	// version is >= MinVersion, it will be reused without a GRV round-trip.
	MinVersion int64

	// StalenessBoundMillis is the maximum staleness (in ms) of a cached read
	// version. If the cached version is older than this, a fresh GRV is fetched.
	StalenessBoundMillis int64

	// IsCausalReadRisky sets the FDB_TR_OPTION_CAUSAL_READ_RISKY flag.
	// This allows the transaction to read from any storage replica, not
	// just the one with the latest committed data.
	IsCausalReadRisky bool
}

// FDBDatabase provides access to the underlying FoundationDB database or tenant
// and manages transaction execution with retry logic.
// This is the Record Layer equivalent of Java's FDBDatabase.
//
// The transactor field can be either an fdb.Database or fdb.Tenant, both of which
// implement the fdb.Transactor interface. This allows transparent support for both
// regular database operations and tenant-isolated operations.
type FDBDatabase struct {
	transactor fdb.Transactor
	// Keep original db/tenant for CreateTransaction which isn't on Transactor interface
	db     fdb.Database
	tenant fdb.Tenant

	// storeStateCache caches store state across transactions.
	// Default: PassThroughRecordStoreStateCache (no caching).
	// Matches Java's FDBDatabase.storeStateCache field.
	storeStateCache FDBRecordStoreStateCache
}

// FDBDatabaseFactory caches FDBDatabase instances by cluster file path.
// Thread-safe. Matches Java's FDBDatabaseFactory.getDatabase(clusterFile).
type FDBDatabaseFactory struct {
	mu        sync.Mutex
	databases map[string]*FDBDatabase

	// StoreStateCacheFactory creates a store state cache for each new database.
	// If nil, PassThroughStoreStateCache is used.
	StoreStateCacheFactory func() FDBRecordStoreStateCache
}

// NewFDBDatabaseFactory creates a factory for caching database instances.
func NewFDBDatabaseFactory() *FDBDatabaseFactory {
	return &FDBDatabaseFactory{
		databases: make(map[string]*FDBDatabase),
	}
}

// GetDatabase returns a cached FDBDatabase for the given cluster file path.
// Creates a new one on first call for each unique path.
// Matches Java's FDBDatabaseFactory.getDatabase(clusterFile).
func (f *FDBDatabaseFactory) GetDatabase(clusterFile string) (*FDBDatabase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if db, ok := f.databases[clusterFile]; ok {
		return db, nil
	}

	rawDB, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		return nil, fmt.Errorf("open database %q: %w", clusterFile, err)
	}

	db := NewFDBDatabase(rawDB)
	if f.StoreStateCacheFactory != nil {
		db.SetStoreStateCache(f.StoreStateCacheFactory())
	}
	f.databases[clusterFile] = db
	return db, nil
}

// NewFDBDatabase creates a new FDBDatabase wrapping the core FDB database
func NewFDBDatabase(db fdb.Database) *FDBDatabase {
	// Record layer reads \xff/metadataVersion — set ReadSystemKeys as default
	// so ALL transactions (including test helpers that bypass Run()) get it.
	db.Options().SetReadSystemKeys()
	return &FDBDatabase{
		transactor:      db,
		db:              db,
		storeStateCache: PassThroughStoreStateCache(),
	}
}

// NewFDBDatabaseWithTransactor creates a new FDBDatabase with a custom Transactor.
// The transactor is used for Run()/RunWithVersionstamp() (transaction execution),
// while the db is used for CreateTransaction() (direct transaction creation).
// This enables wrapping the transactor for fault injection, tracing, or middleware.
func NewFDBDatabaseWithTransactor(transactor fdb.Transactor, db fdb.Database) *FDBDatabase {
	return &FDBDatabase{
		transactor:      transactor,
		db:              db,
		storeStateCache: PassThroughStoreStateCache(),
	}
}

// NewFDBDatabaseWithBackend creates an FDBDatabase driven by a config-selected
// fdb backend (RFC-109) — e.g. the libfdb_c escape hatch opened via
// fdb.OpenDatabaseWithBackend. The backend drives the Run / RunRead gold path
// (record save/load, query, index maintenance) through the Transactor interface.
//
// The concrete-db slot is left empty on purpose: CreateTransaction, the manual
// FDBDatabaseRunner, and LocalityGetBoundaryKeys (online mutual indexing) return
// concrete pure-Go handles a non-pure-Go backend cannot build, so they are
// pure-Go-only in v1 and return errBackendNoDirectTx here (fail-fast, not a nil
// panic) — the same scope boundary the RFC draws around tenants.
func NewFDBDatabaseWithBackend(backend fdb.BackendDatabase) *FDBDatabase {
	return &FDBDatabase{
		transactor:      backend,
		storeStateCache: PassThroughStoreStateCache(),
	}
}

// NewFDBDatabaseFromTenant creates a new FDBDatabase wrapping an FDB tenant
// for tenant-isolated operations. All operations will be scoped to the tenant's keyspace.
func NewFDBDatabaseFromTenant(tenant fdb.Tenant) *FDBDatabase {
	return &FDBDatabase{
		transactor:      tenant,
		tenant:          tenant,
		storeStateCache: PassThroughStoreStateCache(),
	}
}

// SetStoreStateCache sets the cache used for store state across transactions.
// Matches Java's FDBDatabase.setStoreStateCache().
func (d *FDBDatabase) SetStoreStateCache(cache FDBRecordStoreStateCache) {
	d.storeStateCache = cache
}

// GetStoreStateCache returns the current store state cache.
// Matches Java's FDBDatabase.getStoreStateCache().
func (d *FDBDatabase) GetStoreStateCache() FDBRecordStoreStateCache {
	return d.storeStateCache
}

// Run executes a function within a transaction with automatic retry handling.
// Before committing, flushes any queued versionstamp mutations.
// Matches Java's FDBRecordContext.commitAsync() behavior.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastCtx *FDBRecordContext
	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		tx.Options().SetReadSystemKeys()
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
		}
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// Run pre-commit checks before flushing
		if err := recordCtx.runCommitChecks(); err != nil {
			return nil, err
		}

		// Flush queued version mutations before FDB's Transact commits.
		recordCtx.flushVersionMutations()

		return result, nil
	})
	if err != nil {
		return result, err
	}

	// Run post-commit callbacks after successful commit
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}
	return result, nil
}

// runTransactCtx threads ctx into the transactor's retry loop + backoff + reads when
// the transactor supports it (Database/Tenant via fdb.CtxTransactor), else falls back
// to the ctx-less Transact (RFC-090). The dispatched commit + commit_unknown barrier
// run detached regardless, so ctx never cancels an in-flight commit.
func runTransactCtx(t fdb.Transactor, ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ct, ok := t.(fdb.CtxTransactor); ok {
		return ct.TransactCtx(ctx, fn)
	}
	return t.Transact(fn)
}

// runReadTransactCtx is the read-side analog of runTransactCtx.
func runReadTransactCtx(t fdb.ReadTransactor, ctx context.Context, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if ct, ok := t.(fdb.CtxReadTransactor); ok {
		return ct.ReadTransactCtx(ctx, fn)
	}
	return t.ReadTransact(fn)
}

// RunRead executes a read-only function with automatic retry but no commit.
// Uses ReadTransact under the hood — no write conflict ranges, no commit
// round-trip. Suitable for statistics, metadata reads, or any read-only
// operation where a full read-write transaction would waste a commit.
//
// ctx bounds the read-retry loop + backoff when the transactor supports it
// (fdb.CtxReadTransactor); the entry check also returns early if already cancelled.
func (d *FDBDatabase) RunRead(ctx context.Context, fn func(rtx fdb.ReadTransaction) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return runReadTransactCtx(d.transactor, ctx, func(rtx fdb.ReadTransaction) (any, error) {
		rtx.Options().SetReadSystemKeys()
		return fn(rtx)
	})
}

// RunWithWeakReads is like Run but applies weak read semantics to the transaction.
// If IsCausalReadRisky is set, the transaction reads from any replica.
// Matches Java's FDBDatabase.openContext(config, timer, weakReadSemantics, ...).
func (d *FDBDatabase) RunWithWeakReads(ctx context.Context, weak WeakReadSemantics, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastCtx *FDBRecordContext
	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		tx.Options().SetReadSystemKeys()
		if weak.IsCausalReadRisky {
			tx.Options().SetCausalReadRisky()
		}
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
		}
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		if err := recordCtx.runCommitChecks(); err != nil {
			return nil, err
		}
		recordCtx.flushVersionMutations()
		return result, nil
	})
	if err != nil {
		return result, err
	}
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}
	return result, nil
}

// RunWithVersionstamp is like Run but also returns the committed versionstamp.
// Use this when you need the versionstamp after commit (e.g. for record versioning).
// Returns (result, versionstamp, error). Versionstamp is nil for read-only transactions.
func (d *FDBDatabase) RunWithVersionstamp(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, []byte, error) {
	var vsFuture fdb.FutureKey
	var hasVersionMutations bool
	var lastCtx *FDBRecordContext

	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		// Reset on retry — previous attempt's future is stale
		vsFuture = nil
		hasVersionMutations = false

		tx.Options().SetReadSystemKeys()
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
		}
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// Run pre-commit checks
		if err := recordCtx.runCommitChecks(); err != nil {
			return nil, err
		}

		recordCtx.flushVersionMutations()

		hasVersionMutations = recordCtx.HasVersionMutations()
		if hasVersionMutations {
			vsFuture = tx.GetVersionstamp()
		}

		return result, nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Run post-commit callbacks after successful commit
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}

	if hasVersionMutations && vsFuture != nil {
		vs, err := vsFuture.Get()
		if err != nil {
			return result, nil, fmt.Errorf("failed to get versionstamp: %w", err)
		}
		return result, []byte(vs), nil
	}

	return result, nil, nil
}

// BackendCapabilityError is returned when an operation is not supported on the
// configured fdb backend. The libfdb_c escape hatch (RFC-109) drives the
// Run / RunRead gold path through the Transactor interface, but the direct
// (non-retry) CreateTransaction path, the manual FDBDatabaseRunner, and
// LocalityGetBoundaryKeys hand back concrete pure-Go handles a non-pure-Go
// backend cannot build — those are pure-Go-only in v1.
type BackendCapabilityError struct {
	Op string // the unavailable operation, e.g. "CreateTransaction"
}

func (e *BackendCapabilityError) Error() string {
	return fmt.Sprintf("recordlayer: %s is not supported on this fdb backend "+
		"(pure-Go-only; the libfdb_c escape hatch covers the Run/RunRead path)", e.Op)
}

// CreateTransaction creates a new transaction without retry logic.
// This is primarily used for testing scenarios where manual transaction control is needed,
// such as testing isolation levels with concurrent transactions.
// For tenant-isolated databases, the transaction will be scoped to the tenant's keyspace.
func (d *FDBDatabase) CreateTransaction() (fdb.Transaction, error) {
	if d.tenant != (fdb.Tenant{}) {
		return d.tenant.CreateTransaction()
	}
	if !d.db.IsValid() {
		return fdb.Transaction{}, &BackendCapabilityError{Op: "CreateTransaction"}
	}
	return d.db.CreateTransaction()
}

// TransactionPriority controls the priority of FDB transactions.
// Matches Java's FDBTransactionPriority.
type TransactionPriority int

const (
	// PriorityDefault is the default transaction priority.
	PriorityDefault TransactionPriority = iota
	// PriorityBatch is a lower priority for background/batch operations.
	PriorityBatch
	// PrioritySystemImmediate is the highest priority, bypasses throttling.
	// Use with extreme care — only for system-level operations.
	PrioritySystemImmediate
)

// CommitCheckFunc is a pre-commit check that runs before the transaction commits.
// If it returns an error, the commit is aborted.
// Matches Java's CommitCheckAsync interface.
type CommitCheckFunc func() error

// PostCommitFunc is a callback that runs after a successful commit.
// Matches Java's PostCommit interface.
type PostCommitFunc func()

// VersionMutationType represents the type of versionstamp mutation.
// Matches Java's MutationType used in FDBRecordContext.addVersionMutation().
type VersionMutationType int

const (
	// MutationTypeSetVersionstampedValue queues a SET_VERSIONSTAMPED_VALUE mutation.
	MutationTypeSetVersionstampedValue VersionMutationType = iota
	// MutationTypeSetVersionstampedKey queues a SET_VERSIONSTAMPED_KEY mutation.
	MutationTypeSetVersionstampedKey
)

// versionMutation holds a queued versionstamp mutation with its type and value.
type versionMutation struct {
	mutationType VersionMutationType
	value        []byte
}

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
// Goroutine-safe: all mutable fields are protected by atomics, mutexes, or the
// lockRegistry. Multiple goroutines may safely operate on the same context
// within a single FDB transaction (matching Java's CompletableFuture model).
// Matches Java's FDBRecordContext.
// nextTransactionID generates unique IDs for FDBRecordContext instances.
var nextTransactionID atomic.Int64

type FDBRecordContext struct {
	tx            fdb.WritableTransaction
	ctx           context.Context
	transactionID int64 // unique ID for logging/tracing

	// Client-side transaction size thresholds. Zero = disabled.
	txSizeWarnBytes  int64
	txSizeErrorBytes int64
	txSizeWarned     atomic.Bool // only warn once per transaction

	// Version management — matches Java's FDBRecordContext.
	// Java uses AtomicInteger + ConcurrentSkipListMap.
	// Go uses atomic.Int32 + mutex-protected maps.
	localVersion      atomic.Int32               // per-transaction local version counter
	versionMu         sync.Mutex                 // protects localVersionCache + versionMutations
	localVersionCache map[string]int             // key (string) → local version (int)
	versionMutations  map[string]versionMutation // key (string) → mutation (type + value)

	// Commit hooks — matches Java's CommitCheckAsync / PostCommit.
	// Java uses synchronized blocks on all access.
	commitMu     sync.Mutex
	commitChecks []CommitCheckFunc
	postCommits  []PostCommitFunc

	// Diagnostic: tracked read conflict ranges for debugging
	conflictMu     sync.Mutex
	conflictRanges []fdb.KeyRange

	// Store state cache invalidation tracking — matches Java's FDBRecordContext.
	// Java leaves these unprotected (benign single-word writes).
	// Go uses atomics for race-detector cleanliness.
	dirtyStoreState           atomic.Bool // set when any store header or index state is modified
	dirtyMetaDataVersionStamp atomic.Bool // set when SetMetaDataVersionStamp() is called

	// Per-subspace read-write locks for tree-structured indexes (HNSW, R-tree).
	// Matches Java's LockRegistry on FDBRecordContext.
	locks lockRegistry

	// Instrumentation timer — matches Java's FDBRecordContext.timer field.
	// Set once during construction, nil means no instrumentation.
	// Atomic for race-detector cleanliness (concurrent reads from goroutines).
	timer atomic.Pointer[StoreTimer]

	// Session storage — matches Java's FDBRecordContext session
	// (getSession/putSessionIfAbsent): transaction-scoped values shared by
	// index maintainers across store instances opened in the same context.
	// SPFresh keeps its tx-local routing cache here so a same-transaction
	// write-then-search pairs up even when the statements open separate
	// stores.
	sessionMu sync.Mutex
	session   map[string]any
}

// Session returns the transaction-scoped value stored under key, or nil.
// Matches Java's FDBRecordContext.getSession.
func (rc *FDBRecordContext) Session(key string) any {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	return rc.session[key]
}

// PutSession stores a transaction-scoped value under key. Matches Java's
// FDBRecordContext session storage; values die with the context.
func (rc *FDBRecordContext) PutSession(key string, value any) {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	if rc.session == nil {
		rc.session = make(map[string]any)
	}
	rc.session[key] = value
}

// NewFDBRecordContext creates a new FDBRecordContext wrapping an FDB transaction.
// This is primarily used for testing scenarios where direct transaction control is needed.
func NewFDBRecordContext(tx fdb.WritableTransaction) *FDBRecordContext {
	return &FDBRecordContext{
		tx:  tx,
		ctx: context.Background(),
	}
}

// Transaction returns the underlying FDB transaction
func (rc *FDBRecordContext) Transaction() fdb.WritableTransaction {
	return rc.tx
}

// TransactionID returns a unique ID for this record context.
// Useful for logging and tracing. Matches Java's FDBRecordContext transaction ID.
func (rc *FDBRecordContext) TransactionID() int64 {
	return rc.transactionID
}

// Context returns the Go context
func (rc *FDBRecordContext) Context() context.Context {
	return rc.ctx
}

// GetApproximateTransactionSize returns the approximate size in bytes of the
// transaction's mutations so far. Useful for monitoring proximity to FDB's
// 10MB transaction size limit.
// Matches Java's FDBRecordContext.getApproximateTransactionSize().
func (rc *FDBRecordContext) GetApproximateTransactionSize() (int64, error) {
	return rc.tx.GetApproximateSize().Get()
}

// CheckTransactionSize checks the approximate transaction size against the
// configured warning and error thresholds. Returns TransactionSizeExceededError
// if the error threshold is exceeded, TransactionSizeWarningError if the warning
// threshold is exceeded (once per transaction), or nil.
func (rc *FDBRecordContext) CheckTransactionSize() error {
	if rc.txSizeWarnBytes == 0 && rc.txSizeErrorBytes == 0 {
		return nil
	}
	size, err := rc.GetApproximateTransactionSize()
	if err != nil {
		return err
	}
	if rc.txSizeErrorBytes > 0 && size >= rc.txSizeErrorBytes {
		return &TransactionSizeExceededError{CurrentBytes: size, LimitBytes: rc.txSizeErrorBytes}
	}
	if rc.txSizeWarnBytes > 0 && size >= rc.txSizeWarnBytes && rc.txSizeWarned.CompareAndSwap(false, true) {
		return &TransactionSizeWarningError{CurrentBytes: size, LimitBytes: rc.txSizeWarnBytes}
	}
	return nil
}

// TransactionSizeExceededError is returned when the approximate transaction
// size exceeds the configured error threshold. Callers should commit the
// current transaction and start a new one.
type TransactionSizeExceededError struct {
	CurrentBytes int64
	LimitBytes   int64
}

func (e *TransactionSizeExceededError) Error() string {
	return fmt.Sprintf("transaction size %d bytes exceeds limit %d bytes", e.CurrentBytes, e.LimitBytes)
}

// TransactionSizeWarningError is returned once per transaction when the
// approximate size exceeds the configured warning threshold.
type TransactionSizeWarningError struct {
	CurrentBytes int64
	LimitBytes   int64
}

func (e *TransactionSizeWarningError) Error() string {
	return fmt.Sprintf("transaction size %d bytes exceeds warning threshold %d bytes", e.CurrentBytes, e.LimitBytes)
}

// Commit commits the transaction
func (rc *FDBRecordContext) Commit() error {
	return rc.tx.Commit().Get()
}

// CommitWithHooks runs pre-commit checks, flushes pending version mutations,
// commits the FDB transaction, and runs post-commit callbacks.
// Use this instead of Commit() when the context was created manually (not via Run()).
func (rc *FDBRecordContext) CommitWithHooks() error {
	if err := rc.runCommitChecks(); err != nil {
		return err
	}
	rc.flushVersionMutations()
	if err := rc.tx.Commit().Get(); err != nil {
		return err
	}
	rc.runPostCommits()
	return nil
}

// Cancel cancels the transaction
func (rc *FDBRecordContext) Cancel() {
	rc.tx.Cancel()
}

// ClaimLocalVersion atomically claims the next local version number.
// Goroutine-safe via atomic.Int32 (matches Java's AtomicInteger.getAndIncrement()).
func (rc *FDBRecordContext) ClaimLocalVersion() int {
	return int(rc.localVersion.Add(1) - 1) // returns 0, 1, 2, ...
}

// AddToLocalVersionCache caches a local version for a version key within this transaction.
// Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.addToLocalVersionCache().
func (rc *FDBRecordContext) AddToLocalVersionCache(versionKey []byte, localVersion int) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.localVersionCache == nil {
		rc.localVersionCache = make(map[string]int)
	}
	rc.localVersionCache[string(versionKey)] = localVersion
}

// GetLocalVersion retrieves a cached local version for the given key.
// Returns (localVersion, true) if found, (0, false) otherwise.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) GetLocalVersion(versionKey []byte) (int, bool) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	v, ok := rc.localVersionCache[string(versionKey)]
	return v, ok
}

// RemoveLocalVersion removes a cached local version entry.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) RemoveLocalVersion(versionKey []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	delete(rc.localVersionCache, string(versionKey))
}

// AddVersionMutation queues a versionstamp mutation to be applied at commit.
// mutationType selects SET_VERSIONSTAMPED_KEY or SET_VERSIONSTAMPED_VALUE.
// The key or value (depending on type) must include the versionstamp placeholder bytes.
// Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.addVersionMutation(MutationType, key, value).
func (rc *FDBRecordContext) AddVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	rc.versionMutations[string(versionKey)] = versionMutation{
		mutationType: mutationType,
		value:        value,
	}
}

// UpdateVersionMutation queues or updates a versionstamp mutation with a merge function.
// If a mutation for the same key already exists, the merge function decides which value to keep.
// Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.updateVersionMutation(MutationType, key, value, BiFunction).
func (rc *FDBRecordContext) UpdateVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte, merge func(oldValue, newValue []byte) []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	key := string(versionKey)
	if existing, ok := rc.versionMutations[key]; ok && merge != nil {
		merged := merge(existing.value, value)
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        merged,
		}
	} else {
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        value,
		}
	}
}

// RemoveVersionMutation removes a queued version mutation.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) RemoveVersionMutation(versionKey []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	delete(rc.versionMutations, string(versionKey))
}

// RemoveVersionMutationsInRange removes all queued version mutations whose key
// falls in [begin, end). Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.removeVersionMutationRange().
func (rc *FDBRecordContext) RemoveVersionMutationsInRange(begin, end fdb.Key) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for k := range rc.versionMutations {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.versionMutations, k)
		}
	}
}

// RemoveLocalVersionsInRange removes all cached local versions whose key
// falls in [begin, end). Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.removeLocalVersionRange().
func (rc *FDBRecordContext) RemoveLocalVersionsInRange(begin, end fdb.Key) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for k := range rc.localVersionCache {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.localVersionCache, k)
		}
	}
}

// flushVersionMutations applies all queued versionstamp mutations
// to the underlying FDB transaction. Called before commit.
// Dispatches to SetVersionstampedKey or SetVersionstampedValue based on mutation type.
// Goroutine-safe via versionMu (though typically called after all goroutines are done).
func (rc *FDBRecordContext) flushVersionMutations() {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for key, mut := range rc.versionMutations {
		switch mut.mutationType {
		case MutationTypeSetVersionstampedKey:
			rc.tx.SetVersionstampedKey(fdb.Key(key), mut.value)
		case MutationTypeSetVersionstampedValue:
			rc.tx.SetVersionstampedValue(fdb.Key(key), mut.value)
		}
	}
}

// AddCommitCheck registers a pre-commit check function.
// All checks run before the transaction commits. If any returns an error,
// the commit is aborted with that error.
// Goroutine-safe via commitMu (matches Java's synchronized blocks).
// Matches Java's FDBRecordContext.addCommitCheck(CommitCheckAsync).
func (rc *FDBRecordContext) AddCommitCheck(check CommitCheckFunc) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	rc.commitChecks = append(rc.commitChecks, check)
}

// AddPostCommit registers a post-commit callback.
// All callbacks run after the transaction successfully commits.
// Goroutine-safe via commitMu (matches Java's synchronized blocks).
// Matches Java's FDBRecordContext.addPostCommit(PostCommit).
func (rc *FDBRecordContext) AddPostCommit(hook PostCommitFunc) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	rc.postCommits = append(rc.postCommits, hook)
}

// runCommitChecks runs all registered pre-commit checks.
// Returns the first error encountered, or nil if all pass.
// Called at commit time when all goroutines should be done; holds commitMu
// for race-detector cleanliness.
func (rc *FDBRecordContext) runCommitChecks() error {
	rc.commitMu.Lock()
	checks := make([]CommitCheckFunc, len(rc.commitChecks))
	copy(checks, rc.commitChecks)
	rc.commitMu.Unlock()
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// runPostCommits runs all registered post-commit callbacks.
// Called after commit when all goroutines should be done; holds commitMu
// for race-detector cleanliness.
func (rc *FDBRecordContext) runPostCommits() {
	rc.commitMu.Lock()
	hooks := make([]PostCommitFunc, len(rc.postCommits))
	copy(hooks, rc.postCommits)
	rc.commitMu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// GetReadVersion returns the transaction's read version.
// Matches Java's FDBRecordContext.getReadVersion().
func (rc *FDBRecordContext) GetReadVersion() (int64, error) {
	startTime := time.Now()
	v, err := rc.tx.GetReadVersion().Get()
	rc.Timer().RecordSince(EventGetReadVersion, startTime)
	return v, err
}

// SetReadVersion sets the transaction's read version explicitly.
// Matches Java's FDBRecordContext.setReadVersion().
func (rc *FDBRecordContext) SetReadVersion(version int64) {
	rc.tx.SetReadVersion(version)
}

// SetTransactionPriority sets the priority for this transaction.
// Matches Java's FDBRecordContext applying FDBTransactionPriority.
func (rc *FDBRecordContext) SetTransactionPriority(priority TransactionPriority) error {
	switch priority {
	case PriorityBatch:
		return rc.tx.Options().SetPriorityBatch()
	case PrioritySystemImmediate:
		return rc.tx.Options().SetPrioritySystemImmediate()
	default:
		return nil // Default priority, no option needed
	}
}

// GetConflictingKeys attempts to identify conflicting keys after a commit failure.
// Reads the transaction's read conflict ranges. This is a best-effort diagnostic tool.
// Goroutine-safe via conflictMu.
// Matches Java's FDBRecordContext.reportConflictingKeys() (diagnostic, not exact).
func (rc *FDBRecordContext) GetConflictingKeys() []fdb.KeyRange {
	rc.conflictMu.Lock()
	defer rc.conflictMu.Unlock()
	result := make([]fdb.KeyRange, len(rc.conflictRanges))
	copy(result, rc.conflictRanges)
	return result
}

// AddReadConflictRange adds a read conflict range and tracks it for diagnostics.
// Goroutine-safe via conflictMu (FDB transaction is already goroutine-safe).
func (rc *FDBRecordContext) AddReadConflictRange(r fdb.ExactRange) error {
	if err := rc.tx.AddReadConflictRange(r); err != nil {
		return err
	}
	begin, end := r.FDBRangeKeys()
	rc.conflictMu.Lock()
	rc.conflictRanges = append(rc.conflictRanges, fdb.KeyRange{
		Begin: begin.FDBKey(),
		End:   end.FDBKey(),
	})
	rc.conflictMu.Unlock()
	return nil
}

// HasVersionMutations returns true if there are pending version mutations.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) HasVersionMutations() bool {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	return len(rc.versionMutations) > 0
}

// Timer returns the instrumentation timer for this context, or nil if not set.
// Goroutine-safe via atomic.Pointer.
// Matches Java's FDBRecordContext.getTimer().
func (rc *FDBRecordContext) Timer() *StoreTimer {
	return rc.timer.Load()
}

// SetTimer sets the instrumentation timer for this context.
// Goroutine-safe via atomic.Pointer.
// Matches Java's FDBRecordContext.setTimer().
func (rc *FDBRecordContext) SetTimer(timer *StoreTimer) {
	rc.timer.Store(timer)
}

// HasDirtyStoreState returns true if any store state was modified in this transaction.
// When true, cached store state should not be used.
// Goroutine-safe via atomic.Bool.
// Matches Java's FDBRecordContext.hasDirtyStoreState().
func (rc *FDBRecordContext) HasDirtyStoreState() bool {
	return rc.dirtyStoreState.Load()
}

// SetDirtyStoreState marks that store state was modified in this transaction.
// Goroutine-safe via atomic.Bool.
// Matches Java's FDBRecordContext.setDirtyStoreState().
func (rc *FDBRecordContext) SetDirtyStoreState(dirty bool) {
	rc.dirtyStoreState.Store(dirty)
}

// metaDataVersionStampValue is 14 zero bytes: 10 bytes for the global versionstamp
// + 4 bytes for the little-endian offset (0). FDB replaces the first 10 bytes with
// the commit versionstamp when SET_VERSIONSTAMPED_VALUE is used.
// Matches Java's FDBRecordContext.META_DATA_VERSION_STAMP_VALUE.
// Using an array (not slice) to prevent accidental mutation.
var metaDataVersionStampValue [14]byte

// metaDataVersionKey is the FDB system key used to track metadata version changes.
// Matches Java's SystemKeyspace.METADATA_VERSION_KEY = \xff/metadataVersion.
var metaDataVersionKey = append([]byte{0xFF}, []byte("/metadataVersion")...)

// SetMetaDataVersionStamp schedules a SET_VERSIONSTAMPED_VALUE mutation on the
// metadata version key. After commit, this key will contain the commit versionstamp,
// which invalidates any cached store state entries with older stamps.
// Goroutine-safe via atomic.Bool + goroutine-safe FDB transaction.
// Matches Java's FDBRecordContext.setMetaDataVersionStamp().
func (rc *FDBRecordContext) SetMetaDataVersionStamp() {
	rc.dirtyMetaDataVersionStamp.Store(true)
	rc.tx.SetVersionstampedValue(fdb.Key(metaDataVersionKey), metaDataVersionStampValue[:])
}

// GetMetaDataVersionStamp reads the metadata version stamp at snapshot isolation.
// Returns nil if the stamp was written in this transaction (dirty) or doesn't exist.
// On ACCESSED_UNREADABLE errors (FDB code 1036), marks the stamp as dirty and returns nil.
// Goroutine-safe via atomic.Bool + goroutine-safe FDB transaction.
// Matches Java's FDBRecordContext.getMetaDataVersionStampAsync().
func (rc *FDBRecordContext) GetMetaDataVersionStamp() ([]byte, error) {
	if rc.dirtyMetaDataVersionStamp.Load() {
		return nil, nil
	}
	val, err := rc.tx.Snapshot().Get(fdb.Key(metaDataVersionKey)).Get()
	if err != nil {
		// Check for ACCESSED_UNREADABLE (1036) — the versionstamped value was
		// written in this transaction and can't be read back yet.
		// Matches Java's handle() which catches this specific error code.
		var fdbErr fdb.Error
		if errors.As(err, &fdbErr) && fdbErr.Code == 1036 {
			rc.dirtyMetaDataVersionStamp.Store(true)
			return nil, nil
		}
		// For genuine errors (network, transaction_too_old, etc.), propagate.
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val, nil
}

// CommitWithVersionstamp commits the transaction, first running pre-commit checks,
// then flushing all queued versionstamp mutations. Returns the committed
// versionstamp (10 bytes) or nil for read-only transactions / no versionstamp mutations.
// Runs post-commit hooks after successful commit.
// Matches Java's FDBRecordContext.commitAsync() which always runs checks and hooks.
func (rc *FDBRecordContext) CommitWithVersionstamp() ([]byte, error) {
	// Run pre-commit checks before committing
	if err := rc.runCommitChecks(); err != nil {
		return nil, err
	}

	rc.flushVersionMutations()

	// Only request versionstamp future if we actually queued versionstamp mutations.
	// Matches the pattern in RunWithVersionstamp.
	hasVersionMutations := rc.HasVersionMutations()
	var vsFuture fdb.FutureKey
	if hasVersionMutations {
		vsFuture = rc.tx.GetVersionstamp()
	}

	// Commit the transaction — timed as EventCommit
	commitStart := time.Now()
	if err := rc.tx.Commit().Get(); err != nil {
		rc.Timer().RecordSince(EventCommit, commitStart)
		return nil, err
	}
	rc.Timer().RecordSince(EventCommit, commitStart)

	// Run post-commit callbacks after successful commit
	rc.runPostCommits()

	// Retrieve the committed versionstamp only if mutations were queued.
	if hasVersionMutations && vsFuture != nil {
		vs, err := vsFuture.Get()
		if err != nil {
			return nil, fmt.Errorf("get versionstamp after commit: %w", err)
		}
		return []byte(vs), nil
	}

	// No versionstamp mutations — read-only or no versioned writes.
	return nil, nil
}

// buildVersionstampedValue builds the value for SET_VERSIONSTAMPED_VALUE mutation.
// Matches Java's SplitHelper.packVersion(): wraps an incomplete versionstamp in
// a Tuple and uses PackWithVersionstamp to produce bytes with the offset appended.
// After FDB commit, the stored value is a packed Tuple containing the completed versionstamp.
func buildVersionstampedValue(version *FDBRecordVersion) ([]byte, error) {
	vs := tuple.Versionstamp{
		UserVersion: uint16(version.GetLocalVersion()),
	}
	// TransactionVersion is all 0xFF for incomplete versionstamps (placeholder)
	copy(vs.TransactionVersion[:], incompleteGlobalVersionMarker[:])
	return tuple.Tuple{vs}.PackWithVersionstamp(nil)
}

// lockRegistry provides per-key read-write locks within a transaction context.
// Matches Java's LockRegistry (ConcurrentHashMap<LockIdentifier, AtomicReference<AsyncLock>>).
// Used by tree-structured indexes (HNSW, R-tree) to serialize mutations.
type lockRegistry struct {
	mu    sync.Mutex
	locks map[string]*sync.RWMutex
}

func (r *lockRegistry) getOrCreate(key string) *sync.RWMutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks == nil {
		r.locks = make(map[string]*sync.RWMutex)
	}
	if m, ok := r.locks[key]; ok {
		return m
	}
	m := &sync.RWMutex{}
	r.locks[key] = m
	return m
}

// WriteLock acquires an exclusive lock for the given key.
// Matches Java's FDBRecordContext.doWithWriteLock(LockIdentifier).
func (r *lockRegistry) WriteLock(key string) {
	r.getOrCreate(key).Lock()
}

// WriteUnlock releases the exclusive lock for the given key.
func (r *lockRegistry) WriteUnlock(key string) {
	r.getOrCreate(key).Unlock()
}

// ReadLock acquires a shared lock for the given key.
// Matches Java's FDBRecordContext.doWithReadLock(LockIdentifier).
func (r *lockRegistry) ReadLock(key string) {
	r.getOrCreate(key).RLock()
}

// ReadUnlock releases the shared lock for the given key.
func (r *lockRegistry) ReadUnlock(key string) {
	r.getOrCreate(key).RUnlock()
}
