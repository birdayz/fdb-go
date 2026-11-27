package fdb

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbgo/client"
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
// FDB API version to use: client.MinAPIVersion through client.MaxAPIVersion
// (510-730). Re-selecting the same version is a no-op; any other version once
// one is selected fails with api_version_already_set (2201), and an
// unsupported version with api_version_not_supported (2203).
func APIVersion(version int) error {
	return selectAPIVersion(&apiVersion, version)
}

// selectAPIVersion is APIVersion over explicit state. The already-set check
// precedes the range check, as in the Apple binding's APIVersion.
func selectAPIVersion(cur *atomic.Int32, version int) error {
	if v := cur.Load(); v != 0 {
		if int(v) == version {
			return nil
		}
		return Error{Code: 2201} // api_version_already_set
	}
	if version < client.MinAPIVersion || version > client.MaxAPIVersion {
		return Error{Code: 2203} // api_version_not_supported
	}
	if !cur.CompareAndSwap(0, int32(version)) && int(cur.Load()) != version {
		return Error{Code: 2201}
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

// IsAPIVersionSelected reports whether an API version has been selected.
// Mirrors the Apple binding's fdb.IsAPIVersionSelected so callers can branch
// on it without catching the api_version_unset error.
func IsAPIVersionSelected() bool {
	return apiVersion.Load() != 0
}

// MustGetAPIVersion returns the selected API version, panicking if none has
// been selected. Mirrors the Apple binding's fdb.MustGetAPIVersion.
func MustGetAPIVersion() int {
	v, err := GetAPIVersion()
	if err != nil {
		panic(err)
	}
	return v
}

// txDefaults snapshots only the facade-owned fields below, not native conflict-reporting defaults.
// OnError replays non-persistent fields without overwriting successfully applied persistent options.
type txDefaults struct {
	timeout        int64 // milliseconds, 0 = disabled
	retryLimit     int64 // -1 = unlimited, 0 = no retries
	hasRetryLimit  bool
	maxRetryDelay  int64 // milliseconds, 0 = use default
	sizeLimit      int64 // bytes, 0 = disabled
	readSystemKeys bool  // allow reading \xff system keys
	// snapshotRywDisableNet is the NET (disables - enables) of the DB-level snapshot-RYW option.
	// libfdb_c's DatabaseContext keeps a cumulative SIGNED counter (int snapshotRywEnabled,
	// DatabaseContext.h:681; ++/-- at NativeAPI.actor.cpp:2156/2160 and
	// ReadYourWrites.actor.cpp:2591/2596) that each new tx is seeded from
	// (ReadYourWrites.actor.cpp:2082) and which is tested as `<= 0` (:402). The threshold is
	// signed and the counter may legitimately go NEGATIVE, so neither a bool nor a
	// clamp-at-zero can model it: SetSnapshotRywEnable();SetSnapshotRywDisable() must net to 0
	// (still enabled). applyTxDefaults replays this net onto the tx's own disable counter.
	snapshotRywDisableNet int
	bypassUnreadable      bool // DB default bypasses accessed_unreadable on each new tx (set-once flag)
	causalReadRisky       bool // DB default sets the GRV causal-read-risky flag on each new tx (set-once flag)
}

// internalDB wraps client.Database with a context for async operations.
type internalDB struct {
	inner *client.Database
	ctx   context.Context

	// txDefaults is an immutable snapshot of facade-owned fields, published atomically.
	// Native-owned conflict reporting is separate and can observe another default generation.
	//
	// This is NOT thread confinement: in C++ (7.3.77) both fdb_database_set_option
	// and fdb_database_create_transaction run on the CALLER's thread. The C shim
	// passes straight through (bindings/c/fdb_c.cpp:611-617, :631-635);
	// ThreadSafeDatabase::setOption validates and mutates isConfigDB caller-side and
	// only DEFERS the DatabaseContext::setOption body via onMainThreadVoid
	// (ThreadSafeTransaction.cpp:63-85), and createTransaction is entirely caller-side
	// (:58-61).
	//
	// Coherence comes from a narrower mechanism, and it is the one worth porting:
	// the deferred bodies are queued SAME-PRIORITY FIFO onto the network thread (the
	// no-ordering warning at ThreadHelper.actor.h:69-70 is scoped to DIFFERING
	// priorities; ThreadSafeTransaction.cpp:369 states the ordering guarantee), and
	// the whole default list is bulk-copied in ONE non-yielding callback — the RYW
	// constructor's std::copy of getTransactionDefaults()
	// (ReadYourWrites.actor.cpp:1550-1557), which is not an ACTOR and so cannot yield
	// mid-body. A transaction therefore sees the option list as it stood at one
	// instant.
	//
	// Go has no equivalent: any goroutine may set a DB option while another starts a
	// transaction. Publishing a whole snapshot (rather than per-field atomics) is what
	// reproduces that bulk-copy property — a transaction must not pick up the new
	// timeout alongside the old size limit. Readers take one Load; they never hold
	// a lock across the callouts into the client that apply the options.
	//
	// Note this is a property of the single-version path only. C++'s multi-version
	// client is itself racy here: MultiVersionDatabase::createTransaction copies
	// dbState->transactionDefaultOptions BY VALUE with optionLock NOT held
	// (MultiVersionTransaction.actor.cpp:2122-2126) while setOption mutates it under
	// that lock (:2129, :2143). Go implements no multi-version client, so there is
	// nothing to reproduce or work around — but "libfdb_c is immune" is not a claim
	// this code can rest on.
	//
	// nil means "nothing ever set" and reads as the zero txDefaults.
	txDefaults atomic.Pointer[txDefaults]
	// txDefaultsMu serializes the read-modify-write in the setters (the snapshot-RYW
	// net counter is cumulative, so last-write-wins would lose increments).
	txDefaultsMu sync.Mutex
}

// defaults returns the current immutable snapshot of facade-owned transaction defaults.
// Native-owned conflict reporting is not part of this snapshot.
func (d *internalDB) defaults() txDefaults {
	if p := d.txDefaults.Load(); p != nil {
		return *p
	}
	return txDefaults{}
}

// mutateDefaults applies fn to a COPY of the current defaults and publishes the
// result atomically, so a concurrent defaults() reader sees either the whole old
// snapshot or the whole new one — never a half-applied option.
func (d *internalDB) mutateDefaults(fn func(*txDefaults)) {
	d.txDefaultsMu.Lock()
	defer d.txDefaultsMu.Unlock()
	next := d.defaults()
	fn(&next)
	d.txDefaults.Store(&next)
}

// Database is a handle to a FoundationDB database.
// Database is safe for concurrent use by multiple goroutines.
type Database struct {
	d *internalDB
}

// DialFunc overrides the dialer used for every connection (matches net.Dialer.DialContext).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Option configures a database opened via OpenDatabase / OpenWithConnectionString /
// OpenDatabaseFromConfig (e.g. WithTLSConfig, WithDialFunc).
type Option func(*openOptions)

type openOptions struct {
	clientOpts []client.Option
}

func toClientOptions(opts []Option, apiVer int) []client.Option {
	var oo openOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&oo)
		}
	}
	// Thread the selected API version into the client (RFC-149).
	oo.clientOpts = append(oo.clientOpts, client.WithAPIVersion(apiVer))
	return oo.clientOpts
}

// WithTLSConfig connects to the cluster over TLS using a standard
// *crypto/tls.Config (in-memory certs, GetClientCertificate rotation, custom
// VerifyPeerCertificate, cipher/version policy). It takes precedence over the
// FDB_TLS_* environment / cluster-file ":tls" resolution and enables TLS even
// when the cluster string lacks ":tls". It keeps Go's verification, including the
// per-dial host-name check libfdb_c does not make.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithTLSConfig(cfg))
	}
}

// WithDialFunc overrides the dialer used for every connection (advanced / tests).
func WithDialFunc(fn DialFunc) Option {
	return func(o *openOptions) {
		var cfn client.DialFunc
		if fn != nil {
			cfn = client.DialFunc(fn)
		}
		o.clientOpts = append(o.clientOpts, client.WithDialFunc(cfn))
	}
}

// WithCoalesceDelay sets each connection's timer-based batch delay bounds (default
// 0, 0 on D-NG; 10/20 µs on WithLegacyWriter(); the last linger/coalesce option wins).
func WithCoalesceDelay(minDelay, maxDelay time.Duration) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithCoalesceDelay(minDelay, maxDelay))
	}
}

// WithAdaptiveLinger configures Nagle-style adaptive linger on the D-NG writer
// (0 disables; the last linger/coalesce option wins; RFC-282).
func WithAdaptiveLinger(d time.Duration) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithAdaptiveLinger(d))
	}
}

// WithRangeByteCeiling bounds how many bytes a single GetRange may materialize
// before it fails with a *RangeMaterializationLimitError, instead of OOM-ing
// on a runaway unbounded scan. n ≤ 0 (the default) is UNLIMITED, matching libfdb_c.
// An opt-in OOM safety valve; for large result sets prefer the bounded Iterator()
// (which honors StreamingMode). RFC-115 §2.
func WithRangeByteCeiling(n int64) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithRangeByteCeiling(n))
	}
}

// WithTracingSampleRate sets the fraction (0.0–1.0) of transactions whose trace span
// is flagged sampled. Default 0.0 matches C++ TRACING_SAMPLE_RATE — every transaction
// still carries a real (randomly-generated, wire-faithful) SpanContext, flagged
// unsampled. RFC-115 §4.
func WithTracingSampleRate(rate float64) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithTracingSampleRate(rate))
	}
}

// Tracer is an OpenTelemetry tracer used by [WithTracer] and [Config.Tracer].
type Tracer = oteltrace.Tracer

// WithTracer sets the application-owned OpenTelemetry tracer for client spans.
// WithTracingSampleRate gates sampling before the provider's sampler; nil
// disables export without constructing OTel contexts or spans. Managed Transact
// and ReadTransact helpers end spans on every exit, whereas raw
// CreateTransaction handles require commit, OnError, Reset, or Cancel (always
// Cancel or Reset uncommitted handles). Tracer and span-processor callbacks run
// synchronously and must not call transaction methods (including Reset/Cancel);
// the application owns provider flushing and shutdown (Database.Close does not
// shut it down). See also [client.WithTracer] for sampling, span ownership, and
// callback restrictions.
func WithTracer(t oteltrace.Tracer) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithTracer(t))
	}
}

// WithLogger sets the per-handle logger for operational events (transaction
// retries, commit_unknown_result — RFC-097). nil (the default) uses
// slog.Default(), so zero-config apps keep slog.SetDefault; the per-handle
// option exists for multi-tenant hosts and tests.
func WithLogger(l *slog.Logger) Option {
	return func(o *openOptions) {
		o.clientOpts = append(o.clientOpts, client.WithLogger(l))
	}
}

// ClusterFile represents a parsed FoundationDB cluster file or connection string.
// Format: "<description>:<id>@<host>:<port>[,<host>:<port>...]".
type ClusterFile struct {
	Description  string
	ID           string
	Coordinators []string // "host:port" addresses for TCP connection (":tls" suffix stripped)
	UseTLS       bool     // true when coordinators carry the ":tls" suffix
}

// String renders the canonical cluster connection string.
func (cf *ClusterFile) String() string {
	if cf == nil {
		return ""
	}
	return toClientClusterFile(cf).String()
}

// ParseClusterFile reads and parses an fdb.cluster file.
func ParseClusterFile(path string) (*ClusterFile, error) {
	cf, err := client.ParseClusterFile(path)
	if err != nil {
		return nil, err
	}
	return fromClientClusterFile(cf), nil
}

// ParseClusterString parses a cluster connection string.
func ParseClusterString(s string) (*ClusterFile, error) {
	cf, err := client.ParseClusterString(s)
	if err != nil {
		return nil, err
	}
	return fromClientClusterFile(cf), nil
}

func fromClientClusterFile(cf *client.ClusterFile) *ClusterFile {
	if cf == nil {
		return nil
	}
	return &ClusterFile{
		Description:  cf.Description,
		ID:           cf.ID,
		Coordinators: slices.Clone(cf.Coordinators),
		UseTLS:       cf.UseTLS,
	}
}

func toClientClusterFile(cf *ClusterFile) *client.ClusterFile {
	if cf == nil {
		return nil
	}
	return &client.ClusterFile{
		Description:  cf.Description,
		ID:           cf.ID,
		Coordinators: slices.Clone(cf.Coordinators),
		UseTLS:       cf.UseTLS,
	}
}

// defaultBootstrapTimeout bounds the initial coordinator connection so an
// unreachable cluster fails fast instead of blocking forever (a control-plane
// footgun). It applies to bootstrap only — never to ongoing operations.
const defaultBootstrapTimeout = 60 * time.Second

// bootstrapContext returns the context for the initial coordinator connection. A
// caller-supplied deadline is respected; a deadline-less context (e.g.
// context.Background()) is bounded by defaultBootstrapTimeout so bootstrap can
// never hang forever. The returned cancel must always be called.
func bootstrapContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultBootstrapTimeout)
}

// OpenDatabase opens a connection using the specified cluster file path.
// APIVersion must have been called first. See WithTLSConfig / WithDialFunc.
//
// Bootstrap is bounded by an internal default timeout (bootstrapContext) — a place
// this client is STRICTER than libfdb_c, which waits indefinitely for the initial
// coordinator connection. Past open there is no internal bound, matching libfdb_c's
// per-transaction defaults (timeoutInSeconds=0.0, maxRetries=-1;
// ReadYourWrites.actor.cpp:2078-2082): reconnection and the transaction retry loop
// run until the cluster returns. So against a permanently down cluster a subsequent
// bare Transact (which uses context.Background()) does not return on its own — bound
// it with TransactCtx, SetTimeout, or SetRetryLimit, as you would have had to in
// libfdb_c. See the package doc at fdb.dev/pkg/fdbgo/fdb. Use OpenDatabaseFromConfig to
// pass a context for bootstrap itself.
func OpenDatabase(clusterFile string, opts ...Option) (Database, error) {
	return openDatabaseWithContext(context.Background(), clusterFile, opts...)
}

// openDatabaseWithContext opens a Database using clusterFile and bounds the
// initial coordinator bootstrap with ctx (or defaultBootstrapTimeout if ctx has
// no deadline). The returned Database uses context.Background() for ongoing
// operations.
func openDatabaseWithContext(ctx context.Context, clusterFile string, opts ...Option) (Database, error) {
	if apiVersion.Load() == 0 {
		return Database{}, Error{Code: 2200} // api_version_unset
	}
	// Bound bootstrap only. The database's long-lived context must NOT be the
	// bootstrap context (which we cancel after connect).
	bootstrapCtx, bootstrapCancel := bootstrapContext(ctx)
	defer bootstrapCancel()
	// Thread the selected API version into the client (RFC-149); the unset check
	// above guarantees it is non-zero, so the client's mandatory-set gate passes.
	db, err := client.OpenDatabase(bootstrapCtx, clusterFile, toClientOptions(opts, int(apiVersion.Load()))...)
	if err != nil {
		return Database{}, convertError(err)
	}
	// Long-lived context for the database — not the bootstrap timeout.
	return Database{d: &internalDB{inner: db, ctx: context.Background()}}, nil
}

// OpenWithConnectionString opens a connection using a cluster connection string
// (e.g., "description:id@host1:port1,host2:port2").
func OpenWithConnectionString(connStr string, opts ...Option) (Database, error) {
	cf, err := ParseClusterString(connStr)
	if err != nil {
		return Database{}, fmt.Errorf("parse connection string: %w", err)
	}
	return OpenDatabaseFromConfig(context.Background(), cf, opts...)
}

// OpenDatabaseFromConfig creates a Database from a ClusterFile.
// The provided ctx is used only for the initial bootstrap (coordinator
// connection); if it has no deadline, bootstrap is bounded by
// defaultBootstrapTimeout so an unreachable cluster fails fast instead of
// hanging forever. The Database uses context.Background() for ongoing operations.
func OpenDatabaseFromConfig(ctx context.Context, cf *ClusterFile, opts ...Option) (Database, error) {
	if apiVersion.Load() == 0 {
		return Database{}, Error{Code: 2200} // api_version_unset
	}
	bootstrapCtx, cancel := bootstrapContext(ctx)
	defer cancel()
	// Thread the selected API version into the client (RFC-149); the unset check
	// above guarantees it is non-zero, so the client's mandatory-set gate passes.
	db, err := client.OpenDatabaseFromConfig(bootstrapCtx, toClientClusterFile(cf), toClientOptions(opts, int(apiVersion.Load()))...)
	if err != nil {
		return Database{}, convertError(err)
	}
	return Database{d: &internalDB{inner: db, ctx: context.Background()}}, nil
}

// WrapDatabase wraps an existing client.Database as an fdb.Database.
// This is useful when code already has a client.Database handle and needs
// to use the fdb facade layer (e.g., for the directory layer).
func WrapDatabase(db *client.Database) Database {
	return WrapDatabaseContext(context.Background(), db)
}

// WrapDatabaseContext supplies the inherited ctx for facade reads, retry loops and snapshots.
// Explicit *Ctx calls override ctx; cancellation does not end an already-dispatched commit's wait.
func WrapDatabaseContext(ctx context.Context, db *client.Database) Database {
	return Database{d: &internalDB{inner: db, ctx: ctx}}
}

// MustOpen opens a database or panics. The dbName parameter is ignored.
func MustOpen(clusterFile string, _ []byte) Database {
	return MustOpenDatabase(clusterFile)
}

// OpenDefault opens the database of the default cluster file: FDB_CLUSTER_FILE
// when set, else ./fdb.cluster when it exists, else the platform default
// (/etc/foundationdb/fdb.cluster on Linux), as libfdb_c resolves it
// (client.LookupClusterFileName). Like Apple's binding, it is OpenDatabase("").
func OpenDefault() (Database, error) {
	return OpenDatabase("")
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

// LatencyStats is a point-in-time summary of one latency distribution in seconds.
type LatencyStats struct {
	Count  int64
	Sum    float64
	Mean   float64
	Median float64
	P90    float64
	P99    float64
	Min    float64
	Max    float64
}

// MetricsSnapshot is a point-in-time copy of the database's operational
// counters and latency distributions.
type MetricsSnapshot struct {
	TransactionsCommitStarted   int64
	TransactionsCommitCompleted int64

	TransactionsNotCommitted        int64
	TransactionsMaybeCommitted      int64
	TransactionsResourceConstrained int64
	TransactionsProcessBehind       int64
	TransactionsThrottled           int64
	TransactionsTooOld              int64
	TransactionsFutureVersions      int64

	TransactionReadVersionsCompleted          int64
	TransactionBatchReadVersionsCompleted     int64
	TransactionDefaultReadVersionsCompleted   int64
	TransactionImmediateReadVersionsCompleted int64
	GRVCacheHits                              int64
	GRVInBandMaybeDelivered                   int64

	TransactionRetries int64

	TransactionGetRangeRequests int64

	RecoveredPanics               int64
	RecoveredPanicsConsecutiveMax int64

	ClientConnectionFailures int64
	CoordinatorChanges       int64

	ReadLatency        LatencyStats
	CommitLatency      LatencyStats
	GRVLatency         LatencyStats
	TransactionLatency LatencyStats
}

func fromClientLatencyStats(s client.LatencyStats) LatencyStats {
	return LatencyStats{
		Count:  s.Count,
		Sum:    s.Sum,
		Mean:   s.Mean,
		Median: s.Median,
		P90:    s.P90,
		P99:    s.P99,
		Min:    s.Min,
		Max:    s.Max,
	}
}

func fromClientMetricsSnapshot(s client.ClientMetricsSnapshot) MetricsSnapshot {
	return MetricsSnapshot{
		TransactionsCommitStarted:                 s.TransactionsCommitStarted,
		TransactionsCommitCompleted:               s.TransactionsCommitCompleted,
		TransactionsNotCommitted:                  s.TransactionsNotCommitted,
		TransactionsMaybeCommitted:                s.TransactionsMaybeCommitted,
		TransactionsResourceConstrained:           s.TransactionsResourceConstrained,
		TransactionsProcessBehind:                 s.TransactionsProcessBehind,
		TransactionsThrottled:                     s.TransactionsThrottled,
		TransactionsTooOld:                        s.TransactionsTooOld,
		TransactionsFutureVersions:                s.TransactionsFutureVersions,
		TransactionReadVersionsCompleted:          s.TransactionReadVersionsCompleted,
		TransactionBatchReadVersionsCompleted:     s.TransactionBatchReadVersionsCompleted,
		TransactionDefaultReadVersionsCompleted:   s.TransactionDefaultReadVersionsCompleted,
		TransactionImmediateReadVersionsCompleted: s.TransactionImmediateReadVersionsCompleted,
		GRVCacheHits:                              s.GRVCacheHits,
		GRVInBandMaybeDelivered:                   s.GRVInBandMaybeDelivered,
		TransactionRetries:                        s.TransactionRetries,
		TransactionGetRangeRequests:               s.TransactionGetRangeRequests,
		RecoveredPanics:                           s.RecoveredPanics,
		RecoveredPanicsConsecutiveMax:             s.RecoveredPanicsConsecutiveMax,
		ClientConnectionFailures:                  s.ClientConnectionFailures,
		CoordinatorChanges:                        s.CoordinatorChanges,
		ReadLatency:                               fromClientLatencyStats(s.ReadLatency),
		CommitLatency:                             fromClientLatencyStats(s.CommitLatency),
		GRVLatency:                                fromClientLatencyStats(s.GRVLatency),
		TransactionLatency:                        fromClientLatencyStats(s.TransactionLatency),
	}
}

// Metrics returns the database's operational counters and latency distributions,
// so a facade Database can be passed to fdbmetrics.Handler.
func (db Database) Metrics() MetricsSnapshot {
	if db.d == nil || db.d.inner == nil {
		return MetricsSnapshot{}
	}
	return fromClientMetricsSnapshot(db.d.inner.Metrics())
}

// Close closes the database connection, satisfying [io.Closer].
func (db Database) Close() error {
	if db.d != nil && db.d.inner != nil {
		return db.d.inner.Close()
	}
	return nil
}

// CreateTransaction copies database option defaults; reads inherit context.Background() for ordinarily
// opened databases, or WrapDatabaseContext's ctx. TransactCtx/ReadTransactCtx provide explicit bounds.
func (db Database) CreateTransaction() (Transaction, error) {
	tx := db.d.inner.CreateTransaction()
	t := &transaction{
		inner:         tx,
		db:            db,
		ctx:           db.d.ctx,
		versionstamps: true,
	}
	// Apply DB-level option defaults to manually-created transactions too, exactly
	// like the Transact* paths. C++ copies the database transaction defaults into
	// every transaction at construction (ReadYourWrites.actor.cpp); skipping this
	// left a manual CreateTransaction/OnError loop unbounded even when the caller
	// had set SetTransactionTimeout/SetTransactionRetryLimit on the database.
	db.applyTxDefaults(t, true)
	return Transaction{t: t}, nil
}

// CreateWritableTransaction satisfies fdb.BackendDatabase: the standalone-
// transaction creator returning the WritableTransaction interface. On the pure-Go
// client it is exactly CreateTransaction (the concrete Transaction already
// implements WritableTransaction); it exists so the record layer can create a
// standalone transaction on ANY backend (the libfdb_c backend implements the same
// method over cgofdb).
func (db Database) CreateWritableTransaction() (WritableTransaction, error) {
	return db.CreateTransaction()
}

// Transact retries using context.Background() for ordinarily opened databases, or WrapDatabaseContext's ctx.
// Retries default to unbounded; use timeout/retry-limit options or TransactCtx to supply explicit bounds.
func (db Database) Transact(f func(WritableTransaction) (any, error)) (any, error) {
	return db.TransactCtx(db.d.ctx, f)
}

// TransactCtx uses ctx to bound the retry loop, backoff, and reads (RFC-090).
// A dispatched commit and its uncertainty barrier outlive ctx, which, unlike
// fdb_future_cancel in libfdb_c, does not end the wait; a transaction timeout or
// Cancel does, as in libfdb_c (RFC-280).
func (db Database) TransactCtx(ctx context.Context, f func(WritableTransaction) (any, error)) (any, error) {
	var body bodyRun
	var defaults retryTxDefaults
	result, err := db.d.inner.Transact(ctx, func(tx *client.Transaction) (any, error) {
		return body.run(func() (any, error) {
			t := &transaction{
				inner:         tx,
				db:            db,
				ctx:           ctx,
				versionstamps: true,
				defaults:      &defaults,
			}
			if err := defaults.apply(db, t); err != nil {
				return nil, err
			}
			return f(Transaction{t: t})
		})
	})
	if err != nil {
		return nil, body.keepBodyError(err)
	}
	return result, nil
}

// ReadTransact retries read-only work using context.Background() for ordinarily opened databases,
// or WrapDatabaseContext's inherited ctx. Like Transact, timeout/retry-limit options can bound retries.
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
	var body bodyRun
	var defaults retryTxDefaults
	result, err := db.d.inner.ReadTransact(ctx, func(tx *client.Transaction) (any, error) {
		return body.run(func() (any, error) {
			t := transaction{
				inner:    tx,
				db:       db,
				ctx:      ctx,
				defaults: &defaults,
			}
			if err := defaults.apply(db, &t); err != nil {
				return nil, err
			}
			return f(Transaction{t: &t})
		})
	})
	if err != nil {
		return nil, body.keepBodyError(err)
	}
	return result, nil
}

// retryTxDefaults retains failed or skipped inherited timeouts, as C++ reapplies its option list.
// Explicit Reset refreshes both states; database updates alone do not change the captured value.
type retryTxDefaults struct {
	applied        bool
	err            error
	pendingTimeout int64
}

func (d *retryTxDefaults) apply(db Database, t *transaction) error {
	if d.err != nil {
		return convertError(d.err)
	}
	if !d.applied {
		d.err, d.pendingTimeout = db.applyTxDefaults(t, true)
		d.applied = true
		// Initial option failures remain deferred; reject reentry if OnError cleared them.
		return nil
	}
	if d.pendingTimeout > 0 {
		d.err, d.pendingTimeout = applyTimeoutDefault(t, d.pendingTimeout)
		if d.err != nil {
			return convertError(d.err)
		}
	}
	db.applyTxDefaults(t, false)
	return nil
}

// applyTxDefaults captures only facade fields; its results distinguish failed and skipped timeouts.
// Retry callbacks retain installed overrides and reattempt only the skipped timeout value.
func (db Database) applyTxDefaults(t *transaction, persistent bool) (error, int64) {
	return db.d.defaults().apply(t, persistent)
}

// applyTimeoutDefault distinguishes a failed timeout from an option skipped on an already-poisoned handle.
// Both verdict queries read only local counters; pending values retain their original snapshot.
func applyTimeoutDefault(t *transaction, timeout int64) (error, int64) {
	if _, err := t.inner.GetApproximateSize(); err != nil {
		return nil, timeout
	}
	t.inner.SetTimeout(timeout)
	_, err := t.inner.GetApproximateSize()
	return err, 0
}

func (d txDefaults) apply(t *transaction, persistent bool) (error, int64) {
	var persistentErr error
	var pendingTimeout int64
	if persistent {
		if d.timeout > 0 {
			persistentErr, pendingTimeout = applyTimeoutDefault(t, d.timeout)
		}
		if d.hasRetryLimit {
			t.inner.SetRetryLimit(d.retryLimit)
		}
		if d.maxRetryDelay > 0 {
			t.inner.SetMaxRetryDelay(d.maxRetryDelay)
		}
	}
	if d.sizeLimit > 0 {
		t.inner.SetSizeLimit(d.sizeLimit)
	}
	if d.readSystemKeys {
		t.inner.SetReadSystemKeys()
	}
	// Assign the DB's net counter, not another enable/disable increment on each retry.
	// C++ likewise seeds snapshotRywEnabled from the DB (ReadYourWrites.actor.cpp:2082).
	t.inner.SetSnapshotRYWDisableCount(d.snapshotRywDisableNet)
	if d.bypassUnreadable {
		t.inner.SetBypassUnreadable(true)
	}
	if d.causalReadRisky {
		t.inner.SetCausalReadRisky(true)
	}
	return persistentErr, pendingTimeout
}

// Options returns a DatabaseOptions handle for setting database-level defaults.
func (db Database) Options() DatabaseOptions {
	return DatabaseOptions{db: db.d}
}

// SetHedgeEnabled enables budget-funded speculative read requests (default on).
// Disabled, no read hedges; lone replies still accrue budget (RFC-281).
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
	return fmt.Appendf(
		nil,
		`{"connected":true,"grv_proxies":%d,"commit_proxies":%d}`,
		len(info.GRVProxies), len(info.CommitProxies),
	), nil
}

// LocalityGetBoundaryKeys returns shard boundary keys within the given range.
//
// It honors readVersion (RFC-111 P1.6): boundary keys live in the
// \xFF/keyServers/<key> system range, and reading that range AT the supplied read
// version yields MVCC-consistent boundaries — pinning the result to a snapshot
// rather than returning whatever the location cache holds now. readVersion == 0
// means "use a fresh read version" (a normal GRV). This is a 1:1 port of the Apple
// binding's LocalityGetBoundaryKeys.
func (db Database) LocalityGetBoundaryKeys(r ExactRange, limit int, readVersion int64) ([]Key, error) {
	tr, err := db.CreateTransaction()
	if err != nil {
		return nil, err
	}
	if readVersion != 0 {
		tr.SetReadVersion(readVersion)
	}
	// System keys + lock-aware so a locked database still serves the boundary read,
	// matching the Apple binding.
	if err := tr.Options().SetReadSystemKeys(); err != nil {
		return nil, err
	}
	if err := tr.Options().SetLockAware(); err != nil {
		return nil, err
	}

	bk, ek := r.FDBRangeKeys()
	prefix := []byte("\xFF/keyServers/")
	ffer := KeyRange{
		Begin: Key(append(append([]byte(nil), prefix...), bk.FDBKey()...)),
		End:   Key(append(append([]byte(nil), prefix...), ek.FDBKey()...)),
	}

	kvs, err := tr.Snapshot().GetRange(ffer, RangeOptions{Limit: limit}).GetSliceWithError()
	if err != nil {
		return nil, err
	}

	size := len(kvs)
	// Clamp only for a POSITIVE limit: limit <= 0 means "unlimited" (matching the
	// range-read path), so a negative sentinel must not drive size negative and
	// panic make([]Key, size). (The Apple binding's `limit != 0` form
	// panics here on a negative limit; we don't.)
	if limit > 0 && limit < size {
		size = limit
	}
	boundaries := make([]Key, size)
	for i := 0; i < size; i++ {
		boundaries[i] = kvs[i].Key[len(prefix):] // strip the \xFF/keyServers/ prefix
	}
	return boundaries, nil
}

// RebootWorker is not yet implemented.
func (db Database) RebootWorker(_ string, _ bool, _ int) error {
	return errNotSupported
}
