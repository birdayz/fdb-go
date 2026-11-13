package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/prometheus/client_golang/prometheus"

	"fdb.dev/layers/otel"
)

// PluginID is the plugin's Grafana ID.
const PluginID = "birdayz-foundationdb-datasource"

// Pool and cache sizes (RFC §9).
const (
	tenantPoolCapacity   = 32 // tenant SQL pools (sqlds datasources) per instance
	readerCacheCapacity  = 32 // tenant OTel readers per instance
	sqlMaxOpenConns      = 8
	sqlMaxIdleConns      = 4
	catalogPoolKey       = "" // schema-less connection: tenant listing, health
	attributeCacheMaxLen = 1024
)

// deps are the I/O entry points and the metrics sink; tests substitute fakes.
type deps struct {
	openFDB      fdbOpener
	openReader   readerOpener
	newConnector connectorFactory
	metrics      *metrics
}

func defaultDeps() deps {
	return deps{openFDB: openFDB, openReader: openReader, newConnector: newConnector, metrics: processMetrics()}
}

// Datasource is one datasource instance: one FDB database, per-tenant SQL
// pools and OTel readers, all closed by Dispose.
type Datasource struct {
	inst   backend.DataSourceInstanceSettings
	cfg    *Settings
	cfgErr error
	deps   deps

	gate gate

	fdbMu sync.Mutex
	fdb   *fdbHandle

	sqlPools *refLRU[*sqlTenant]
	readers  *refLRU[otelReader]
	attrs    *ttlCache

	mux       *datasource.QueryTypeMux
	resources backend.CallResourceHandler
}

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

// NewDatasource is the instance factory for datasource.Manage. It does no
// I/O: invalid settings and an unreachable cluster surface through
// CheckHealth and queries, with the failing step named.
func NewDatasource(_ context.Context, s backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	return newDatasource(s, defaultDeps()), nil
}

func newDatasource(s backend.DataSourceInstanceSettings, d deps) *Datasource {
	ds := &Datasource{inst: s, deps: d, attrs: newTTLCache(attributeCacheMaxLen, attributeCacheTTL)}
	ds.cfg, ds.cfgErr = ParseSettings(s)
	ds.sqlPools = newRefLRU(tenantPoolCapacity, ds.openSQLTenant, (*sqlTenant).close)
	ds.readers = newRefLRU(readerCacheCapacity, ds.openTenantReader, func(r otelReader) { _ = r.Close() })
	ds.mux = datasource.NewQueryTypeMux()
	ds.mux.HandleFunc(queryTypeSQL, ds.querySQL)
	ds.mux.HandleFunc(queryTypeTrace, ds.perQuery(ds.queryTrace))
	ds.mux.HandleFunc(queryTypeTraceSearch, ds.perQuery(ds.queryTraceSearch))
	ds.mux.HandleFunc(queryTypeLogs, ds.perQuery(ds.queryLogs))
	ds.resources = ds.newResourceHandler()
	return ds
}

// Dispose closes pools, readers and the FDB database once the requests that
// started before it have finished.
func (d *Datasource) Dispose() {
	d.gate.close(func() {
		d.sqlPools.shutdown()
		d.readers.shutdown()
		d.fdbMu.Lock()
		defer d.fdbMu.Unlock()
		if d.fdb != nil {
			d.fdb.close()
			d.fdb = nil
		}
	})
}

// QueryData dispatches on queryType.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	if !d.gate.enter() {
		return nil, errDisposed
	}
	defer d.gate.leave()
	if d.cfgErr != nil {
		resp := backend.NewQueryDataResponse()
		for _, q := range req.Queries {
			resp.Responses[q.RefID] = backend.ErrorResponseWithErrorSource(backend.DownstreamError(d.cfgErr))
		}
		return resp, nil
	}
	return d.mux.QueryData(ctx, req)
}

// CallResource serves the resource routes.
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	if !d.gate.enter() {
		return errDisposed
	}
	defer d.gate.leave()
	return d.resources.CallResource(ctx, req, sender)
}

// database opens the FDB database on first use; a failed open is retried by
// the next request.
func (d *Datasource) database(ctx context.Context) (*fdbHandle, error) {
	d.fdbMu.Lock()
	defer d.fdbMu.Unlock()
	if d.fdb != nil {
		return d.fdb, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.QueryTimeout)
	defer cancel()
	h, err := d.deps.openFDB(ctx, d.cfg, d.inst)
	if err != nil {
		return nil, &StepError{Step: stepOpenCluster, Err: err}
	}
	d.fdb = h
	return h, nil
}

// tenant resolves a query's tenant (empty = default schema) and applies
// allowedSchemas before anything reads.
func (d *Datasource) tenant(name string) (string, error) {
	if name == "" {
		name = d.cfg.DefaultSchema
	}
	if name == "" {
		return "", &TenantError{Tenant: "", Reason: "the query names no tenant and the datasource has no default schema"}
	}
	if err := d.cfg.AllowedSchemas.check(name); err != nil {
		return "", err
	}
	return name, nil
}

// readerFor returns the tenant's OTel reader; release it when done.
func (d *Datasource) readerFor(ctx context.Context, tenant string) (otelReader, func(), error) {
	if d.cfg.OTelMode == otelModeOff {
		return nil, nil, &OTelDisabledError{}
	}
	return d.readers.acquire(ctx, tenant)
}

// withReader runs fn on the tenant's reader. A store whose metadata changed
// since the reader opened fails stale: the reader is re-opened and fn retried
// once (RFC §8, D12).
func (d *Datasource) withReader(ctx context.Context, queryType, tenant string, fn func(otelReader) error) error {
	for attempt := 0; ; attempt++ {
		r, release, err := d.readerFor(ctx, tenant)
		if err != nil {
			return err
		}
		err = fn(r)
		var stale *otel.StaleMetadataError
		if attempt == 0 && errors.As(err, &stale) {
			d.deps.metrics.readerReopens.With(d.labels(queryType, tenant)).Inc()
			d.readers.invalidate(tenant, func(x otelReader) bool { return x == r })
			release()
			continue
		}
		release()
		return err
	}
}

// labels are the metric labels of one query type on one tenant.
func (d *Datasource) labels(queryType, tenant string) prometheus.Labels {
	return prometheus.Labels{"datasource": d.inst.UID, "query_type": queryType, "tenant": tenant}
}

// traceBudget bounds a trace read: traceMaxBytes is what Grafana receives
// (decoded protobuf); stored bytes read keep the reader's default.
func (d *Datasource) traceBudget() otel.TraceBudget {
	return otel.TraceBudget{MaxSpans: d.cfg.TraceMaxSpans, MaxDecodedBytes: d.cfg.TraceMaxBytes}
}

func (d *Datasource) openTenantReader(ctx context.Context, schema string) (otelReader, error) {
	h, err := d.database(ctx)
	if err != nil {
		return nil, err
	}
	r, err := d.deps.openReader(ctx, h.db, d.cfg.Database, schema, otel.ReaderConfig{
		MaxCandidates:      d.cfg.SearchMaxCandidates,
		MaxBytes:           d.cfg.SearchMaxBytes,
		K:                  d.cfg.SearchLimit,
		TransactionTimeout: d.cfg.TransactionTimeout,
		TransactionTags:    d.cfg.TransactionTags,
	})
	if err != nil {
		if isOwnershipError(err) {
			return nil, &NotOTelStoreError{Tenant: schema, Err: err}
		}
		return nil, err
	}
	return r, nil
}

// perQuery runs fn for each query of the request concurrently, bounded by
// the query timeout, and turns its error into that query's response.
func (d *Datasource) perQuery(fn func(ctx context.Context, pc backend.PluginContext, q backend.DataQuery) (backend.DataResponse, error)) func(context.Context, *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	return func(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
		resp := backend.NewQueryDataResponse()
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, q := range req.Queries {
			wg.Go(func() {
				qctx, cancel := context.WithTimeout(ctx, d.cfg.QueryTimeout)
				defer cancel()
				r, err := fn(qctx, req.PluginContext, q)
				if err != nil {
					r = errorResponse(err)
				}
				mu.Lock()
				resp.Responses[q.RefID] = r
				mu.Unlock()
			})
		}
		wg.Wait()
		return resp, nil
	}
}

// OTelDisabledError refuses OTel query types when otelMode is off.
type OTelDisabledError struct{}

func (*OTelDisabledError) Error() string {
	return "OTel queries are disabled for this datasource (otelMode off)"
}

// NotOTelStoreError reports a tenant schema that is not an OTEL_V1 store.
type NotOTelStoreError struct {
	Tenant string
	Err    error
}

func (e *NotOTelStoreError) Error() string {
	return fmt.Sprintf("tenant %q is not an OTEL_V1 store: %v", e.Tenant, e.Err)
}

func (e *NotOTelStoreError) Unwrap() error { return e.Err }

// errorResponse states err with its FDB code or SQLSTATE. Query, tenant,
// settings, budget and FDB failures are all downstream of the plugin.
func errorResponse(err error) backend.DataResponse {
	return backend.ErrorResponseWithErrorSource(backend.DownstreamError(errors.New(describeError(err))))
}
