package fdb

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// apiVersion is set-once, matching the Apple binding's global-init model
// (fdb_select_api_version_impl). Once set, it can never be changed or unset.
// This is intentional — FDB's API versioning is a process-wide guarantee.
var apiVersion atomic.Int32

// MustAPIVersion is a convenience function equivalent to APIVersion that
// panics on error. Matches Apple's fdb.MustAPIVersion.
func MustAPIVersion(version int) {
	if err := APIVersion(version); err != nil {
		panic(err)
	}
}

// APIVersion must be called before any other fdb function. It selects the
// FDB API version to use. The pure Go client supports a broad range of
// API versions but does not enforce version-specific behavior differences.
func APIVersion(version int) error {
	if version < 510 {
		return Error{Code: 2201} // api_version_not_supported
	}
	v := int32(version)
	// Atomic set-if-unset. Reject re-set with a different version.
	if !apiVersion.CompareAndSwap(0, v) {
		if apiVersion.Load() != v {
			return Error{Code: 2201}
		}
	}
	return nil
}

// GetAPIVersion returns the API version that has been selected, or 0 if
// none has been selected.
func GetAPIVersion() (int, error) {
	v := apiVersion.Load()
	if v == 0 {
		return 0, Error{Code: 2200} // api_version_unset
	}
	return int(v), nil
}

// txDefaults holds database-level transaction option defaults.
// Applied to every new transaction created by Transact/ReadTransact.
// Matches C++ FDB_DB_OPTION_TRANSACTION_* options.
type txDefaults struct {
	timeout        int64 // milliseconds, 0 = disabled
	retryLimit     int64 // -1 = unlimited, 0 = no retries
	hasRetryLimit  bool  // distinguishes "not set" from "set to 0"
	maxRetryDelay  int64 // milliseconds, 0 = use default
	sizeLimit      int64 // bytes, 0 = disabled
	readSystemKeys bool  // allow reading \xff system keys
}

// internalDB wraps client.Database with a context for async operations.
type internalDB struct {
	inner      *client.Database
	ctx        context.Context
	txDefaults txDefaults
}

// Database is a handle to a FoundationDB database.
// Database is safe for concurrent use by multiple goroutines.
type Database struct {
	d *internalDB
}

// Option configures a database opened via OpenDatabase / OpenDatabaseFromConfig
// (e.g. WithTLSConfig, WithDialFunc). It is an alias for client.Option so the
// two packages share one option type.
type Option = client.Option

// WithTLSConfig connects to the cluster over TLS using a standard
// *crypto/tls.Config (in-memory certs, GetClientCertificate rotation, custom
// VerifyPeerCertificate, cipher/version policy). It takes precedence over the
// FDB_TLS_* environment / cluster-file ":tls" resolution and enables TLS even
// when the cluster string lacks ":tls".
func WithTLSConfig(cfg *tls.Config) Option { return client.WithTLSConfig(cfg) }

// WithDialFunc overrides the dialer used for every connection (advanced / tests).
func WithDialFunc(fn client.DialFunc) Option { return client.WithDialFunc(fn) }

// defaultBootstrapTimeout bounds the initial coordinator connection so an
// unreachable cluster fails fast instead of blocking forever (a control-plane
// footgun). It applies to bootstrap only — never to ongoing operations.
const defaultBootstrapTimeout = 60 * time.Second

// bootstrapContext returns the context for the initial coordinator connection. A
// caller-supplied deadline is respected; a deadline-less context (e.g.
// context.Background()) is bounded by defaultBootstrapTimeout so bootstrap can
// never hang forever. The returned cancel must always be called.
func bootstrapContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultBootstrapTimeout)
}

// OpenDatabase opens a connection using the specified cluster file path.
// APIVersion must have been called first. See WithTLSConfig / WithDialFunc.
func OpenDatabase(clusterFile string, opts ...Option) (Database, error) {
	if apiVersion.Load() == 0 {
		return Database{}, Error{Code: 2200} // api_version_unset
	}
	// Bound bootstrap only. The database's long-lived context must NOT be the
	// bootstrap context (which we cancel after connect).
	bootstrapCtx, bootstrapCancel := bootstrapContext(context.Background())
	defer bootstrapCancel()
	db, err := client.OpenDatabase(bootstrapCtx, clusterFile, opts...)
	if err != nil {
		return Database{}, err
	}
	// Long-lived context for the database — not the bootstrap timeout.
	ctx := context.Background()
	return Database{d: &internalDB{inner: db, ctx: ctx}}, nil
}

// OpenWithConnectionString opens a connection using a cluster connection string
// (e.g., "description:id@host1:port1,host2:port2").
func OpenWithConnectionString(connStr string, opts ...Option) (Database, error) {
	cf, err := client.ParseClusterString(connStr)
	if err != nil {
		return Database{}, fmt.Errorf("parse connection string: %w", err)
	}
	return OpenDatabaseFromConfig(context.Background(), cf, opts...)
}

// OpenDatabaseFromConfig creates a Database from a client.ClusterFile.
// The provided ctx is used only for the initial bootstrap (coordinator
// connection); if it has no deadline, bootstrap is bounded by
// defaultBootstrapTimeout so an unreachable cluster fails fast instead of
// hanging forever. The Database uses context.Background() for ongoing operations.
func OpenDatabaseFromConfig(ctx context.Context, cf *client.ClusterFile, opts ...Option) (Database, error) {
	if apiVersion.Load() == 0 {
		return Database{}, Error{Code: 2200} // api_version_unset
	}
	bootstrapCtx, cancel := bootstrapContext(ctx)
	defer cancel()
	db, err := client.OpenDatabaseFromConfig(bootstrapCtx, cf, opts...)
	if err != nil {
		return Database{}, err
	}
	return Database{d: &internalDB{inner: db, ctx: context.Background()}}, nil
}

// WrapDatabase wraps an existing client.Database as an fdb.Database.
// This is useful when code already has a client.Database handle and needs
// to use the fdb facade layer (e.g., for the directory layer).
func WrapDatabase(db *client.Database) Database {
	return Database{d: &internalDB{inner: db, ctx: context.Background()}}
}

// Open opens a database. The dbName parameter is ignored (legacy API compatibility).
func Open(clusterFile string, _ []byte) (Database, error) {
	return OpenDatabase(clusterFile)
}

// MustOpen opens a database or panics. The dbName parameter is ignored.
func MustOpen(clusterFile string, _ []byte) Database {
	return MustOpenDatabase(clusterFile)
}

// OpenDefault opens the database at the default cluster file (/etc/foundationdb/fdb.cluster).
func OpenDefault() (Database, error) {
	return OpenDatabase("/etc/foundationdb/fdb.cluster")
}

// MustOpenDefault opens the default database or panics.
func MustOpenDefault() Database {
	db, err := OpenDefault()
	if err != nil {
		panic(err)
	}
	return db
}

// MustOpenDatabase opens a database or panics.
func MustOpenDatabase(clusterFile string) Database {
	db, err := OpenDatabase(clusterFile)
	if err != nil {
		panic(err)
	}
	return db
}

// Close closes the database connection.
func (db Database) Close() {
	if db.d != nil && db.d.inner != nil {
		db.d.inner.Close()
	}
}

// CreateTransaction creates a new Transaction.
func (db Database) CreateTransaction() (Transaction, error) {
	tx := db.d.inner.CreateTransaction()
	return Transaction{t: &transaction{
		inner:      tx,
		db:         db,
		ctx:        db.d.ctx,
		commitDone: make(chan struct{}),
	}}, nil
}

// Transact runs a transactional function with automatic retry.
func (db Database) Transact(f func(WritableTransaction) (any, error)) (any, error) {
	return db.TransactCtx(db.d.ctx, f)
}

// TransactCtx is Transact bounded by ctx (RFC-090 / fdb.CtxTransactor): ctx bounds the
// retry loop, backoff, and reads. The dispatched commit and its commit_unknown_result
// idempotency barrier run on a detached context (in client.Database.Transact), so the
// caller's ctx never cancels an in-flight commit — which is already bounded by the
// per-RPC timeout.
func (db Database) TransactCtx(ctx context.Context, f func(WritableTransaction) (any, error)) (any, error) {
	var lastTx *transaction // capture for commitDone signaling
	result, err := db.d.inner.Transact(ctx, func(tx *client.Transaction) (r any, e error) {
		defer panicToError(&e)
		t := &transaction{
			inner:      tx,
			db:         db,
			ctx:        ctx,
			commitDone: make(chan struct{}),
		}
		db.applyTxDefaults(t)
		lastTx = t
		r, e = f(Transaction{t: t})
		e = unconvertError(e)
		return
	})
	// Signal commitDone — client.Transact auto-committed after the closure
	// returned. Any GetVersionstamp goroutine blocked on commitDone will unblock.
	if lastTx != nil && lastTx.commitDone != nil {
		select {
		case <-lastTx.commitDone:
		default:
			if err != nil {
				lastTx.commitErr = convertError(err)
			}
			close(lastTx.commitDone)
		}
	}
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// ReadTransact runs a read-only transactional function with automatic retry.
func (db Database) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return db.ReadTransactCtx(db.d.ctx, f)
}

// ReadTransactCtx is ReadTransact bounded by ctx (RFC-090 / fdb.CtxReadTransactor):
// ctx bounds the read-retry loop and backoff.
func (db Database) ReadTransactCtx(ctx context.Context, f func(ReadTransaction) (any, error)) (any, error) {
	// Use a reusable transaction wrapper to avoid per-call allocation.
	// The transaction struct is stack-allocated (doesn't escape because
	// the closure doesn't store it — it only stores the Transaction value
	// which embeds a pointer to t).
	result, err := db.d.inner.ReadTransact(ctx, func(tx *client.Transaction) (r any, e error) {
		defer panicToError(&e)
		t := transaction{
			inner: tx,
			db:    db,
			ctx:   ctx,
		}
		db.applyTxDefaults(&t)
		r, e = f(Transaction{t: &t})
		e = unconvertError(e)
		return
	})
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// applyTxDefaults applies database-level transaction option defaults.
// Matches C++ FDB_DB_OPTION_TRANSACTION_* behavior.
func (db Database) applyTxDefaults(t *transaction) {
	d := &db.d.txDefaults
	if d.timeout > 0 {
		t.inner.SetTimeout(d.timeout)
	}
	if d.hasRetryLimit {
		t.inner.SetRetryLimit(d.retryLimit)
	}
	if d.maxRetryDelay > 0 {
		t.inner.SetMaxRetryDelay(d.maxRetryDelay)
	}
	if d.sizeLimit > 0 {
		t.inner.SetSizeLimit(d.sizeLimit)
	}
	if d.readSystemKeys {
		t.inner.SetReadSystemKeys()
	}
}

// Options returns a DatabaseOptions handle for setting database-level defaults.
func (db Database) Options() DatabaseOptions {
	return DatabaseOptions{db: db.d}
}

// SetHedgeEnabled controls speculative second requests (hedge) for read RPCs.
// When enabled (default), slow reads are rescued by sending a backup request
// to a second server after max(10ms, 2×latency).
func (db Database) SetHedgeEnabled(enabled bool) {
	db.d.inner.SetHedgeEnabled(enabled)
}

// HedgeEnabled returns whether speculative second requests are active.
func (db Database) HedgeEnabled() bool {
	return db.d.inner.HedgeEnabled()
}

// InvalidateGRVCache forces the next transaction to fetch a fresh read version
// from the GRV proxy instead of using the cached version. Use after external
// writes (e.g., from a Java conformance server) to ensure Go reads see them.
func (db Database) InvalidateGRVCache() {
	db.d.inner.InvalidateGRVCache()
}

// Tenant operations.

// OpenTenant opens a tenant by name. Reads the tenant ID from the system
// key name index (\xff/tenant/nameIndex/<name>).
func (db Database) OpenTenant(name KeyConvertible) (Tenant, error) {
	tenantName := name.FDBKey()
	if len(tenantName) == 0 || tenantName[0] == 0xff {
		return Tenant{}, errTenantInvalid
	}
	var tenantId int64
	_, err := db.Transact(func(trw WritableTransaction) (any, error) {
		tr := trw.(Transaction) // tenant ops are pure-Go only (out of RFC-109 escape-hatch scope)
		tr.Options().SetReadSystemKeys()
		tr.Options().SetReadLockAware() // C++ tryGetTenant: READ_SYSTEM_KEYS + READ_LOCK_AWARE
		var err error
		tenantId, err = openTenantInternal(tr, tenantName)
		return nil, err
	})
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{db: db, tenantId: tenantId}, nil
}

// OpenTenantById opens a tenant by its numeric ID. All operations on the
// returned Tenant are scoped to the tenant's key space. The caller is
// responsible for ensuring the tenant ID is valid.
func (db Database) OpenTenantById(id int64) Tenant {
	return Tenant{db: db, tenantId: id}
}

// CreateTenant creates a new tenant. Writes to system keys (\xff/tenant/*)
// matching C++ TenantAPI::createTenantTransaction.
func (db Database) CreateTenant(name KeyConvertible) error {
	_, err := db.Transact(func(trw WritableTransaction) (any, error) {
		tr := trw.(Transaction) // tenant ops are pure-Go only (out of RFC-109 escape-hatch scope)
		tr.Options().SetAccessSystemKeys()
		tr.Options().SetLockAware() // C++ createTenant: ACCESS_SYSTEM_KEYS + LOCK_AWARE
		_, err := createTenantInternal(tr, name.FDBKey())
		return nil, err
	})
	return err
}

// DeleteTenant deletes a tenant. Writes to system keys (\xff/tenant/*).
func (db Database) DeleteTenant(name KeyConvertible) error {
	_, err := db.Transact(func(trw WritableTransaction) (any, error) {
		tr := trw.(Transaction) // tenant ops are pure-Go only (out of RFC-109 escape-hatch scope)
		tr.Options().SetAccessSystemKeys()
		tr.Options().SetLockAware() // C++ deleteTenant: ACCESS_SYSTEM_KEYS + LOCK_AWARE
		return nil, deleteTenantInternal(tr, name.FDBKey())
	})
	return err
}

// ListTenants lists all tenants by scanning the name index.
func (db Database) ListTenants() ([]Key, error) {
	result, err := db.Transact(func(trw WritableTransaction) (any, error) {
		tr := trw.(Transaction) // tenant ops are pure-Go only (out of RFC-109 escape-hatch scope)
		tr.Options().SetReadSystemKeys()
		tr.Options().SetLockAware() // C++ listTenants: READ_SYSTEM_KEYS + LOCK_AWARE
		return listTenantsInternal(tr)
	})
	if err != nil {
		return nil, err
	}
	return result.([]Key), nil
}

// GetClientStatus returns a JSON blob with client connection status.
// Provides basic connectivity info — not the full FDB status JSON.
func (db Database) GetClientStatus() ([]byte, error) {
	info := db.d.inner.GetDBInfo()
	if info == nil {
		return []byte(`{"connected":false}`), nil
	}
	return fmt.Appendf(nil,
		`{"connected":true,"grv_proxies":%d,"commit_proxies":%d}`,
		len(info.GRVProxies), len(info.CommitProxies),
	), nil
}

// LocalityGetBoundaryKeys returns shard boundary keys within the given range.
// Uses the location cache to find shard boundaries. The limit and readVersion
// parameters match the Apple binding signature but are advisory.
func (db Database) LocalityGetBoundaryKeys(r ExactRange, limit int, _ int64) ([]Key, error) {
	begin, end := r.FDBRangeKeys()
	ctx := db.d.ctx
	tx := db.d.inner.CreateTransaction()
	locs, err := tx.GetLocations(ctx, begin.FDBKey(), end.FDBKey(), limit)
	if err != nil {
		return nil, err
	}
	keys := make([]Key, 0, len(locs)+1)
	for _, loc := range locs {
		keys = append(keys, Key(loc.ShardBegin))
	}
	return keys, nil
}

// RebootWorker is not yet implemented.
func (db Database) RebootWorker(_ string, _ bool, _ int) error {
	return errNotSupported
}
