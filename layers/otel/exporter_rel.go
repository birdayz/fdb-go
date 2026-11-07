package otel

import (
	"context"
	"errors"
	"strings"
	"sync"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The relational Exporter (rfcs/relational-exporter-migration.md §1, §5): one
// OTEL_V1 schema store per (run, tenant), created on first open, every write
// and read a transaction on it under the exporter's policy. It is unexported
// until the cutover renames it Exporter (§6 phase 3).

var _ sdktrace.SpanExporter = (*relExporter)(nil)

// relMessageHooks are the GenAI message stores of a tenant store (stream 2C,
// messages_rel.go). The exporter decomposes and re-encodes manifests itself
// (decomposeMessages, encodeManifests) and has no hooks when messages are off.
type relMessageHooks interface {
	// persist stores d's unique elements and trace markers before the spans
	// that reference them and sets every element's id, so
	// d.encodeManifests(false) yields final manifests.
	persist(ctx context.Context, d *messageDecomposition) (MessageStoreStats, error)
	// refChunks packs one span's references into SPANS.ref_chunks values.
	refChunks(uses []messageUse) ([][]byte, error)
	// reconstruct restores the original attribute values of a loaded
	// single-span ResourceSpans, reading MESSAGES rows in store, charging work.
	reconstruct(ctx context.Context, store *recordlayer.FDBRecordStore, rs *tracepb.ResourceSpans, work *queryWork) error
	close()
}

// relLogs is the logs of a tenant store (*relLogStore, logs_rel.go).
type relLogs interface {
	ExportLogs(ctx context.Context, logs []LogRecord) (LogExportStats, error)
	QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error)
	QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error)
}

// relParts are what other streams supply to a relExporter, and test seams.
type relParts struct {
	// messages builds the message hooks when cfg enables messages; nil is
	// newRelMessageStore.
	messages func(s *tenantStore, cfg ExporterConfig) (relMessageHooks, error)
	// logs builds the log store; nil is newRelLogStore.
	logs func(s *tenantStore, cfg ExporterConfig) (relLogs, error)
	// newDatabase wraps the FDB handle; nil is recordlayer.NewFDBDatabase.
	newDatabase func(fdb.Database) *recordlayer.FDBDatabase
}

// relExporter stores OTLP spans as SPANS rows of its tenant store and serves
// every read from the store's standard indexes. Its methods are safe for
// concurrent use; reads share no state, so none serializes another.
type relExporter struct {
	cfg      ExporterConfig
	raw      fdb.Database
	store    *tenantStore
	reader   *SpanReader
	writer   *SpanWriter
	messages relMessageHooks // nil when messages are off
	logs     relLogs
	// txnSlots holds one token per in-flight write transaction.
	txnSlots chan struct{}

	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
	done   chan struct{}
}

// newRelExporter validates cfg, opens the database and ensures the tenant
// store (template, database, schema, created when absent: D8) in exporter
// policy transactions, then checks the store's layout.
func newRelExporter(ctx context.Context, cfg ExporterConfig, parts relParts) (*relExporter, error) {
	const op = "open exporter"
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(cfg.RecordCompression, "brotli") {
		// D2: SQL and Java read the store's records; neither reads Brotli.
		return nil, failf("config", "exporter", "record_compression %s is not readable by SQL or Java; use none, zlib-1 or zlib-default", cfg.RecordCompression)
	}
	serializer, err := recordSerializer(cfg.RecordCompression)
	if err != nil {
		return nil, err
	}
	ctx = withExporterTxns(ctx, cfg.TransactionTimeout)
	raw, err := openRelDatabase(ctx, cfg.ClusterFile)
	if err != nil {
		return nil, err
	}
	newDatabase := parts.newDatabase
	if newDatabase == nil {
		newDatabase = recordlayer.NewFDBDatabase
	}
	db := newDatabase(raw)
	abort := func(err error) (*relExporter, error) {
		raw.Close()
		return nil, err
	}
	keyspace.RegisterDomainIfNotExists(relDomain)
	path, schema := relTenantPaths(cfg.Run, cfg.Tenant)
	ts, err := otelsql.EnsureTenant(ctx, db, path, schema, recordTransactionConfigurer(ctx))
	if err != nil {
		return abort(relOpenError(ctx, op, err))
	}
	ts.Serializer = serializer
	store := &tenantStore{db: db, ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema, fullText: cfg.EnableMessageFullText}
	if err := checkStoreLayout(ctx, store, relStoreLayout(cfg)); err != nil {
		return abort(relOpenError(ctx, op, err))
	}
	e := &relExporter{
		cfg: cfg, raw: raw, store: store,
		reader:   &SpanReader{store: store, cfg: cfg.backendConfig(), keys: storeMetaContinuationKeys{}},
		writer:   &SpanWriter{store: store, opts: SpanWriterOptions{BatchSize: cfg.BatchSize, Workers: cfg.Workers, BulkIndexConflicts: cfg.BulkIndexConflicts}, maxTxnBytes: maxSpanTxnBytes},
		txnSlots: make(chan struct{}, cfg.MaxInFlightTransactions),
		done:     make(chan struct{}),
	}
	if parts.messages == nil {
		parts.messages = func(s *tenantStore, cfg ExporterConfig) (relMessageHooks, error) {
			return newRelMessageStore(s, cfg.EnableMessageFullText), nil
		}
	}
	if cfg.messageLayout() != messagesNone {
		if e.messages, err = parts.messages(store, cfg); err != nil {
			return abort(err)
		}
	}
	if parts.logs == nil {
		parts.logs = func(s *tenantStore, cfg ExporterConfig) (relLogs, error) { return newRelLogStore(s, cfg) }
	}
	{
		if e.logs, err = parts.logs(store, cfg); err != nil {
			if e.messages != nil {
				e.messages.close()
			}
			return abort(err)
		}
	}
	return e, nil
}

// openRelDatabase opens the cluster file's database; caller cancellation
// ends a bootstrap in progress.
func openRelDatabase(ctx context.Context, cluster string) (fdb.Database, error) {
	if err := ctx.Err(); err != nil {
		return fdb.Database{}, wrap("canceled", "open FDB", err)
	}
	if cluster == "" {
		return fdb.Database{}, fail("config", "open FDB", "explicit owned FDB cluster file required")
	}
	if err := fdb.APIVersion(730); err != nil {
		return fdb.Database{}, err
	}
	cf, err := client.ParseClusterFile(cluster)
	if err != nil {
		return fdb.Database{}, wrap("config", "cluster file", err)
	}
	if err := ctx.Err(); err != nil {
		return fdb.Database{}, wrap("canceled", "open FDB", err)
	}
	return fdb.OpenDatabaseFromConfig(ctx, cf)
}

// relOpenError classifies a store open failure (§4.5): a stored template or
// schema binding of another build and a removed store are ownership, an
// invalid store path is config, and the rest as any exporter transaction.
func relOpenError(ctx context.Context, op string, err error) error {
	var mismatch *otelsql.TemplateMismatchError
	var missing *recordlayer.RecordStoreDoesNotExistError
	var apiErr *api.Error
	switch {
	case errors.As(err, &mismatch), errors.As(err, &missing):
		return wrap("ownership", op, err)
	case errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeInvalidPath:
		return wrap("config", op, err)
	}
	return exporterTxnError(ctx, op, err)
}

func (e *relExporter) Config() ExporterConfig { return e.cfg }

// txnContext applies the exporter's transaction policy to work under ctx.
func (e *relExporter) txnContext(ctx context.Context) context.Context {
	return withExporterTxns(ctx, e.cfg.TransactionTimeout)
}

func (e *relExporter) begin(op string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fail("operation", op, "exporter is shut down")
	}
	e.active.Add(1)
	return nil
}

// Shutdown rejects new work, joins active calls and the message hooks, then
// closes the database. It is idempotent; on context expiry it returns
// canceled and the drain continues in the background.
func (e *relExporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		go func() {
			e.active.Wait()
			if e.messages != nil {
				e.messages.close()
			}
			e.raw.Close()
			close(e.done)
		}()
	}
	e.mu.Unlock()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return wrap("canceled", "exporter shutdown", ctx.Err())
	}
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *relExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	_, err := e.ExportResourceSpans(ctx, ResourceSpansFromReadOnly(spans))
	return err
}

// ConsumeTraces accepts OTLP ResourceSpans from a Collector or OTLP receiver.
func (e *relExporter) ConsumeTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	_, err := e.ExportResourceSpans(ctx, spans)
	return err
}

func (e *relExporter) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	if err := e.begin("query"); err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	defer e.active.Done()
	return e.reader.Query(e.txnContext(ctx), a)
}

func (e *relExporter) Trace(ctx context.Context, traceID string) ([]SpanHit, error) {
	if err := e.begin("trace"); err != nil {
		return []SpanHit{}, err
	}
	defer e.active.Done()
	out, _, err := e.reader.Trace(e.txnContext(ctx), traceID)
	return out, err
}

func (e *relExporter) Span(ctx context.Context, spanID string) ([]SpanMatch, error) {
	if err := e.begin("span"); err != nil {
		return []SpanMatch{}, err
	}
	defer e.active.Done()
	out, _, err := e.reader.Span(e.txnContext(ctx), spanID)
	return out, err
}

func (e *relExporter) Time(ctx context.Context, interval TimeRange) (QueryResult, error) {
	if err := e.begin("time"); err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	defer e.active.Done()
	return e.reader.Time(e.txnContext(ctx), interval)
}

// QuerySameSpan returns one verified snapshot page of same-span matches.
func (e *relExporter) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	if err := e.begin("same-span query"); err != nil {
		return SpanQueryPage{Spans: []SpanMatch{}, Order: "time"}, err
	}
	defer e.active.Done()
	return e.reader.QuerySameSpan(e.txnContext(ctx), q)
}

// QueryTraceExistential returns the newest traces satisfying every clause, with witnesses.
func (e *relExporter) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	if len(q.MessageClauses) > 0 && !e.cfg.EnableMessageFullText {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, fail("config", "trace existential", "message clauses need enable_message_full_text")
	}
	if err := e.begin("trace existential"); err != nil {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, err
	}
	defer e.active.Done()
	return e.reader.QueryTraceExistential(e.txnContext(ctx), q)
}

// TraverseTraceGraph returns the observed parent/child/link topology of one trace.
func (e *relExporter) TraverseTraceGraph(ctx context.Context, traceID string) (TraceGraph, error) {
	if err := e.begin("trace graph"); err != nil {
		return TraceGraph{}, err
	}
	defer e.active.Done()
	g, _, err := e.reader.TraverseTraceGraph(e.txnContext(ctx), traceID)
	return g, err
}

// logStore is the log store, or a config error until stream 2B supplies one.
func (e *relExporter) logStore(op string) (relLogs, error) {
	if e.logs == nil {
		return nil, fail("config", op, "logs are not yet supported on the relational store")
	}
	return e.logs, nil
}

// ExportLogs stores log records; its write transactions take write slots.
func (e *relExporter) ExportLogs(ctx context.Context, logs []LogRecord) (LogExportStats, error) {
	if err := e.begin("log export"); err != nil {
		return LogExportStats{}, err
	}
	defer e.active.Done()
	s, err := e.logStore("log export")
	if err != nil {
		return LogExportStats{}, err
	}
	return s.ExportLogs(withTxnLimiter(e.txnContext(ctx), e.txnSlots), logs)
}

// ConsumeLogs accepts OTLP ResourceLogs from a Collector or OTLP receiver.
func (e *relExporter) ConsumeLogs(ctx context.Context, logs []*logspb.ResourceLogs) error {
	_, err := e.ExportLogs(ctx, LogRecordsFromResourceLogs(logs))
	return err
}

func (e *relExporter) QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error) {
	if err := e.begin("log trace query"); err != nil {
		return []LogRecord{}, err
	}
	defer e.active.Done()
	s, err := e.logStore("log trace query")
	if err != nil {
		return []LogRecord{}, err
	}
	return s.QueryLogsByTrace(e.txnContext(ctx), traceID, spanID)
}

func (e *relExporter) QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error) {
	if err := e.begin("log attribute query"); err != nil {
		return []LogRecord{}, err
	}
	defer e.active.Done()
	s, err := e.logStore("log attribute query")
	if err != nil {
		return []LogRecord{}, err
	}
	return s.QueryLogsByAttribute(e.txnContext(ctx), a, limit)
}
