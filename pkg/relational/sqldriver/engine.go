// Portions derived from FoundationDB Record Layer (
// RelationalKeyspaceProvider.java, RelationalPlanCache.java,
// RecordLayerEngine.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/internal/fdbclient"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
)

// PlanCacheEvent classifies how the plan cache participated in a Plan call.
type PlanCacheEvent int

const (
	PlanCacheInconclusive PlanCacheEvent = iota
	PlanCacheSkip
	PlanCacheHit
	PlanCacheMiss
)

// String returns the lowercase event name.
func (e PlanCacheEvent) String() string {
	switch e {
	case PlanCacheSkip:
		return "skip"
	case PlanCacheHit:
		return "hit"
	case PlanCacheMiss:
		return "miss"
	default:
		return "inconclusive"
	}
}

// PlanInfo is the diagnostic record emitted once per query planning call.
type PlanInfo struct {
	SQL              string
	PlanHash         uint64
	PlanExplain      string
	PlanningDuration time.Duration
	Cache            PlanCacheEvent
	CacheNumEntries  int
	SlowQuery        bool
	LogQuery         bool
	Err              error
}

// PlanHook receives one callback per query planning call.
type PlanHook func(ctx context.Context, info PlanInfo)

// ExecStats is the diagnostic record emitted once per statement execution.
type ExecStats struct {
	SQL               string
	PlanHash          uint64
	ExecutionDuration time.Duration
	RowsReturned      int64
	RowsAffected      int64
	RecordsScanned    int64
	BytesScanned      int64
	Pages             int
	Retries           int
	SlowQuery         bool
	Err               error
}

// ExecStatsHook receives one callback per statement execution.
type ExecStatsHook func(ctx context.Context, stats ExecStats)

type planHookAdapter struct{ fn PlanHook }

func (a planHookAdapter) LogPlanGeneration(ctx context.Context, info embedded.PlanGenerationInfo) {
	a.fn(ctx, PlanInfo{
		SQL:              info.SQL,
		PlanHash:         info.PlanHash,
		PlanExplain:      info.PlanExplain,
		PlanningDuration: info.PlanningDuration,
		Cache:            PlanCacheEvent(info.Cache),
		CacheNumEntries:  info.CacheNumEntries,
		SlowQuery:        info.SlowQuery,
		LogQuery:         info.LogQuery,
		Err:              info.Err,
	})
}

type execStatsHookAdapter struct{ fn ExecStatsHook }

func (a execStatsHookAdapter) LogExecutionStats(ctx context.Context, stats embedded.ExecutionStats) {
	a.fn(ctx, ExecStats{
		SQL:               stats.SQL,
		PlanHash:          stats.PlanHash,
		ExecutionDuration: stats.ExecutionDuration,
		RowsReturned:      stats.RowsReturned,
		RowsAffected:      stats.RowsAffected,
		RecordsScanned:    stats.RecordsScanned,
		BytesScanned:      stats.BytesScanned,
		Pages:             stats.Pages,
		Retries:           stats.Retries,
		SlowQuery:         stats.SlowQuery,
		Err:               stats.Err,
	})
}

// ResourceLimits configures per-statement and per-page resource governance.
// Zero-valued fields leave that dimension unbounded (for TransactionTimeout, 0
// inherits the database default).
type ResourceLimits struct {
	// MaxRows caps total returned rows across all pages (api.OptMaxRows).
	MaxRows int64
	// ScannedRowsLimit caps records scanned per page (api.OptExecutionScannedRowsLimit).
	ScannedRowsLimit int64
	// ScannedBytesLimit caps bytes scanned per page (api.OptExecutionScannedBytesLimit).
	ScannedBytesLimit int64
	// PageTimeLimit narrows the per-page execution budget (api.OptExecutionTimeLimit).
	PageTimeLimit time.Duration
	// FailOnScanLimitReached makes a leaf cursor exceeding ScannedRowsLimit or
	// ScannedBytesLimit fail with SQLSTATE 54F01 instead of paginating.
	FailOnScanLimitReached bool
	// StatementTimeout bounds the total wall-clock duration of one execution
	// across all pages.
	StatementTimeout time.Duration
	// MaxStatementMemoryBytes caps in-memory buffering per statement
	// (api.OptMaxStatementMemoryBytes).
	MaxStatementMemoryBytes int64
	// MaxResultBytes caps cumulative encoded bytes of returned rows per statement.
	MaxResultBytes int64
	// TransactionTimeout bounds each underlying FDB transaction
	// (api.OptTransactionTimeout). 0 inherits the database default.
	TransactionTimeout time.Duration
}

// DefaultLimits returns bounded production resource limits suitable for
// multi-tenant and shared-cluster services.
func DefaultLimits() *ResourceLimits {
	return &ResourceLimits{
		MaxRows:                 10_000,
		ScannedRowsLimit:        500_000,
		ScannedBytesLimit:       64 << 20,
		PageTimeLimit:           2 * time.Second,
		FailOnScanLimitReached:  true,
		StatementTimeout:        30 * time.Second,
		MaxStatementMemoryBytes: 256 << 20,
		MaxResultBytes:          32 << 20,
	}
}

// UnboundedJavaCompatLimits returns the unbounded resource limits matching
// Java Record Layer's default connection behavior.
func UnboundedJavaCompatLimits() *ResourceLimits {
	return &ResourceLimits{}
}

func cloneLimits(l *ResourceLimits) *ResourceLimits {
	if l == nil {
		return DefaultLimits()
	}
	cp := *l
	return &cp
}

func validateResourceLimits(l *ResourceLimits) error {
	if l == nil {
		return nil
	}
	if l.MaxRows < 0 || l.MaxRows > math.MaxInt32 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"MaxRows must be in [0, %d], got %d", math.MaxInt32, l.MaxRows)
	}
	if l.ScannedRowsLimit < 0 || l.ScannedRowsLimit > math.MaxInt32 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"ScannedRowsLimit must be in [0, %d], got %d", math.MaxInt32, l.ScannedRowsLimit)
	}
	if l.ScannedBytesLimit < 0 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"ScannedBytesLimit must be >= 0, got %d", l.ScannedBytesLimit)
	}
	if l.PageTimeLimit < 0 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"PageTimeLimit must be >= 0, got %v", l.PageTimeLimit)
	}
	if l.StatementTimeout < 0 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"StatementTimeout must be >= 0, got %v", l.StatementTimeout)
	}
	if l.MaxStatementMemoryBytes < 0 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"MaxStatementMemoryBytes must be >= 0, got %d", l.MaxStatementMemoryBytes)
	}
	if l.MaxResultBytes < 0 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"MaxResultBytes must be >= 0, got %d", l.MaxResultBytes)
	}
	if l.TransactionTimeout < 0 || l.TransactionTimeout.Milliseconds() > math.MaxInt32 {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"TransactionTimeout must be in [0, %dms], got %v", math.MaxInt32, l.TransactionTimeout)
	}
	return nil
}

func millisRoundedUp(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}

func applyLimitsToBuilder(b *api.OptionsBuilder, l *ResourceLimits) {
	if l == nil {
		return
	}
	if l.MaxRows > 0 {
		b.Set(api.OptMaxRows, int(l.MaxRows))
	}
	if l.ScannedRowsLimit > 0 {
		b.Set(api.OptExecutionScannedRowsLimit, int(l.ScannedRowsLimit))
	}
	if l.ScannedBytesLimit > 0 {
		b.Set(api.OptExecutionScannedBytesLimit, l.ScannedBytesLimit)
	}
	if l.PageTimeLimit > 0 {
		b.Set(api.OptExecutionTimeLimit, millisRoundedUp(l.PageTimeLimit))
	}
	if l.MaxStatementMemoryBytes > 0 {
		b.Set(api.OptMaxStatementMemoryBytes, l.MaxStatementMemoryBytes)
	}
	if l.TransactionTimeout > 0 {
		b.Set(api.OptTransactionTimeout, millisRoundedUp(l.TransactionTimeout))
	}
}

// EngineConfig configures an explicit Engine instance.
type EngineConfig struct {
	// ClusterFile is the FDB cluster file path. Empty selects FDB's default
	// cluster-file resolution; must be empty when WithDatabase is used.
	ClusterFile string
	// Domains are the instance-scoped keyspace domains registered on this
	// engine (e.g. "FRL"), isolated from process-global domains.
	Domains []string
	// Limits sets default resource limits for connections opened from this
	// engine. Nil defaults to DefaultLimits().
	Limits *ResourceLimits
	// StoreTimer collects record-layer counters across transactions executed by
	// this engine. Nil creates a dedicated StoreTimer unless DisableStoreTimer
	// is true.
	StoreTimer *recordlayer.StoreTimer
	// DisableStoreTimer disables StoreTimer attachment so NewEngine does not
	// call fdbDB.SetTimer and Engine.StoreTimer returns nil (for uninstrumented
	// benchmarks). Cannot be combined with a non-nil StoreTimer.
	DisableStoreTimer bool
	// PlanHook receives one callback per query planning call across connections
	// opened from this engine (surviving ResetSession).
	PlanHook PlanHook
	// ExecStatsHook receives one callback per statement execution across
	// connections opened from this engine (surviving ResetSession).
	ExecStatsHook ExecStatsHook
	// SlowQueryThreshold marks planning or execution as slow when exceeded.
	// Zero uses the canonical 2s default; negative disables slow-query marking.
	SlowQueryThreshold time.Duration
	// PlanCacheOptions optionally configures PLAN_CACHE_* sizing and TTLs.
	PlanCacheOptions *api.Options
}

type engineOptions struct {
	hasDB bool
	db    *recordlayer.FDBDatabase
}

// EngineOption customizes NewEngine construction.
type EngineOption func(*engineOptions)

// WithDatabase injects an already-constructed *recordlayer.FDBDatabase into
// NewEngine (e.g. a SimFDB-backed database), replacing process-global
// RegisterBackend. The caller retains ownership of db: Engine.Close does not
// close an injected database.
func WithDatabase(db *recordlayer.FDBDatabase) EngineOption {
	return func(o *engineOptions) {
		o.hasDB = true
		o.db = db
	}
}

// PlanCacheMetrics is a snapshot of an Engine's plan-cache counters.
type PlanCacheMetrics struct {
	PrimaryMiss          int64
	SecondaryMiss        int64
	TertiaryHit          int64
	TertiaryMiss         int64
	PrimaryLRUEviction   int64
	SecondaryLRUEviction int64
	TertiaryLRUEviction  int64
}

// EngineMetrics is a point-in-time snapshot of an Engine's instrumentation.
type EngineMetrics struct {
	StoreTimer map[string]*recordlayer.CounterSnapshot
	PlanCache  PlanCacheMetrics
	WarmUp     WarmUpCounts
	Client     *client.ClientMetricsSnapshot
}

// Engine owns an isolated relational SQL runtime: FDB database handle (when
// opened by the engine), StoreTimer, instance-scoped keyspace Domains, catalog,
// shared plan cache, default ResourceLimits, and telemetry hooks. It touches
// none of the legacy process-global caches or registries.
type Engine struct {
	fdbDB        *recordlayer.FDBDatabase
	ownedBackend fdb.BackendDatabase // non-nil only when opened by NewEngine
	timer        *recordlayer.StoreTimer
	ks           *keyspace.RelationalKeyspace
	cat          *catalog.RecordLayerStoreCatalog
	factory      *ddl.RecordLayerMetadataOperationsFactory
	planCache    *embedded.RelationalPlanCache
	warmUp       WarmUpCounts

	limits                   *ResourceLimits
	planHook                 PlanHook
	execStatsHook            ExecStatsHook
	hasSlowQueryThreshold    bool
	slowQueryThresholdMicros int64

	closed atomic.Bool
}

// NewEngine constructs an explicit, isolated SQL Engine from cfg and opts.
func NewEngine(cfg EngineConfig, opts ...EngineOption) (*Engine, error) {
	var eo engineOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&eo)
		}
	}
	if eo.hasDB {
		if eo.db == nil {
			return nil, api.NewError(api.ErrCodeInvalidParameter, "WithDatabase: nil FDB database")
		}
		if cfg.ClusterFile != "" {
			return nil, api.NewError(api.ErrCodeInvalidParameter,
				"cannot specify both ClusterFile and WithDatabase")
		}
	}
	if cfg.DisableStoreTimer && cfg.StoreTimer != nil {
		return nil, api.NewError(api.ErrCodeInvalidParameter,
			"cannot specify both StoreTimer and DisableStoreTimer")
	}
	if err := validateResourceLimits(cfg.Limits); err != nil {
		return nil, err
	}
	if cfg.PlanCacheOptions != nil {
		for k, v := range cfg.PlanCacheOptions.AllEntries() {
			if err := api.ValidateOption(k, v); err != nil {
				return nil, err
			}
		}
	}
	limits := cloneLimits(cfg.Limits)

	var hasSlow bool
	var slowMicros int64
	switch {
	case cfg.SlowQueryThreshold < 0:
		hasSlow = true
		slowMicros = 0
	case cfg.SlowQueryThreshold > 0:
		hasSlow = true
		slowMicros = cfg.SlowQueryThreshold.Microseconds()
		if slowMicros == 0 {
			slowMicros = 1
		}
	}

	var fdbDB *recordlayer.FDBDatabase
	var ownedBackend fdb.BackendDatabase
	if eo.hasDB {
		fdbDB = eo.db
	} else {
		clusterFile := cfg.ClusterFile
		if clusterFile == "" {
			clusterFile = os.Getenv(defaultClusterFileEnv)
		}
		rawDB, err := fdbclient.Open(clusterFile)
		if err != nil {
			return nil, api.WrapError(api.ErrCodeInternalError, "open FDB database", err)
		}
		ownedBackend = rawDB
		fdbDB = recordlayer.NewFDBDatabaseWithBackend(rawDB)
		fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	}

	var timer *recordlayer.StoreTimer
	if !cfg.DisableStoreTimer {
		timer = cfg.StoreTimer
		if timer == nil {
			if existing := fdbDB.Timer(); existing != nil && eo.hasDB {
				timer = existing
			} else {
				timer = recordlayer.NewStoreTimer()
			}
		}
		fdbDB.SetTimer(timer)
	}

	ks := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains(cfg.Domains...))
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		if ownedBackend != nil {
			ownedBackend.Close()
		}
		return nil, err
	}
	factory := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)
	planCache := embedded.NewRelationalPlanCache(cfg.PlanCacheOptions)

	warmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	warmRaw := embedded.WarmStoredQueries(warmCtx, planCache,
		embedded.StoredQueryTemplates(warmCtx, embedded.ReadOnlyRunner(fdbDB), cat))

	return &Engine{
		fdbDB:                    fdbDB,
		ownedBackend:             ownedBackend,
		timer:                    timer,
		ks:                       ks,
		cat:                      cat,
		factory:                  factory,
		planCache:                planCache,
		warmUp:                   warmUpCountsFromEmbedded(warmRaw),
		limits:                   limits,
		planHook:                 cfg.PlanHook,
		execStatsHook:            cfg.ExecStatsHook,
		hasSlowQueryThreshold:    hasSlow,
		slowQueryThresholdMicros: slowMicros,
	}, nil
}

// ConnConfig configures connections opened from an Engine via OpenDB or Connector.
type ConnConfig struct {
	Schema                       string
	ReadOnly                     bool
	RestrictDDLToSessionDatabase bool
	DryRun                       bool
	IsolationLevelSnapshot       bool
	PlannerStatistics            bool
	TransactionTags              []string
	Limits                       *ResourceLimits
	PlanHook                     PlanHook
	ExecStatsHook                ExecStatsHook
	SlowQueryThreshold           time.Duration
	Options                      *api.Options

	planHookSet           bool
	execStatsHookSet      bool
	slowQueryThresholdSet bool
}

// ConnOption configures a Connector or *sql.DB opened from an Engine.
type ConnOption func(*ConnConfig)

// WithConnConfig applies a ConnConfig value.
func WithConnConfig(cfg ConnConfig) ConnOption {
	return func(c *ConnConfig) {
		*c = cfg
		if cfg.PlanHook != nil {
			c.planHookSet = true
		}
		if cfg.ExecStatsHook != nil {
			c.execStatsHookSet = true
		}
		if cfg.SlowQueryThreshold != 0 {
			c.slowQueryThresholdSet = true
		}
	}
}

// WithSchema sets the connection's default schema (restored on ResetSession).
func WithSchema(schema string) ConnOption {
	return func(c *ConnConfig) { c.Schema = schema }
}

// WithReadOnly marks every connection opened from the connector read-only.
func WithReadOnly(readOnly bool) ConnOption {
	return func(c *ConnConfig) { c.ReadOnly = readOnly }
}

// WithRestrictDDLToSessionDatabase confines DDL on the connection to its own database path.
func WithRestrictDDLToSessionDatabase(restrict bool) ConnOption {
	return func(c *ConnConfig) { c.RestrictDDLToSessionDatabase = restrict }
}

// WithDryRun enables DRY_RUN mode on every connection opened from the connector.
func WithDryRun(dryRun bool) ConnOption {
	return func(c *ConnConfig) { c.DryRun = dryRun }
}

// WithIsolationLevelSnapshot runs SELECTs at snapshot isolation and refuses DML.
func WithIsolationLevelSnapshot(snapshot bool) ConnOption {
	return func(c *ConnConfig) { c.IsolationLevelSnapshot = snapshot }
}

// WithPlannerStatistics enables collected planner statistics on the connection.
func WithPlannerStatistics(enabled bool) ConnOption {
	return func(c *ConnConfig) { c.PlannerStatistics = enabled }
}

// WithTransactionTags sets FDB transaction tags on every transaction opened by the connection.
func WithTransactionTags(tags ...string) ConnOption {
	return func(c *ConnConfig) { c.TransactionTags = append([]string(nil), tags...) }
}

// WithResourceLimits overrides the engine's default ResourceLimits for this connector.
func WithResourceLimits(limits *ResourceLimits) ConnOption {
	return func(c *ConnConfig) { c.Limits = limits }
}

// WithPlanHook overrides the engine's PlanHook for this connector (survives ResetSession).
func WithPlanHook(hook PlanHook) ConnOption {
	return func(c *ConnConfig) {
		c.PlanHook = hook
		c.planHookSet = true
	}
}

// WithExecStatsHook overrides the engine's ExecStatsHook for this connector (survives ResetSession).
func WithExecStatsHook(hook ExecStatsHook) ConnOption {
	return func(c *ConnConfig) {
		c.ExecStatsHook = hook
		c.execStatsHookSet = true
	}
}

// WithSlowQueryThreshold overrides the slow-query threshold for this connector (survives ResetSession).
// Negative disables slow-query marking; zero uses the canonical 2s default.
func WithSlowQueryThreshold(d time.Duration) ConnOption {
	return func(c *ConnConfig) {
		c.SlowQueryThreshold = d
		c.slowQueryThresholdSet = true
	}
}

// WithOptions merges additional api.Options onto connections opened from this connector.
func WithOptions(opts *api.Options) ConnOption {
	return func(c *ConnConfig) { c.Options = opts }
}

// Connector returns a driver.Connector bound to this Engine for dbPath.
func (e *Engine) Connector(dbPath string, opts ...ConnOption) (driver.Connector, error) {
	if e.closed.Load() {
		return nil, api.NewError(api.ErrCodeConnectionDoesNotExist, "engine is closed")
	}
	if dbPath == "" || dbPath == "/" || !strings.HasPrefix(dbPath, "/") {
		return nil, api.NewErrorf(api.ErrCodeInvalidPath, "<%s> is an invalid database path", dbPath)
	}

	var cfg ConnConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := validateResourceLimits(cfg.Limits); err != nil {
		return nil, err
	}

	limits := e.limits
	if cfg.Limits != nil {
		limits = cloneLimits(cfg.Limits)
	}

	b := api.NewOptionsBuilder()
	applyLimitsToBuilder(b, limits)
	if cfg.Options != nil {
		for k, v := range cfg.Options.AllEntries() {
			if err := api.ValidateOption(k, v); err != nil {
				return nil, err
			}
			if k == api.OptTransactionTimeout {
				if ms, ok := v.(int64); ok && ms > math.MaxInt32 {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"FDB transaction timeout is at most %d ms, got %d", math.MaxInt32, ms)
				}
			}
			b.Set(k, v)
		}
	}
	if cfg.RestrictDDLToSessionDatabase {
		b.Set(api.OptRestrictDDLToSessionDatabase, true)
	}
	if cfg.DryRun {
		b.Set(api.OptDryRun, true)
	}
	if cfg.IsolationLevelSnapshot {
		b.Set(api.OptIsolationLevelSnapshot, true)
	}
	if cfg.PlannerStatistics {
		b.Set(api.OptPlannerStatistics, true)
	}
	if len(cfg.TransactionTags) > 0 {
		uniq, err := recordlayer.ValidateTags(cfg.TransactionTags)
		if err != nil {
			return nil, api.WrapError(api.ErrCodeInvalidParameter, "transaction_tags", err)
		}
		sort.Strings(uniq)
		b.Set(api.OptTransactionTags, uniq)
	}

	planHook := e.planHook
	if cfg.planHookSet {
		planHook = cfg.PlanHook
	}
	execStatsHook := e.execStatsHook
	if cfg.execStatsHookSet {
		execStatsHook = cfg.ExecStatsHook
	}
	hasSlow := e.hasSlowQueryThreshold
	slowMicros := e.slowQueryThresholdMicros
	if cfg.slowQueryThresholdSet {
		switch {
		case cfg.SlowQueryThreshold < 0:
			hasSlow, slowMicros = true, 0
		case cfg.SlowQueryThreshold > 0:
			hasSlow = true
			slowMicros = cfg.SlowQueryThreshold.Microseconds()
			if slowMicros == 0 {
				slowMicros = 1
			}
		default:
			hasSlow, slowMicros = false, 0
		}
	}

	dsn := &DSN{
		Mode:    ModeEmbedded,
		Path:    dbPath,
		Schema:  cfg.Schema,
		Options: map[string]string{},
	}
	c := &Connector{
		driver:                   &Driver{},
		dsn:                      dsn,
		connOpts:                 b.Build(),
		readOnly:                 cfg.ReadOnly,
		callerDB:                 true,
		fdbDB:                    e.fdbDB,
		cat:                      e.cat,
		ks:                       e.ks,
		factory:                  e.factory,
		planCache:                e.planCache,
		engine:                   e,
		failOnScanLimitReached:   limits.FailOnScanLimitReached,
		statementTimeout:         limits.StatementTimeout,
		maxResultBytes:           limits.MaxResultBytes,
		planHook:                 planHook,
		execStatsHook:            execStatsHook,
		hasSlowQueryThreshold:    hasSlow,
		slowQueryThresholdMicros: slowMicros,
	}
	warmUp := e.warmUp
	c.warmUp.Store(&warmUp)
	return c, nil
}

// OpenDB returns a *sql.DB pool bound to this Engine for dbPath.
func (e *Engine) OpenDB(dbPath string, opts ...ConnOption) (*sql.DB, error) {
	c, err := e.Connector(dbPath, opts...)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// RegisterDomain adds a domain to this engine's instance-scoped keyspace.
func (e *Engine) RegisterDomain(name string) {
	e.ks.RegisterDomain(name)
}

// RegisteredDomains returns the domains registered on this engine's keyspace.
func (e *Engine) RegisteredDomains() []string {
	return e.ks.RegisteredDomains()
}

// StoreTimer returns the engine's record-layer StoreTimer (usable with
// rlmetrics.Handler), or nil when EngineConfig.DisableStoreTimer is true.
func (e *Engine) StoreTimer() *recordlayer.StoreTimer {
	return e.timer
}

// WarmUp returns the stored-query warm-up counts collected when the engine started.
func (e *Engine) WarmUp() WarmUpCounts {
	return e.warmUp
}

// Metrics returns a point-in-time snapshot of the engine's instrumentation.
func (e *Engine) Metrics() EngineMetrics {
	counts := e.planCache.Counts()
	m := EngineMetrics{
		StoreTimer: e.timer.Snapshot(),
		PlanCache: PlanCacheMetrics{
			PrimaryMiss:          counts.PrimaryMiss,
			SecondaryMiss:        counts.SecondaryMiss,
			TertiaryHit:          counts.TertiaryHit,
			TertiaryMiss:         counts.TertiaryMiss,
			PrimaryLRUEviction:   counts.PrimaryLRUEviction,
			SecondaryLRUEviction: counts.SecondaryLRUEviction,
			TertiaryLRUEviction:  counts.TertiaryLRUEviction,
		},
		WarmUp: e.warmUp,
	}
	if src, ok := e.ownedBackend.(interface {
		Metrics() client.ClientMetricsSnapshot
	}); ok {
		cm := src.Metrics()
		m.Client = &cm
	}
	return m
}

// Close closes resources owned by this Engine (the opened FDB backend handle,
// if any) and rejects subsequent OpenDB / Connector / Connect calls. Idempotent.
func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	if e.ownedBackend != nil {
		e.ownedBackend.Close()
	}
	e.planCache.Invalidate()
	return nil
}
