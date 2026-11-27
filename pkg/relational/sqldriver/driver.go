// Portions derived from FoundationDB Record Layer (
// RelationalKeyspaceProvider.java, RelationalPlanCache.java,
// RecordLayerEngine.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package sqldriver implements a database/sql driver for the
// FoundationDB Record Layer relational (SQL) layer.
//
// Construct an explicit Engine (with instance-scoped domains, StoreTimer,
// default ResourceLimits, and Close lifecycle) and open a *sql.DB pool from it:
//
//	engine, err := sqldriver.NewEngine(sqldriver.EngineConfig{
//	    ClusterFile: "/etc/foundationdb/fdb.cluster",
//	    Domains:     []string{"FRL"},
//	})
//	if err != nil {
//	    return err
//	}
//	defer engine.Close()
//
//	db, err := engine.OpenDB("/FRL/MYDB", sqldriver.WithSchema("S"))
//
// A database path is /DOMAIN/DATABASE, and its domain must be registered on
// the Engine (EngineConfig.Domains or Engine.RegisterDomain) before use; /__SYS,
// the system catalog, needs no domain. Legacy sql.Open("fdbsql", dsn) also
// registers the "fdbsql" driver on import and validates paths against the
// process-global keyspace domain registry (keyspace.RegisterDomainIfNotExists).
//
// DSN shape mirrors Java's JDBC URI (minus the jdbc: prefix):
//
//	fdbsql:///PATH                          — embedded, default cluster file
//	fdbsql:///PATH?cluster_file=/path       — embedded, explicit cluster file
//	fdbsql://HOST:PORT/PATH                 — remote (gRPC) — NOT YET IMPLEMENTED
//
// read_only=true makes every connection read-only: only SELECT and EXPLAIN of a
// SELECT run (anything else, a multi-statement batch included, is 0A000), and
// every transaction is cancelled instead of committed, so nothing a read does
// persists, not even the store-header upgrade an open makes on a store an older
// version wrote. sql.TxOptions{ReadOnly: true} is accepted on such a
// connection and refused on any other.
//
// NewConnector drives the driver over an FDB database the caller already has,
// with no cluster_file:
//
//	connector, err := sqldriver.NewConnector(fdbDB, "fdbsql:///FRL/MYDB?schema=S&read_only=true")
//	db := sql.OpenDB(connector)
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
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/internal/fdbclient"
	wire "fdb.dev/internal/fdbwire"
	"fdb.dev/internal/relational/catalog"
	"fdb.dev/internal/relational/ddl"
	"fdb.dev/internal/relational/keyspace"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// DriverName is the database/sql driver name.
const DriverName = "fdbsql"

// defaultClusterFileEnv is the environment variable FDB checks for cluster file path.
const defaultClusterFileEnv = "FDB_CLUSTER_FILE"

// Driver is the database/sql/driver.Driver for fdbsql.
//
// Implements driver.Driver and driver.DriverContext.
type Driver struct {
	openBackend func(clusterFile string) (fdb.BackendDatabase, error)
}

// Open satisfies driver.Driver. Prefer OpenConnector (via
// driver.DriverContext) for lazy connection pooling. The returned connection
// owns its underlying connector and closes it when Conn.Close runs.
func (d *Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		_ = c.(*Connector).Close()
		return nil, err
	}
	return &connectorClosingConn{
		EmbeddedConnection: conn.(*embedded.EmbeddedConnection),
		connector:          c.(*Connector),
	}, nil
}

type connectorClosingConn struct {
	*embedded.EmbeddedConnection
	connector *Connector
}

func (c *connectorClosingConn) Close() error {
	connErr := c.EmbeddedConnection.Close()
	closeErr := c.connector.Close()
	if connErr != nil {
		return connErr
	}
	return closeErr
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
	readOnly, err := dsn.ReadOnly()
	if err != nil {
		return nil, err
	}
	return &Connector{driver: d, dsn: dsn, connOpts: connOpts, readOnly: readOnly, openBackend: d.openBackend}, nil
}

// NewConnector returns a Connector over db, an FDB database the caller opened
// and owns, for sql.OpenDB. Unlike sql.Open it neither opens nor owns a
// database by cluster file: the connector, its catalog, keyspace and plan cache
// are its own, and Connector.Close does not close db. dsn is an embedded DSN
// without cluster_file, for example "fdbsql:///FRL/MYDB?schema=S&read_only=true";
// its domain must be registered (keyspace.RegisterDomainIfNotExists) like any
// other. The DSN is checked here, as in OpenConnector. A StoreTimer is the
// caller's to set on db. For full engine lifecycle, instance-scoped domains, and
// default resource limits, prefer NewEngine.
func NewConnector(db *recordlayer.FDBDatabase, dsn string) (driver.Connector, error) {
	if db == nil {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "NewConnector: nil FDB database")
	}
	c, err := (&Driver{}).OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	connector := c.(*Connector)
	if connector.dsn.Mode != ModeEmbedded {
		return nil, api.NewError(api.ErrCodeInvalidParameter,
			"NewConnector takes an embedded DSN (fdbsql:///PATH), not a remote one")
	}
	if _, ok := connector.dsn.Options["cluster_file"]; ok {
		return nil, api.NewError(api.ErrCodeInvalidParameter,
			"NewConnector does not take cluster_file: the FDB database is the caller's")
	}
	connector.fdbDB, connector.callerDB = db, true
	return connector, nil
}

// Connector holds a parsed DSN and produces connections on demand.
// The FDB database, keyspace, and factory are initialised lazily by the
// first Connect that succeeds in doing so; a failed initialization is retried
// by the next Connect. The catalog Bootstrap (Initialize) is deferred
// further — it runs inside the first DDL transaction, not at Connect time.
type Connector struct {
	driver *Driver
	dsn    *DSN
	// connOpts are the DSN's connection options, decoded and validated in
	// OpenConnector so a malformed value is a DSN error rather than a
	// connect-time failure. Never nil.
	connOpts *api.Options
	// readOnly is the DSN's read_only: every connection is made read-only
	// (EmbeddedConnection.SetReadOnly), and the connector's own catalog read
	// at its start is cancelled too.
	readOnly bool
	// callerDB marks fdbDB as the caller's (NewConnector or Engine.Connector):
	// initialize does not open a backend and Close does not close fdbDB.
	callerDB bool

	closeMu      sync.Mutex
	closed       atomic.Bool
	ownedBackend fdb.BackendDatabase // non-nil only when opened by Connector.initialize
	openBackend  func(clusterFile string) (fdb.BackendDatabase, error)

	init    initFlight
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
	// public accessor a caller may poll while another goroutine's Connect is
	// still initializing; nil until initialize stores it.
	warmUp atomic.Pointer[WarmUpCounts]

	// Engine-scoped defaults (nil for legacy Driver/NewConnector paths).
	engine                   *Engine
	failOnScanLimitReached   bool
	statementTimeout         time.Duration
	maxResultBytes           int64
	planHook                 PlanHook
	execStatsHook            ExecStatsHook
	hasSlowQueryThreshold    bool
	slowQueryThresholdMicros int64
}

// WarmUpCounts are the offline stored-query warm-up counts and duration
// collected at engine or connector start-up (Java's OFFLINE_STORED_QUERIES_*).
type WarmUpCounts struct {
	TemplatesProcessed     int
	QueriesProcessed       int
	QueriesFailed          int
	TempFunctionsProcessed int
	TempFunctionsFailed    int
	Duration               time.Duration
}

func warmUpCountsFromEmbedded(w embedded.StoredQueryWarmUpCounts) WarmUpCounts {
	return WarmUpCounts{
		TemplatesProcessed:     w.TemplatesProcessed,
		QueriesProcessed:       w.QueriesProcessed,
		QueriesFailed:          w.QueriesFailed,
		TempFunctionsProcessed: w.TempFunctionsProcessed,
		TempFunctionsFailed:    w.TempFunctionsFailed,
		Duration:               w.Duration,
	}
}

// initFlight runs an initialization until it succeeds once. An attempt runs
// on its own goroutine, detached from the cancellation of the caller that
// started it, so no caller's deadline cuts it short; every caller waits for it
// on its own context. A failure goes to the callers waiting on that attempt,
// and the next caller starts another.
type initFlight struct {
	mu      sync.Mutex
	done    bool
	running *initAttempt // nil when no attempt runs
}

type initAttempt struct {
	finished chan struct{}
	err      error // set before finished is closed
}

func (f *initFlight) do(ctx context.Context, init func(context.Context) error) error {
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		return nil
	}
	a := f.running
	if a == nil {
		if err := ctx.Err(); err != nil {
			f.mu.Unlock()
			return err
		}
		a = &initAttempt{finished: make(chan struct{})}
		f.running = a
		go f.attempt(context.WithoutCancel(ctx), a, init)
	}
	f.mu.Unlock()
	select {
	case <-a.finished:
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt runs init and publishes its outcome; a panic is its error, since
// nothing above this goroutine would recover it.
func (f *initFlight) attempt(ctx context.Context, a *initAttempt, init func(context.Context) error) {
	defer func() {
		if r := recover(); r != nil {
			a.err = embedded.RecoveredPanicError(r)
		}
		f.mu.Lock()
		f.done = a.err == nil
		f.running = nil
		f.mu.Unlock()
		close(a.finished)
	}()
	a.err = init(ctx)
}

// StoredQueryWarmUp returns what the connector's start planned into its plan
// cache: Java's OFFLINE_STORED_QUERIES_* counts. Zero before the first
// connection.
func (c *Connector) StoredQueryWarmUp() WarmUpCounts {
	if w := c.warmUp.Load(); w != nil {
		return *w
	}
	return WarmUpCounts{}
}

// Connect opens a connection. Honors ctx.Done() for cancellation, also while
// it waits for the connector's initialization, which ctx does not cut short
// (initFlight).
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.dsn.Mode == ModeRemote {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"remote (gRPC) mode is not yet implemented")
	}
	if c.closed.Load() {
		return nil, api.NewError(api.ErrCodeConnectionDoesNotExist, "connector is closed")
	}
	if c.engine != nil {
		if c.engine.closed.Load() {
			return nil, api.NewError(api.ErrCodeConnectionDoesNotExist, "engine is closed")
		}
	} else if err := c.init.do(ctx, c.initialize); err != nil {
		return nil, err
	}
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Load() {
		return nil, api.NewError(api.ErrCodeConnectionDoesNotExist, "connector is closed")
	}
	if c.engine != nil && c.engine.closed.Load() {
		return nil, api.NewError(api.ErrCodeConnectionDoesNotExist, "engine is closed")
	}
	conn := embedded.New(c.dsn.Path, c.fdbDB, c.cat, c.factory, c.ks)
	if c.readOnly {
		conn.SetReadOnly()
	}
	conn.SetOptions(c.connOpts)
	conn.SetPlanCache(c.planCache)
	if c.dsn.Schema != "" {
		conn.SetDefaultSchema(c.dsn.Schema)
	}
	if c.engine != nil {
		conn.SetDefaultFailOnScanLimitReached(c.failOnScanLimitReached)
		conn.SetDefaultStatementTimeout(c.statementTimeout)
		conn.SetDefaultMaxResultBytes(c.maxResultBytes)
		if c.planHook != nil {
			conn.SetDefaultPlanLogger(planHookAdapter{fn: c.planHook})
		}
		if c.execStatsHook != nil {
			conn.SetDefaultExecutionStatsLogger(execStatsHookAdapter{fn: c.execStatsHook})
		}
		if c.hasSlowQueryThreshold {
			conn.SetDefaultSlowQueryThresholdMicros(c.slowQueryThresholdMicros)
		}
	}
	return conn, nil
}

// initialize opens FDB, unless the database is the caller's (NewConnector),
// and wires catalog + factory. The catalog Bootstrap
// (Initialize) is deferred — it runs on the first DDL transaction, not here.
func (c *Connector) initialize(ctx context.Context) error {
	if c.closed.Load() {
		return api.NewError(api.ErrCodeConnectionDoesNotExist, "connector is closed")
	}
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

	var (
		openedBackend fdb.BackendDatabase
		newDB         *recordlayer.FDBDatabase
		planCache     *embedded.RelationalPlanCache
		handedOff     bool
	)
	defer func() {
		if handedOff {
			return
		}
		if planCache != nil {
			planCache.Invalidate()
		}
		if openedBackend != nil {
			openedBackend.Close()
		}
	}()

	if c.callerDB {
		newDB = c.fdbDB
	} else {
		// Leave an empty path for the selected backend's default lookup, including
		// a present-but-empty environment variable and a local fdb.cluster.
		openFn := fdbclient.Open
		if c.openBackend != nil {
			openFn = c.openBackend
		}
		rawDB, err := openFn(clusterFile)
		if err != nil {
			return api.WrapError(api.ErrCodeInternalError, "open FDB database", err)
		}
		openedBackend = rawDB
		if c.closed.Load() {
			return api.NewError(api.ErrCodeConnectionDoesNotExist, "connector is closed")
		}
		newDB = recordlayer.NewFDBDatabaseWithBackend(rawDB)
		newDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	}

	// root subspace is the empty subspace — all catalog and schema data lives
	// under well-known tuple prefixes via RelationalKeyspace.
	ks := keyspace.New(subspace.Sub())
	cat, catErr := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if catErr != nil {
		return catErr
	}
	factory := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)
	planCache = embedded.NewRelationalPlanCache(c.connOpts)
	// Java's RecordLayerEngine.makeEngine: the engine's start plans every
	// template's stored queries into the shared cache. Failures are logged,
	// never returned.
	var catalogReader keyspace.Runner = newDB
	if c.readOnly {
		catalogReader = embedded.ReadOnlyRunner(newDB)
	}
	warmUp := warmUpCountsFromEmbedded(embedded.WarmStoredQueries(ctx, planCache,
		embedded.StoredQueryTemplates(ctx, catalogReader, cat)))

	c.closeMu.Lock()
	if c.closed.Load() {
		c.closeMu.Unlock()
		return api.NewError(api.ErrCodeConnectionDoesNotExist, "connector is closed")
	}
	if !c.callerDB {
		c.fdbDB = newDB
		c.ownedBackend = openedBackend
	}
	c.ks = ks
	c.cat = cat
	c.factory = factory
	c.planCache = planCache
	c.warmUp.Store(&warmUp)
	handedOff = true
	c.closeMu.Unlock()
	return nil
}

// Close closes any FDB database handle and plan cache owned by this Connector
// and rejects subsequent Connect calls. Callers must close all active
// connections (or the owning *sql.DB) before calling Close, matching
// database/sql semantics: closing a Connector while connections are still
// active tears down its owned FDB handle underneath them. For Connectors
// opened from an Engine or NewConnector, the Engine or caller retains
// ownership of the underlying database, so Close leaves it open. Idempotent;
// implements io.Closer so sql.DB.Close cleans up connectors created by
// sql.Open.
func (c *Connector) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Swap(true) {
		return nil
	}
	owned := c.ownedBackend
	c.ownedBackend = nil
	var planCache *embedded.RelationalPlanCache
	if c.engine == nil {
		planCache = c.planCache
		c.planCache = nil
	}
	if planCache != nil {
		planCache.Invalidate()
	}
	if owned != nil {
		owned.Close()
	}
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
	_ io.Closer            = (*Connector)(nil)
)

func init() {
	sql.Register(DriverName, &Driver{})
}

// IsRetryable reports whether err represents a transient relational or FDB
// failure that is safe to retry in a fresh SQL transaction.
//
// It walks err's unwrap tree (including joined errors) and returns false for
// nil, typed-nil error pointers, context.Canceled, context.DeadlineExceeded
// (including custom errors whose Is method matches them),
// api.ErrCodeStatementCompletionUnknown (SQLSTATE 40003), and FDB
// maybe-committed codes 1021 (commit_unknown_result) and 1039
// (cluster_version_changed), because the transaction may already have
// committed and a blind retry risks double-applying non-idempotent work. If
// none of those vetoes appear anywhere in the error tree, it returns true
// when the tree contains api.ErrCodeSerializationFailure (SQLSTATE 40001),
// a *recordlayer.RecordCoreRetriableTransactionError, or a
// definitely-not-committed retryable FDB error (fdb.Error, *fdb.Error, or
// *wire.FDBError with fdb.IsRetryable(code) excluding 1021 and 1039).
func IsRetryable(err error) bool {
	if isNilError(err) || hasNonRetryableVeto(err) {
		return false
	}
	return isRetryableNode(err)
}

func isNilError(err error) bool {
	if err == nil {
		return true
	}
	v := reflect.ValueOf(err)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// isMaybeCommittedFDBCode reports FDB error codes where the transaction may
// already have committed (MAYBE_COMMITTED in pkg/fdbgo/fdb/error.go).
// SQL callers must not blindly retry them.
func isMaybeCommittedFDBCode(code int) bool {
	return code == 1021 || code == 1039
}

func hasNonRetryableVeto(err error) bool {
	if isNilError(err) {
		return false
	}
	if err == context.DeadlineExceeded || err == context.Canceled {
		return true
	}
	if custom, ok := err.(interface{ Is(error) bool }); ok {
		if custom.Is(context.DeadlineExceeded) || custom.Is(context.Canceled) {
			return true
		}
	}
	switch e := err.(type) {
	case *api.Error:
		if e.Code == api.ErrCodeStatementCompletionUnknown {
			return true
		}
		return hasNonRetryableVeto(e.Cause)
	case fdb.Error:
		return isMaybeCommittedFDBCode(e.Code)
	case *fdb.Error:
		return isMaybeCommittedFDBCode(e.Code)
	case *wire.FDBError:
		return isMaybeCommittedFDBCode(e.Code)
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, branch := range u.Unwrap() {
			if hasNonRetryableVeto(branch) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return hasNonRetryableVeto(u.Unwrap())
	}
	return false
}

func isRetryableNode(err error) bool {
	if isNilError(err) {
		return false
	}
	switch e := err.(type) {
	case *api.Error:
		switch e.Code {
		case api.ErrCodeSerializationFailure:
			return true
		case api.ErrCodeStatementCompletionUnknown:
			return false
		}
		return isRetryableNode(e.Cause)
	case fdb.Error:
		return fdb.IsRetryable(e.Code) && !isMaybeCommittedFDBCode(e.Code)
	case *fdb.Error:
		return fdb.IsRetryable(e.Code) && !isMaybeCommittedFDBCode(e.Code)
	case *wire.FDBError:
		return fdb.IsRetryable(e.Code) && !isMaybeCommittedFDBCode(e.Code)
	case *recordlayer.RecordCoreRetriableTransactionError:
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, branch := range u.Unwrap() {
			if isRetryableNode(branch) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return isRetryableNode(u.Unwrap())
	}
	return false
}
