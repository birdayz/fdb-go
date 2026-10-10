// Portions derived from FoundationDB Record Layer (
// RelationalKeyspaceProvider.java, RelationalPlanCache.java,
// RecordLayerEngine.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package sqldriver implements a database/sql driver for the
// FoundationDB Record Layer relational (SQL) layer.
//
// Register the driver by blank-importing this package, then open a
// connection:
//
//	import (
//	    "database/sql"
//	    _ "fdb.dev/pkg/relational/sqldriver"
//	)
//
//	sqldriver.RegisterDomainIfNotExists("FRL")
//	db, err := sql.Open("fdbsql", "fdbsql:///FRL/MYDB?cluster_file=/etc/foundationdb/fdb.cluster")
//
// A database path is /DOMAIN/DATABASE, and its domain must be registered
// first, once per process, exactly as a Java program registers it with
// RelationalKeyspaceProvider.instance().registerDomainIfNotExists. The driver
// registers none itself (Java's engine neither); Java's server, CLI and
// yaml-test runner use FRL. /__SYS, the system catalog, needs no domain.
//
// DSN shape mirrors Java's JDBC URI (minus the jdbc: prefix):
//
//	fdbsql:///PATH                          — embedded, default cluster file
//	fdbsql:///PATH?cluster_file=/path       — embedded, explicit cluster file
//	fdbsql://HOST:PORT/PATH                 — remote (gRPC) — NOT YET IMPLEMENTED
//
// This is the public entry point. Internally it wraps
// pkg/relational/core which implements the SQL engine over FDB.
//
// The port follows Java's fdb-relational-* modules 1:1 wherever
// reasonable; database/sql compatibility is the single intentional
// deviation — the Go-idiomatic driver surface is at the edge, the Java
// surface (pkg/relational/api.Connection etc.) is preserved underneath.
package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/internal/fdbclient"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
)

// DriverName is the database/sql driver name.
const DriverName = "fdbsql"

// defaultClusterFileEnv is the environment variable FDB checks for cluster file path.
const defaultClusterFileEnv = "FDB_CLUSTER_FILE"

// fdbDBCache caches FDB database handles by cluster-file path so repeated
// sql.Open calls against the same cluster don't leak FDB connections.
//
// Why: database/sql creates a new driver.Connector per sql.Open, and each
// Connector lazily opens its own FDB database (see initialize). database/sql
// has no Connector-close hook, so once an *sql.DB is closed, the Connector
// (and its FDB database handle) can only be released when GC eventually runs
// — which doesn't release the underlying TCP connection to FDB. Workloads
// that repeatedly open+close *sql.DB against the same cluster (e.g. plandiff's
// per-corpus-entry ephemeral schemas) accumulate hundreds of leaked FDB
// connections, eventually exhausting the testcontainer FDB's connection
// table and causing i/o timeouts on subsequent opens.
//
// The cache is process-global, keyed by cluster-file path. Concurrent opens
// against the same path race once and the loser drops its handle. Different
// cluster files get distinct entries.
var fdbDBCache sync.Map // clusterFile string -> *recordlayer.FDBDatabase

// RegisterBackend associates an already-built FDBDatabase with a cluster_file key, so a DSN of
// the form "fdbsql:///FRL/DB?cluster_file=<key>" drives the full SQL engine (parser → Cascades →
// executor → record layer) over that backend instead of opening a real cluster. It returns a
// func that unregisters the key.
//
// This is the seam for deterministic-simulation harnesses that back the SQL stack with SimFDB
// (RFC-199 DST): register a SimFDB-backed FDBDatabase, open a `fdbsql` DSN against its key, and
// the whole relational layer runs in-process with no Docker. It only exposes the cache
// population that connect() already performs internally — production behavior is unchanged.
func RegisterBackend(key string, db *recordlayer.FDBDatabase) (unregister func()) {
	fdbDBCache.Store(key, db)
	applyStoreTimer(key, db)
	return func() { fdbDBCache.Delete(key) }
}

// storeTimers holds the record-layer StoreTimer armed for each cluster-file key, so a
// timer can be installed before the corresponding FDBDatabase exists. Keyed the same way
// as fdbDBCache and never removed: a scrape endpoint outlives any single *sql.DB, and
// dropping the timer when the last connection closed would silently reset an operator's
// counters mid-flight.
var storeTimers sync.Map // clusterFile string -> *recordlayer.StoreTimer

// applyStoreTimer installs the armed timer, if any, on a database just entered into the
// cache. Called from both directions (here and EnableStoreTimer) precisely because either
// can happen first; both write the same pointer, so the overlap is idempotent rather than
// a race, and the two orderings between them leave no window where a database ends up
// uninstrumented.
func applyStoreTimer(clusterFile string, db *recordlayer.FDBDatabase) {
	if t, ok := storeTimers.Load(clusterFile); ok {
		db.SetTimer(t.(*recordlayer.StoreTimer))
	}
}

// EnableStoreTimer arms record-layer instrumentation for the given cluster_file key and
// returns the StoreTimer that collects it. Idempotent: repeat calls for the same key
// return the same timer, so an operator never has to reason about who called first.
//
// This is the inversion the SQL path needs. The driver opens the record-layer database
// itself and caches it privately (fdbDBCache), so a tenant service that speaks only
// database/sql has no *recordlayer.FDBDatabase to call SetTimer on — and record-layer
// counters were, in consequence, unobservable from SQL. Arming by key instead of by handle
// works before the lazy Connect that opens the database:
//
//	timer := sqldriver.EnableStoreTimer("/etc/foundationdb/fdb.cluster")
//	http.Handle("/metrics/recordlayer", rlmetrics.Handler(timer))
//	db, _ := sql.Open("fdbsql", "fdbsql:///T/1?cluster_file=/etc/foundationdb/fdb.cluster")
//
// SCOPE — and this is the part that decides what the numbers mean. One timer per
// cluster-file key means one timer per process, aggregating EVERY tenant, connection and
// transaction that runs against that cluster. It is deliberately not per-tenant: tenant
// count in a SaaS deployment is unbounded and operator-driven, so a tenant label would
// make the cardinality of every record-layer metric grow with the customer list — the
// classic way to take down a Prometheus. Per-tenant attribution is already available at a
// layer that can afford it, because it is sampled and log-shaped rather than a live time
// series: PlanGenerationLogger and ExecutionStatsLogger are installed per connection and
// close over the tenant ID (see docs/mt-saas.md §4).
//
// A caller who genuinely wants finer scope has it without a label: build the
// *recordlayer.FDBDatabase, call SetTimer on it, and RegisterBackend it under a key of its
// own. That keeps the cardinality decision explicit and in the caller's hands.
func EnableStoreTimer(clusterFile string) *recordlayer.StoreTimer {
	actual, _ := storeTimers.LoadOrStore(clusterFile, recordlayer.NewStoreTimer())
	timer := actual.(*recordlayer.StoreTimer)
	if db, ok := fdbDBCache.Load(clusterFile); ok {
		db.(*recordlayer.FDBDatabase).SetTimer(timer)
	}
	return timer
}

// Driver is the database/sql/driver.Driver for fdbsql.
//
// Implements driver.Driver and driver.DriverContext.
type Driver struct{}

// Open satisfies driver.Driver. Prefer OpenConnector (via
// driver.DriverContext) for lazy connection pooling.
func (d *Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector parses the DSN and returns a lazy Connector.
// Parsing errors are reported here so misconfigured DSNs surface at
// sql.Open time, not at first query.
//
// Connection options are decoded here too, and the decoded value is kept on the
// Connector. Deferring the decode to Connect would break the contract this
// doc-comment states: Connect opens FDB before it would ever look at the
// options, so a misspelled option value would surface as a cluster-connection
// failure instead of the DSN error it is — misleading exactly when the operator
// needs a clear message, and a security-relevant option that never took effect.
//
// The DSN is FROZEN here: the Connector keeps a private deep clone, and every
// later read — Path, Schema, Mode, cluster_file, and the decoded options — comes
// from that one snapshot. Freezing only the decoded options while the rest of
// the struct stayed live would be a split brain, and the security-relevant
// direction is the bad one: a caller holding the *DSN could flip
// restrict_ddl_to_session_database after OpenConnector, have Connect honour the
// stale decode (restriction silently not what the DSN now says), and slip a
// newly-malformed value past validation entirely.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	parsed, err := ParseDSN(name)
	if err != nil {
		return nil, err
	}
	// Clone before anything reads it, so the decode below and every read in
	// Connect / initialize observe the same immutable value.
	dsn := parsed.Clone()
	connOpts, err := dsn.ConnectionOptions()
	if err != nil {
		return nil, err
	}
	return &Connector{driver: d, dsn: dsn, connOpts: connOpts}, nil
}

// Connector holds a parsed DSN and produces connections on demand.
// The FDB database, keyspace, and factory are initialised lazily on
// the first Connect call. The catalog Bootstrap (Initialize) is deferred
// further — it runs inside the first DDL transaction, not at Connect time.
type Connector struct {
	driver *Driver
	dsn    *DSN
	// connOpts are the DSN's connection options, decoded and validated in
	// OpenConnector so a malformed value is a DSN error rather than a
	// connect-time failure. Never nil.
	connOpts *api.Options

	once    sync.Once
	fdbDB   *recordlayer.FDBDatabase
	cat     *catalog.RecordLayerStoreCatalog
	ks      *keyspace.RelationalKeyspace
	factory *ddl.RecordLayerMetadataOperationsFactory
	// planCache is the engine-wide plan cache every connection of this
	// connector shares (Java's RelationalPlanCache, one per engine), sized
	// by the DSN's PLAN_CACHE_* options.
	planCache *embedded.RelationalPlanCache
	// warmUp is the stored-query warm-up's outcome (Java's
	// OFFLINE_STORED_QUERIES_* counts). Atomic because StoredQueryWarmUp is a
	// public accessor a caller may poll while another goroutine's first
	// Connect is still inside once.Do; nil until initialize stores it.
	warmUp  atomic.Pointer[embedded.StoredQueryWarmUpCounts]
	initErr error
}

// StoredQueryWarmUp returns what the connector's start planned into its plan
// cache: Java's OFFLINE_STORED_QUERIES_* counts. Zero before the first
// connection.
func (c *Connector) StoredQueryWarmUp() embedded.StoredQueryWarmUpCounts {
	if w := c.warmUp.Load(); w != nil {
		return *w
	}
	return embedded.StoredQueryWarmUpCounts{}
}

// Connect opens a connection. Honors ctx.Done() for cancellation.
// On first call, initialises the FDB database and catalog (idempotent).
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.dsn.Mode == ModeRemote {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"remote (gRPC) mode is not yet implemented")
	}
	c.once.Do(func() { c.initErr = c.initialize(ctx) })
	if c.initErr != nil {
		return nil, c.initErr
	}
	conn := embedded.New(c.dsn.Path, c.fdbDB, c.cat, c.factory, c.ks)
	conn.SetOptions(c.connOpts)
	conn.SetPlanCache(c.planCache)
	if c.dsn.Schema != "" {
		conn.SetDefaultSchema(c.dsn.Schema)
	}
	return conn, nil
}

// initialize opens FDB and wires catalog + factory. The catalog Bootstrap
// (Initialize) is deferred — it runs on the first DDL transaction, not here.
func (c *Connector) initialize(ctx context.Context) error {
	clusterFile := c.dsn.Options["cluster_file"]
	if clusterFile == "" {
		clusterFile = os.Getenv(defaultClusterFileEnv)
	}

	// API-version selection is owned by fdbclient.Open (it selects the default
	// only if the app hasn't already), so we must NOT pre-pin it here. Pre-pinning
	// the pure-Go facade to a different version than the active backend wants is a
	// hard error: under -tags libfdbc, libfdbc.Open sets the facade to 730 (the
	// record layer reads it for versionstamps), so a stale 720 pin would make every
	// open fail with a version mismatch. An app that legitimately selected its own
	// version still wins — fdbclient.Open never overrides a set version.

	// Reuse a previously-opened FDB database for this cluster file.
	// See fdbDBCache docstring above for why this is necessary.
	cacheKey := clusterFile
	if cached, ok := fdbDBCache.Load(cacheKey); ok {
		c.fdbDB = cached.(*recordlayer.FDBDatabase)
	} else {
		// Leave an empty path for the selected backend's default lookup, including
		// a present-but-empty environment variable and a local fdb.cluster.
		rawDB, err := fdbclient.Open(clusterFile)
		if err != nil {
			return api.WrapError(api.ErrCodeInternalError, "open FDB database", err)
		}
		newDB := recordlayer.NewFDBDatabaseWithBackend(rawDB)
		newDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
		// LoadOrStore returns the previously-stored entry if a concurrent
		// caller raced ahead. In that case, the FDB database we just
		// opened becomes garbage; close it to release its TCP connection.
		actual, loaded := fdbDBCache.LoadOrStore(cacheKey, newDB)
		if loaded {
			rawDB.Close()
			c.fdbDB = actual.(*recordlayer.FDBDatabase)
		} else {
			c.fdbDB = newDB
		}
	}

	// Install the armed StoreTimer, if any. This runs AFTER the database is in the
	// cache on every branch, which is what closes the ordering window against a
	// concurrent EnableStoreTimer: that call stores the timer before it looks in the
	// cache, so if it missed this database then this read cannot miss its timer.
	applyStoreTimer(cacheKey, c.fdbDB)

	// root subspace is the empty subspace — all catalog and schema data lives
	// under well-known tuple prefixes via RelationalKeyspace.
	c.ks = keyspace.New(subspace.Sub())
	cat, catErr := catalog.NewRecordLayerStoreCatalog(c.ks.CatalogSubspace())
	if catErr != nil {
		return catErr
	}
	c.cat = cat
	c.factory = ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, c.ks)
	c.planCache = embedded.NewRelationalPlanCache(c.connOpts)
	// Java's RecordLayerEngine.makeEngine: the engine's start plans every
	// template's stored queries into the shared cache. Failures are logged,
	// never returned.
	warmUp := embedded.WarmStoredQueries(ctx, c.planCache,
		embedded.StoredQueryTemplates(ctx, c.fdbDB, c.cat))
	c.warmUp.Store(&warmUp)
	return nil
}

// Driver returns the driver that created this Connector.
func (c *Connector) Driver() driver.Driver { return c.driver }

// DSN returns a COPY of the parsed DSN. Exposed for diagnostics.
//
// The copy is the point: the Connector's DSN is an immutable snapshot taken at
// OpenConnector, and handing out the internal pointer would make it mutable
// again through this accessor — the only route by which anything outside this
// package can reach it. Mutating the returned value affects nothing; to change
// a connection's configuration, open a new Connector with a new DSN string.
func (c *Connector) DSN() *DSN { return c.dsn.Clone() }

// Static interface checks.
var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
	_ driver.Connector     = (*Connector)(nil)
)

func init() {
	sql.Register(DriverName, &Driver{})
}

// RegisterDomainIfNotExists makes /name/DATABASE a valid database path for
// every connection of the process: Java's
// RelationalKeyspaceProvider.instance().registerDomainIfNotExists. Idempotent
// and additive; a path under an unregistered domain is INVALID_PATH where Java
// resolves it (CREATE DATABASE, a schema's store, connecting to a schema).
func RegisterDomainIfNotExists(name string) {
	keyspace.RegisterDomainIfNotExists(name)
}
