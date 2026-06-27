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
//	db, err := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/etc/foundationdb/fdb.cluster")
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

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdbclient"
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

// defaultClusterFilePath is FDB's conventional cluster-file location, used when
// neither the DSN nor FDB_CLUSTER_FILE specifies one. It mirrors the path the
// pure-Go client's OpenDefault uses, and is the standard default for libfdb_c too.
const defaultClusterFilePath = "/etc/foundationdb/fdb.cluster"

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
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	dsn, err := ParseDSN(name)
	if err != nil {
		return nil, err
	}
	return &Connector{driver: d, dsn: dsn}, nil
}

// Connector holds a parsed DSN and produces connections on demand.
// The FDB database, keyspace, and factory are initialised lazily on
// the first Connect call. The catalog Bootstrap (Initialize) is deferred
// further — it runs inside the first DDL transaction, not at Connect time.
type Connector struct {
	driver *Driver
	dsn    *DSN

	once    sync.Once
	fdbDB   *recordlayer.FDBDatabase
	cat     *catalog.RecordLayerStoreCatalog
	ks      *keyspace.RelationalKeyspace
	factory *ddl.RecordLayerMetadataOperationsFactory
	initErr error
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
	if c.dsn.Schema != "" {
		conn.SetDefaultSchema(c.dsn.Schema)
	}
	return conn, nil
}

// initialize opens FDB and wires catalog + factory. The catalog Bootstrap
// (Initialize) is deferred — it runs on the first DDL transaction, not here.
func (c *Connector) initialize(_ context.Context) error {
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
		// Open through the build-tag-selectable seam (RFC-109): the default build
		// uses the pure-Go client; -tags libfdbc swaps in Apple's libfdb_c with no
		// source change here. fdbclient.Open needs an explicit path, so an empty
		// cluster file falls back to FDB's default location — matching the prior
		// OpenDefault() branch — and works the same for both backends.
		openPath := clusterFile
		if openPath == "" {
			openPath = defaultClusterFilePath
		}
		rawDB, err := fdbclient.Open(openPath)
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

	// root subspace is the empty subspace — all catalog and schema data lives
	// under well-known tuple prefixes via RelationalKeyspace.
	c.ks = keyspace.New(subspace.Sub())
	cat, catErr := catalog.NewRecordLayerStoreCatalog(c.ks.CatalogSubspace())
	if catErr != nil {
		return catErr
	}
	c.cat = cat
	c.factory = ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, c.ks)
	return nil
}

// Driver returns the driver that created this Connector.
func (c *Connector) Driver() driver.Driver { return c.driver }

// DSN returns the parsed DSN. Exposed for diagnostics.
func (c *Connector) DSN() *DSN { return c.dsn }

// Static interface checks.
var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
	_ driver.Connector     = (*Connector)(nil)
)

func init() {
	sql.Register(DriverName, &Driver{})
}
