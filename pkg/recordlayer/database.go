// Portions derived from FoundationDB Record Layer (FDBRecordContext.java,
// FDBDatabase.java, FDBDatabaseRunner.java, FDBDatabaseFactory.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"fdb.dev/internal/fdbclient"
	"fdb.dev/pkg/fdbgo/fdb"
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

	// resolverCache is the directory cache of committed resolver mappings
	// (Java's FDBDatabase.getDirectoryCache); reverseCacheEntry the long of
	// the reverse directory cache's directory, resolved once.
	resolverCacheOnce sync.Once
	resolverCache     *resolverCache
	reverseCacheMu    sync.Mutex
	reverseCacheEntry *int64

	// storeStateCache caches store state across transactions.
	// Default: PassThroughRecordStoreStateCache (no caching).
	// Matches Java's FDBDatabase.storeStateCache field.
	storeStateCache FDBRecordStoreStateCache

	// env is the simulation environment (Clock + Random) inherited by every
	// FDBRecordContext this database opens. Nil means production (wall clock, crypto/rand)
	// — the nil-safe *Env accessors handle that. A simulation sets a seeded env via SetEnv
	// so persisted timestamps/nonces are reproducible (RFC-199 Tier 0).
	env *Env

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

	// The attempt policy of Run and its variants (attempt_loop.go): Java's
	// FDBDatabaseFactory maxAttempts / initialDelayMillis / maxDelayMillis.
	// Zero means the default.
	maxAttempts  atomic.Int64
	initialDelay atomic.Int64
	maxDelay     atomic.Int64
	delaysSet    atomic.Bool

	attemptObserver atomic.Pointer[AttemptObserver]
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

// SetEnv installs the simulation environment inherited by every context this database opens.
// Production leaves it unset (nil). Returns d for chaining.
func (d *FDBDatabase) SetEnv(env *Env) *FDBDatabase {
	d.env = env
	return d
}

// Env returns the database's simulation environment, or nil (which the *Env accessors treat
// as production). Nil-safe on the receiver, like FDBRecordContext.Env: an accessor whose whole
// contract is "nil means production" must not be the one call in the chain that panics.
func (d *FDBDatabase) Env() *Env {
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

	// knobs are the client knobs set (SetKnob), knobOrder the order they were
	// first set, and inited whether the client has started (the first open).
	knobs     map[string]string
	knobOrder []string
	inited    bool

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
	if err := f.startClientLocked(); err != nil {
		return nil, fmt.Errorf("open database %q: %w", clusterFile, err)
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

// OpenRecordContext opens a transaction and its record context as each Run
// attempt does (system keys readable unless the database is tenant-scoped),
// for a caller that commits (CommitWithHooks) or cancels it itself and does
// its own retrying.
func (d *FDBDatabase) OpenRecordContext(ctx context.Context) (*FDBRecordContext, error) {
	tx, err := d.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	d.applyReadSystemKeys(tx.Options())
	return d.NewRecordContextIn(ctx, tx), nil
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
