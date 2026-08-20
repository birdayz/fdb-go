package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/internal/fdbclient"
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
	// isTenant is true for a tenant-backed database (NewFDBDatabaseFromTenant). The record layer
	// must NOT set READ_SYSTEM_KEYS on a tenant transaction: libfdb_c throws invalid_option there
	// (system-key access can't be tenant-scoped, NativeAPI.actor.cpp:7159-7171), and it is also
	// unnecessary — the only system key the record layer reads (\xff/metadataVersion) is exempt
	// from the system-key gate and read globally even on a tenant transaction.
	isTenant bool

	// storeStateCache caches store state across transactions.
	// Default: PassThroughRecordStoreStateCache (no caching).
	// Matches Java's FDBDatabase.storeStateCache field.
	storeStateCache FDBRecordStoreStateCache

	// env is the DST Tier-0 environment (Clock + Randomness + Buggify) inherited by every
	// FDBRecordContext this database opens. Nil means production (wall clock, crypto/rand,
	// buggify off) — the nil-safe *dst.Env accessors handle that. A simulation sets a
	// seeded env via SetEnv so persisted timestamps/nonces are reproducible (RFC-199 Tier 0).
	env *dst.Env

	// timer is the StoreTimer inherited by every FDBRecordContext this database opens,
	// and therefore the aggregation point for record-layer instrumentation across all
	// transactions in the process. Nil (the default) means uninstrumented — the
	// nil-receiver-safe StoreTimer methods make that a single nil check per site, and it
	// matches Java, where FDBRecordContextConfig's timer defaults to null
	// (FDBRecordContextConfig.java:266).
	//
	// Sharing ONE timer instance across many contexts is Java's own aggregation model,
	// not a Go shortcut: FDBDatabaseRunner.setTimer installs a timer on the shared
	// context-config builder and every context the retry loop opens writes into that same
	// instance (FDBDatabaseRunner.java:105-119). Java's other model — a fresh timer per
	// transaction folded into a process-level registry (RecordLayerTransactionManager.java:62-70
	// with MetricRegistryStoreTimer) — needs a per-commit fold and a second registry type to
	// aggregate into; sharing the instance gets the same operator-visible totals with neither.
	// StoreTimer.Add ports the fold for callers who want the first model anyway.
	//
	// atomic.Pointer because SetTimer may be called on a database other goroutines are
	// already opening contexts against.
	timer atomic.Pointer[StoreTimer]
}

// SetTimer installs the StoreTimer that every context this database opens will record
// into, replacing any previous one. Passing nil disables instrumentation.
//
// Matches Java's FDBDatabaseRunner.setTimer / FDBRecordContextConfig.Builder.setTimer
// (FDBDatabaseRunner.java:112-119, FDBRecordContextConfig.java:357-368): the timer is a
// caller-supplied object whose lifetime and scope the caller chooses, never something the
// database owns or creates for you. Contexts already open keep the timer they were built
// with — the read happens once at context construction, as in Java.
func (d *FDBDatabase) SetTimer(timer *StoreTimer) {
	d.timer.Store(timer)
}

// Timer returns the StoreTimer installed by SetTimer, or nil when the database is
// uninstrumented. Matches Java's FDBDatabaseRunner.getTimer (FDBDatabaseRunner.java:105-110).
func (d *FDBDatabase) Timer() *StoreTimer {
	if d == nil {
		return nil
	}
	return d.timer.Load()
}

// SetEnv installs the DST environment inherited by every context this database opens. A
// simulation driver calls this with dst.NewSim(seed) right after constructing the database
// (typically over a SimFDB backend); production leaves it unset. Returns d for chaining.
func (d *FDBDatabase) SetEnv(env *dst.Env) *FDBDatabase {
	d.env = env
	return d
}

// Env returns the database's DST environment, or nil (which the *dst.Env accessors treat
// as production). Nil-safe on the receiver, like FDBRecordContext.Env: an accessor whose whole
// contract is "nil means production" must not be the one call in the chain that panics.
func (d *FDBDatabase) Env() *dst.Env {
	if d == nil {
		return nil
	}
	return d.env
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

	// Open through the build-tag-selectable seam (RFC-109): the default build is
	// the pure-Go client; -tags libfdbc swaps in Apple's libfdb_c, with no source
	// change here. NewFDBDatabaseWithBackend keeps the pure-Go concrete handle when
	// the backend IS pure-Go (so CreateTransaction/locality are unaffected in the
	// default build) and fail-fasts those direct paths only under libfdb_c.
	rawDB, err := fdbclient.Open(clusterFile)
	if err != nil {
		return nil, fmt.Errorf("open database %q: %w", clusterFile, err)
	}

	db := NewFDBDatabaseWithBackend(rawDB)
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

// NewFDBDatabaseWithBackend creates an FDBDatabase driven by a build-selected fdb
// backend (RFC-109) — e.g. the libfdb_c escape hatch, opened via fdbclient.Open
// (which a build tag points at the pure-Go or libfdb_c client). The backend drives
// the Run / RunRead gold path (record save/load, query, index maintenance) through
// the Transactor interface.
//
// The concrete-db slot is left empty for a non-pure-Go backend. Standalone
// transactions go through CreateWritableTransaction and locality through
// LocalityGetBoundaryKeys — both delegate to the backend's own
// interface-returning methods, so explicit transactions, the FDBDatabaseRunner,
// AND online MUTUAL indexing all work on libfdb_c. The one remaining
// concrete-only path is the pure-Go-typed CreateTransaction (it returns the
// concrete fdb.Transaction a non-pure-Go backend cannot build), which stays
// pure-Go-only in v1 and fails fast with BackendCapabilityError (not a nil panic)
// — the same scope boundary the RFC draws around tenants.
func NewFDBDatabaseWithBackend(backend fdb.BackendDatabase) *FDBDatabase {
	d := &FDBDatabase{
		transactor:      backend,
		storeStateCache: PassThroughStoreStateCache(),
	}
	// If the selected backend is actually the pure-Go client, keep its concrete
	// handle so the direct CreateTransaction / FDBDatabaseRunner / locality paths
	// still work — only a non-pure-Go backend (libfdb_c) genuinely lacks them.
	// Without this, the pure-Go client via this constructor would needlessly cripple
	// those paths.
	if goDB, ok := backend.(fdb.Database); ok {
		// Match NewFDBDatabase: the record layer reads \xff/metadataVersion, so make
		// ReadSystemKeys the default on every transaction (including the direct
		// CreateTransaction path) — otherwise the pure-Go client via this constructor
		// would behave differently from NewFDBDatabase and fail those reads.
		goDB.Options().SetReadSystemKeys()
		d.db = goDB
	}
	return d
}

// NewFDBDatabaseFromTenant creates a new FDBDatabase wrapping an FDB tenant
// for tenant-isolated operations. All operations will be scoped to the tenant's keyspace.
func NewFDBDatabaseFromTenant(tenant fdb.Tenant) *FDBDatabase {
	return &FDBDatabase{
		transactor:      tenant,
		tenant:          tenant,
		isTenant:        true,
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
// Transactor returns the transactor this database runs through, and Database
// the raw handle beneath it. They are the read side of
// NewFDBDatabaseWithTransactor, which already takes both publicly: a caller that
// may CONSTRUCT a wrapped database can also wrap an existing one, and without
// these it cannot. Used to layer per-transaction behaviour — FDB transaction
// tags, fault injection — onto every transaction a library call opens, rather
// than threading an option through each signature that opens one.
func (d *FDBDatabase) Transactor() fdb.Transactor { return d.transactor }

// Database returns the raw FDB handle. See Transactor.
func (d *FDBDatabase) Database() fdb.Database { return d.db }

func (d *FDBDatabase) GetStoreStateCache() FDBRecordStoreStateCache {
	return d.storeStateCache
}

// applyReadSystemKeys grants the per-transaction READ_SYSTEM_KEYS the record layer uses to read
// \xff/metadataVersion. It is a NO-OP on a tenant-backed database: libfdb_c rejects READ_SYSTEM_KEYS
// on a tenant transaction (invalid_option, NativeAPI.actor.cpp:7159-7171 — system-key access can't
// be tenant-scoped), and it is unnecessary there because metadataVersion is exempt from the
// system-key gate and read globally even under a tenant.
func (d *FDBDatabase) applyReadSystemKeys(o fdb.TransactionOptions) {
	if !d.isTenant {
		o.SetReadSystemKeys()
	}
}

// Run executes a function within a transaction with automatic retry handling.
// Before committing, flushes any queued versionstamp mutations.
// Matches Java's FDBRecordContext.commitAsync() behavior.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastCtx *FDBRecordContext
	var commitStart time.Time
	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.applyReadSystemKeys(tx.Options())
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
			env:           d.env,
		}
		recordCtx.SetTimer(d.Timer())
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// EventCommit spans the pre-commit checks plus the commit itself, which is
		// the interval Java measures: startTimeNanos is taken at the first line of
		// commitAsync, before runCommitChecks, and the event is recorded when the
		// commit future completes (FDBRecordContext.java:478, :513-533; the
		// FDBStoreTimer javadoc at :81-86 says so explicitly). Post-commit hooks
		// are outside the span in Java and are outside it here.
		//
		// It has to be recorded out here rather than in FDBRecordContext.Commit
		// because on this path the commit belongs to the transactor's retry loop —
		// nothing ever calls Commit, which is why the whole autocommit SQL path
		// reported zero commits while committing on every statement.
		commitStart = time.Now()

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
	recordCommitSince(lastCtx, commitStart)

	// Run post-commit callbacks after successful commit
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}
	return result, nil
}

// recordCommitSince records EventCommit for a transaction that committed inside
// the transactor's retry loop. On a retry, commitStart is overwritten by each
// attempt, so the recorded span is the successful attempt's — the same one Java
// records, since Java's per-attempt commitAsync only reaches its success arm on
// the attempt that commits.
//
// Zero commitStart means the closure returned before reaching the commit (fn
// failed), so there was no commit to time.
//
// Java's other two commit outcomes are NOT ported here, deliberately.
// COMMIT_READ_ONLY needs the committed version to tell a write-free transaction
// apart (FDBRecordContext.java:492, :524) and COMMIT_FAILURE needs the commit's
// own exception (:517); on this path the transactor owns the commit and surfaces
// neither. Recording a plain COMMIT for those cases would be worse than
// recording nothing — it would inflate the commit count with transactions that
// did not commit.
func recordCommitSince(rc *FDBRecordContext, commitStart time.Time) {
	if rc == nil || commitStart.IsZero() {
		return
	}
	rc.Timer().RecordSince(EventCommit, commitStart)
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
		d.applyReadSystemKeys(rtx.Options())
		return fn(rtx)
	})
}

// RunWithWeakReads is like Run but applies weak read semantics to the transaction.
// If IsCausalReadRisky is set, the transaction reads from any replica.
// Matches Java's FDBDatabase.openContext(config, timer, weakReadSemantics, ...).
func (d *FDBDatabase) RunWithWeakReads(ctx context.Context, weak WeakReadSemantics, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastCtx *FDBRecordContext
	var commitStart time.Time
	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.applyReadSystemKeys(tx.Options())
		if weak.IsCausalReadRisky {
			tx.Options().SetCausalReadRisky()
		}
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
			env:           d.env,
		}
		recordCtx.SetTimer(d.Timer())
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		commitStart = time.Now()
		if err := recordCtx.runCommitChecks(); err != nil {
			return nil, err
		}
		recordCtx.flushVersionMutations()
		return result, nil
	})
	if err != nil {
		return result, err
	}
	recordCommitSince(lastCtx, commitStart)
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
	var commitStart time.Time

	result, err := runTransactCtx(d.transactor, ctx, func(tx fdb.WritableTransaction) (any, error) {
		// Reset on retry — previous attempt's future is stale
		vsFuture = nil
		hasVersionMutations = false

		d.applyReadSystemKeys(tx.Options())
		recordCtx := &FDBRecordContext{
			transactionID: nextTransactionID.Add(1),
			tx:            tx,
			ctx:           ctx,
			env:           d.env,
		}
		recordCtx.SetTimer(d.Timer())
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		commitStart = time.Now()

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
	recordCommitSince(lastCtx, commitStart)

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
// Run / RunRead gold path, standalone transactions (CreateWritableTransaction —
// used by the FDBDatabaseRunner and SQL BeginTx), and LocalityGetBoundaryKeys
// (online MUTUAL indexing) all through backend interfaces, so those work on
// libfdb_c. Only the pure-Go-typed CreateTransaction (which returns the concrete
// fdb.Transaction) is pure-Go-only in v1; it — and any operation a custom
// transactor genuinely can't provide — fail fast with this error.
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

// CreateWritableTransaction creates a standalone, non-retry transaction as the
// backend-agnostic WritableTransaction interface. Unlike CreateTransaction (which
// returns the concrete pure-Go fdb.Transaction and is therefore pure-Go-only), this
// works on ANY backend — including the libfdb_c escape hatch — so the SQL engine's
// database/sql explicit transactions (BeginTx / COMMIT, which span multiple driver
// calls and so can't use the closure-based Run gold path) are not silently
// pure-Go-only. Prefer this over CreateTransaction wherever a concrete pure-Go
// handle isn't specifically required.
func (d *FDBDatabase) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	if d.tenant != (fdb.Tenant{}) {
		return d.tenant.CreateTransaction()
	}
	// Pure-Go concrete handle present (default open, chaos transactor, or a pure-Go
	// fdbclient backend): use it directly — identical to CreateTransaction.
	if d.db.IsValid() {
		return d.db.CreateTransaction()
	}
	// No concrete handle ⇒ a non-pure-Go backend (libfdb_c). Delegate to the
	// backend's own standalone-transaction creator.
	if be, ok := d.transactor.(fdb.BackendDatabase); ok {
		return be.CreateWritableTransaction()
	}
	return nil, &BackendCapabilityError{Op: "CreateWritableTransaction"}
}

// LocalityGetBoundaryKeys returns the FDB shard boundary keys within r, working on
// both the pure-Go and libfdb_c backends (the online MUTUAL indexer uses them to
// partition the keyspace into fragments for concurrent building). It's a read of
// the \xff/keyServers system range — byte-identical on the pure-Go and libfdb_c
// clients against the same cluster — so mutual indexing parallelizes on either
// backend. A handle that can't provide it (a tenant-backed FDBDatabase, or a
// custom transactor) returns BackendCapabilityError; callers that want graceful
// degradation (1 fragment) treat an error as "no boundaries".
//
// Unlike CreateWritableTransaction there is no tenant branch: shard boundaries
// are a cluster-wide property of the keyServers map, independent of any tenant.
func (d *FDBDatabase) LocalityGetBoundaryKeys(r fdb.ExactRange, limit int, readVersion int64) ([]fdb.Key, error) {
	if d.db.IsValid() {
		return d.db.LocalityGetBoundaryKeys(r, limit, readVersion)
	}
	if be, ok := d.transactor.(fdb.BackendDatabase); ok {
		return be.LocalityGetBoundaryKeys(r, limit, readVersion)
	}
	return nil, &BackendCapabilityError{Op: "LocalityGetBoundaryKeys"}
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

	// env is the DST Tier-0 environment inherited from the FDBDatabase (Clock + Randomness +
	// Buggify). Nil means production; read it through Env() which is nil-safe. Persisted-byte
	// sites (store header LastUpdateTime, lock-state timestamp, heartbeats, nonces) route
	// their time/randomness through here so a simulation run is byte-reproducible.
	env *dst.Env

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

// Env returns the DST environment for this context (Clock + Randomness + Buggify),
// inherited from the FDBDatabase. Never nil in the sense that matters: the returned *dst.Env
// may be nil, and the *dst.Env accessors (Now/Read/Fault) treat nil as production. Use this
// at every persisted-byte site instead of time.Now / crypto.rand so a simulation run is
// byte-reproducible.
func (rc *FDBRecordContext) Env() *dst.Env {
	if rc == nil {
		return nil
	}
	return rc.env
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

// NewFDBRecordContext wraps an externally-created FDB transaction — the path SQL's explicit
// BeginTx→COMMIT takes, plus direct-transaction-control tests.
//
// env is REQUIRED rather than defaulted, and that is the point. This is the sixth construction
// site of an FDBRecordContext; the other five (Run/RunAsync/ReadOnly in this file and the two
// in runner.go) all thread d.env, and this one silently did not. The consequence was that every
// explicit transaction ran with env==nil — production seams, wall clock, crypto/rand — even
// under a fully seeded simulation environment. That is precisely the path an explicit-COMMIT
// semantics RFC needs to replay, so it was the one path the seam did not cover. A default here
// would reintroduce the same silence; passing nil is allowed but has to be written down.
//
// Callers holding an *FDBDatabase should NOT call this — call db.NewRecordContext(tx),
// which threads every piece of per-database state at once. Enumerating the fields at each
// call site is what let env go unthreaded here in the first place, and a second such field
// (the StoreTimer) would have repeated it exactly.
func NewFDBRecordContext(tx fdb.WritableTransaction, env *dst.Env) *FDBRecordContext {
	return &FDBRecordContext{
		tx:  tx,
		ctx: context.Background(),
		env: env,
	}
}

// NewRecordContext wraps an externally-created FDB transaction in a context that inherits
// everything this database gives the contexts it opens itself: the DST environment and the
// StoreTimer. It is the form every caller holding an *FDBDatabase should use — SQL's
// explicit BeginTx→COMMIT path among them.
//
// The point is that per-database state is threaded in ONE place. NewFDBRecordContext takes
// the fields one by one, which is why env was silently dropped on the explicit-transaction
// path until someone noticed; adding the timer as a second such parameter would have set up
// the identical failure for instrumentation, where the symptom is even quieter (a metric
// that reads zero looks exactly like a workload that did nothing).
func (d *FDBDatabase) NewRecordContext(tx fdb.WritableTransaction) *FDBRecordContext {
	rc := NewFDBRecordContext(tx, d.Env())
	rc.SetTimer(d.Timer())
	return rc
}

// Transaction returns the underlying FDB transaction
func (rc *FDBRecordContext) Transaction() fdb.WritableTransaction {
	return rc.tx
}

// ReadTransaction returns the transaction a READ should go through for the
// given isolation: the snapshot view (no read conflict ranges) when snapshot
// is true, the plain transaction (conflict-tracking) otherwise.
//
// This is the ONE place the snapshot/serializable choice is made, so that
// "serializable" is a property of the mechanism rather than of each leaf
// remembering to ask. Java has exactly this method —
// FDBRecordContext.readTransaction(boolean) (FDBRecordContext.java:660-666) —
// and every Java leaf resolves through it with the same expression:
// context.readTransaction(scanProperties.getExecuteProperties()
// .getIsolationLevel().isSnapshot()) (KeyValueCursorBase.java:358,
// VectorIndexMaintainer.java:201-202, TextIndexMaintainer.java:542,
// RankIndexMaintainer.java:292).
//
// A leaf that calls Transaction().Snapshot() directly instead is not making a
// local choice — it is silently overriding the caller's isolation, which reads
// as isolated and commits as non-serializable.
func (rc *FDBRecordContext) ReadTransaction(snapshot bool) fdb.ReadTransaction {
	return readTransactionFor(rc.tx, snapshot)
}

// readTransactionFor is the single implementation behind every
// snapshot-vs-serializable choice in the package. It is a pure function of the
// transaction and the flag — which is exactly what Java's
// FDBRecordContext.readTransaction is (`snapshot ? ensureActive().snapshot() :
// ensureActive()`), so siting it here rather than only on the context lets an
// index maintainer resolve isolation from the transaction it already holds
// without needing a context reference it may not have.
func readTransactionFor(tx fdb.WritableTransaction, snapshot bool) fdb.ReadTransaction {
	if snapshot {
		return tx.Snapshot()
	}
	return tx
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

// Commit commits the transaction.
//
// Records EventCommit, as every commit path must. Java has ONE commit method
// (FDBRecordContext.commitAsync) and it records the event unconditionally
// (FDBRecordContext.java:513-533); Go splits that method three ways — here,
// CommitWithHooks and CommitWithVersionstamp — and a split where only one arm
// records turns the metric into a function of which API the caller happened to
// pick. Only CommitWithVersionstamp recorded, which is why explicit SQL
// transactions committed on every COMMIT statement and reported none.
func (rc *FDBRecordContext) Commit() error {
	commitStart := time.Now()
	err := rc.tx.Commit().Get()
	rc.Timer().RecordSince(EventCommit, commitStart)
	return err
}

// CommitWithHooks runs pre-commit checks, flushes pending version mutations,
// commits the FDB transaction, and runs post-commit callbacks.
// Use this instead of Commit() when the context was created manually (not via Run()).
//
// The EventCommit span starts before the pre-commit checks and ends when the
// commit resolves, which is Java's interval — its startTimeNanos is taken at the
// first line of commitAsync, ahead of runCommitChecks, and the post-commit hooks
// are chained after the event is recorded (FDBRecordContext.java:478, :513-533).
func (rc *FDBRecordContext) CommitWithHooks() error {
	commitStart := time.Now()
	if err := rc.runCommitChecks(); err != nil {
		return err
	}
	rc.flushVersionMutations()
	if err := rc.tx.Commit().Get(); err != nil {
		return err
	}
	rc.Timer().RecordSince(EventCommit, commitStart)
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
// Nil-safe on the receiver, for the same reason FDBRecordContext.Env is: every
// StoreTimer method tolerates a nil timer, so "no instrumentation" is a supported
// state all the way down — and an accessor at the head of that chain must not be
// the one call in it that panics. Call sites are of the form
// store.context.Timer().RecordSince(...), where the nil check would otherwise have
// to be repeated at each one.
func (rc *FDBRecordContext) Timer() *StoreTimer {
	if rc == nil {
		return nil
	}
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
