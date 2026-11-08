package otel

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The Reader (rfcs/grafana-datasource.md §5, §7, §8): a dashboard's view of
// one OTEL_V1 tenant store. It creates, publishes and commits nothing: every
// transaction is read-only and cancelled, and its reads are the SpanReader's
// and the log store's, without an Exporter.

// ReaderConfig bounds a Reader's work; zero fields take the defaults.
type ReaderConfig struct {
	// MaxCandidates and MaxBytes are each search's budget (default 20,000
	// candidates, 4 MiB logical read bytes); exceeding one fails the search.
	MaxCandidates int
	MaxBytes      int64
	// K is the default search limit (default 20, at most 1,000).
	K int
	// TransactionTimeout bounds each FDB transaction (default 5s).
	TransactionTimeout time.Duration
	// TransactionTags are set on every transaction as auto-throttle tags, so
	// an operator can throttle the Reader's reads: at most 5, 1..16 bytes each.
	TransactionTags []string
}

const (
	defaultReaderMaxCandidates = 20_000
	defaultReaderMaxBytes      = 4 << 20
	defaultReaderK             = 20
	maxReaderK                 = 1000
	defaultReaderTxnTimeout    = 5 * time.Second
	// FDB's MAX_TAGS_PER_TRANSACTION and MAX_TRANSACTION_TAG_LENGTH.
	maxReaderTags     = 5
	maxReaderTagBytes = 16
	// readerAttempts bounds a read's attempts: the first and its retries.
	readerAttempts = benchTxnRetryLimit + 1
)

func (c ReaderConfig) withDefaults() ReaderConfig {
	if c.MaxCandidates == 0 {
		c.MaxCandidates = defaultReaderMaxCandidates
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = defaultReaderMaxBytes
	}
	if c.K == 0 {
		c.K = defaultReaderK
	}
	if c.TransactionTimeout == 0 {
		c.TransactionTimeout = defaultReaderTxnTimeout
	}
	return c
}

// Validate checks c after its defaults are applied.
func (c ReaderConfig) Validate() error {
	const op = "reader config"
	c = c.withDefaults()
	switch {
	case c.MaxCandidates < 1 || c.MaxCandidates > 100_000_000:
		return fail("config", op, "max candidates must be 1..100000000")
	case c.MaxBytes < 1:
		return fail("config", op, "max bytes must be positive")
	case c.K < 1 || c.K > maxReaderK:
		return failf("config", op, "K must be 1..%d", maxReaderK)
	case c.TransactionTimeout < minExporterTxnTimeout || c.TransactionTimeout > maxExporterTxnTimeout:
		return failf("config", op, "transaction timeout must be %s..%s", minExporterTxnTimeout, maxExporterTxnTimeout)
	case len(c.TransactionTags) > maxReaderTags:
		return failf("config", op, "at most %d transaction tags", maxReaderTags)
	}
	for _, tag := range c.TransactionTags {
		if tag == "" || len(tag) > maxReaderTagBytes {
			return failf("config", op, "transaction tag %q must be 1..%d bytes", tag, maxReaderTagBytes)
		}
	}
	return nil
}

// TraceBudget bounds TraceOTLP; zero fields take the defaults (10,000 spans,
// 64 MiB of logical read bytes, span rows and GenAI messages together).
type TraceBudget struct {
	MaxSpans int
	MaxBytes int64
}

const (
	defaultTraceMaxSpans = 10_000
	defaultTraceMaxBytes = 64 << 20
)

// TraceReadStats is what TraceOTLP read. Truncated reports a partial result;
// Reason lists the limits it hit, comma-separated, among the TraceReason
// values. Bytes counts every row read, including message rows a budget then
// left unused.
type TraceReadStats struct {
	Spans     int
	Bytes     int64
	Truncated bool
	Reason    string
}

// The reasons of a partial TraceOTLP result.
const (
	// TraceReasonMaxSpans: the trace has more than MaxSpans spans; the first
	// MaxSpans in span ID order are returned.
	TraceReasonMaxSpans = "max_spans"
	// TraceReasonMaxBytes: the span rows pass MaxBytes; those read before
	// it, in span ID order, are returned.
	TraceReasonMaxBytes = "max_bytes"
	// TraceReasonMessages: restoring the spans' GenAI message values would
	// pass MaxBytes, so they are the stored manifests.
	TraceReasonMessages = "messages"
)

// Reader reads one OTEL_V1 tenant store without writing: traces as OTLP,
// searches, logs and attribute autocomplete. Its methods are safe for
// concurrent use.
type Reader struct {
	cfg    ReaderConfig
	store  *tenantStore
	layout string
	spans  *SpanReader
	logs   *relLogStore
	closed atomic.Bool
}

// readerTxns runs a Reader's transactions. Each attempt is a fresh
// transaction with the GRV cache and the Reader's tags, cancelled when the
// attempt ends and never committed; a retryable FDB error retries after
// OnError's backoff, at most readerAttempts attempts in all.
type readerTxns struct {
	tags []string
	// observe, when set, runs in each attempt's transaction before its work
	// (tests).
	observe func(*recordlayer.FDBRecordContext)
}

func (p *readerTxns) run(ctx context.Context, db *recordlayer.FDBDatabase, op string, fn func(*recordlayer.FDBRecordContext) error) error {
	for attempt := 1; ; attempt++ {
		retry, err := p.attempt(ctx, db, fn, attempt < readerAttempts)
		if !retry {
			return exporterTxnError(ctx, op, err)
		}
	}
}

func (p *readerTxns) attempt(ctx context.Context, db *recordlayer.FDBDatabase, fn func(*recordlayer.FDBRecordContext) error, mayRetry bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rc, err := db.OpenRecordContext(ctx)
	if err != nil {
		return false, err
	}
	defer rc.Cancel()
	o := rc.Transaction().Options()
	if err := o.SetUseGrvCache(); err != nil {
		return false, err
	}
	for _, tag := range p.tags {
		if err := o.SetAutoThrottleTag(tag); err != nil {
			return false, err
		}
	}
	if err := rc.Configure(recordTransactionConfigurer(ctx)); err != nil {
		return false, err
	}
	if p.observe != nil {
		p.observe(rc)
	}
	err = fn(rc)
	code, isFDB := fdbErrorCode(err)
	if err == nil || !isFDB || !mayRetry {
		return false, err
	}
	// OnError waits out the backoff and fails for a code that is not retryable.
	if rc.Transaction().OnError(fdb.Error{Code: code}).Get() != nil {
		return false, err
	}
	return true, nil
}

// OpenReader opens the OTEL_V1 store of database (/DOMAIN/DATABASE) and
// schema on db, which the caller owns and closes. It creates nothing: an
// unknown database or schema is config, and a schema of another template, or
// whose store no Exporter has opened (no STORE_META("layout")), is ownership.
func OpenReader(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg ReaderConfig) (*Reader, error) {
	const op = "open reader"
	if db == nil {
		return nil, fail("config", op, "an FDB database is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	// The path resolves only in a registered domain (keyspace.ToDatabasePath).
	if parts, err := keyspace.ParseDBPath(database); err == nil && len(parts) == 2 && parts[0] != "" {
		keyspace.RegisterDomainIfNotExists(strings.ToUpper(parts[0]))
	}
	// Reads uncompressed and zlib records alike; the Reader never writes one.
	serializer, err := RecordSerializer("zlib-default")
	if err != nil {
		return nil, err
	}
	txns := &readerTxns{tags: cfg.TransactionTags}
	ctx = withExporterTxns(ctx, cfg.TransactionTimeout)
	ts, err := otelsql.OpenTenantStoreCompiledIn(func(fn func(*recordlayer.FDBRecordContext) error) error {
		return txns.run(ctx, db, op, fn)
	}, database, schema, serializer)
	if err != nil {
		return nil, readerOpenError(ctx, op, err)
	}
	store := &tenantStore{db: db, ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema, reader: txns}
	var layout string
	err = store.read(ctx, op, func(s *recordlayer.FDBRecordStore) error {
		row, ok, err := loadStoreMeta(s, otelsql.StoreMetaLayout)
		if err != nil {
			return err
		}
		if !ok || row.Str == nil {
			return failf("ownership", op, "store %s/%s has no layout: no Exporter has opened it", ts.DatabasePath, ts.Schema)
		}
		layout = *row.Str
		return nil
	})
	if err != nil {
		return nil, readerOpenError(ctx, op, err)
	}
	messages, ok := map[string]string{StoreLayoutNone: messagesNone, StoreLayoutDeltas: messagesDeltas, StoreLayoutFullText: messagesFullText}[layout]
	if !ok {
		return nil, failf("ownership", op, "store %s/%s has layout %q, unknown to this build", ts.DatabasePath, ts.Schema, layout)
	}
	logs, err := newRelLogStore(store, ExporterConfig{K: cfg.K, MaxCandidates: cfg.MaxCandidates, MaxBytes: cfg.MaxBytes})
	if err != nil {
		return nil, err
	}
	return &Reader{
		cfg: cfg, store: store, layout: layout, logs: logs,
		spans: &SpanReader{store: store, cfg: Config{K: cfg.K, MaxCandidates: cfg.MaxCandidates, MaxBytes: cfg.MaxBytes, Messages: messages}, keys: storeMetaContinuationKeys{}},
	}, nil
}

// readerOpenError classifies an open failure as relOpenError does; a
// database or schema the catalog does not name (UNDEFINED_SCHEMA for both)
// is config.
func readerOpenError(ctx context.Context, op string, err error) error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUndefinedSchema {
		return wrap("config", op, err)
	}
	return relOpenError(ctx, op, err)
}

// Layout is the store's STORE_META("layout"): StoreLayoutNone,
// StoreLayoutDeltas or StoreLayoutFullText.
func (r *Reader) Layout() string { return r.layout }

// Close ends the Reader: later calls fail. The FDB database stays the caller's.
func (r *Reader) Close() error {
	r.closed.Store(true)
	return nil
}

// begin refuses work after Close and applies the Reader's transaction policy.
func (r *Reader) begin(ctx context.Context, op string) (context.Context, error) {
	if r.closed.Load() {
		return ctx, fail("operation", op, "reader is closed")
	}
	return withExporterTxns(ctx, r.cfg.TransactionTimeout), nil
}

// Query returns the newest K distinct traces with a span carrying a.
func (r *Reader) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	ctx, err := r.begin(ctx, "query")
	if err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	return r.spans.Query(ctx, a)
}

// Time returns the newest K distinct traces with a span starting in tr.
func (r *Reader) Time(ctx context.Context, tr TimeRange) (QueryResult, error) {
	ctx, err := r.begin(ctx, "time")
	if err != nil {
		return QueryResult{Hits: []Hit{}}, err
	}
	return r.spans.Time(ctx, tr)
}

// QuerySameSpan returns one verified page of same-span matches; see
// SpanQueryPage for its continuation.
func (r *Reader) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	ctx, err := r.begin(ctx, "same-span query")
	if err != nil {
		return SpanQueryPage{Spans: []SpanMatch{}}, err
	}
	return r.spans.QuerySameSpan(ctx, q)
}

// QueryTraceExistential returns the newest traces satisfying every clause;
// message clauses need the fulltext layout.
func (r *Reader) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	ctx, err := r.begin(ctx, "trace existential")
	if err != nil {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, err
	}
	return r.spans.QueryTraceExistential(ctx, q)
}

// QueryLogsByTrace returns the trace's logs, or one span's, in (effective
// time, event ID) order.
func (r *Reader) QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error) {
	ctx, err := r.begin(ctx, "log trace query")
	if err != nil {
		return []LogRecord{}, err
	}
	return r.logs.QueryLogsByTrace(ctx, traceID, spanID)
}

// QueryLogsByAttribute returns up to limit (0: K) logs carrying a, newest first.
func (r *Reader) QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error) {
	ctx, err := r.begin(ctx, "log attribute query")
	if err != nil {
		return []LogRecord{}, err
	}
	return r.logs.QueryLogsByAttribute(ctx, a, limit)
}

// TraceOTLP returns the trace's spans in one scan of its SPANS rows, each as
// the single-span ResourceSpans LoadSpanOTLP returns, in span ID order, with
// their GenAI message values restored when the layout stores messages. A
// trace past b is returned in part (TraceReadStats); an unknown trace is
// empty.
func (r *Reader) TraceOTLP(ctx context.Context, traceID string, b TraceBudget) ([]*tracepb.ResourceSpans, TraceReadStats, error) {
	const op = "trace otlp"
	out := []*tracepb.ResourceSpans{}
	ctx, err := r.begin(ctx, op)
	if err != nil {
		return out, TraceReadStats{}, err
	}
	if !validIdentity(traceID, 16) {
		return out, TraceReadStats{}, fail("config", op, "expected a lowercase hex nonzero 16-byte trace identifier")
	}
	if b.MaxSpans == 0 {
		b.MaxSpans = defaultTraceMaxSpans
	}
	if b.MaxBytes == 0 {
		b.MaxBytes = defaultTraceMaxBytes
	}
	if b.MaxSpans < 0 || b.MaxBytes < 0 {
		return out, TraceReadStats{}, fail("config", op, "trace budget must not be negative")
	}
	trace, _ := hex.DecodeString(traceID)
	var stats TraceReadStats
	err = r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		var reasons []string
		work := &queryWork{maxCandidates: math.MaxInt, maxBytes: b.MaxBytes}
		spans, reason, err := scanTraceOTLP(ctx, store, op, trace, b, work)
		if err != nil {
			return err
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
		if r.spans.cfg.Messages != messagesNone && len(spans) > 0 {
			p, err := planReconstruct(ctx, spans)
			if err != nil {
				return err
			}
			if len(p.sites) > 0 {
				restored := false
				if reason != TraceReasonMaxBytes {
					raws, ok, err := loadRelMessages(store, p.refs, p.encodings, work, b.MaxBytes)
					if err != nil {
						return err
					}
					if ok {
						if spans, err = p.apply(raws); err != nil {
							return err
						}
						restored = true
					}
				}
				if !restored {
					reasons = append(reasons, TraceReasonMessages)
				}
			}
		}
		out = spans
		stats = TraceReadStats{Spans: len(spans), Bytes: work.bytes, Truncated: len(reasons) > 0, Reason: strings.Join(reasons, ",")}
		return nil
	})
	if err != nil {
		return []*tracepb.ResourceSpans{}, TraceReadStats{}, err
	}
	return out, stats, nil
}

// scanTraceOTLP reads the trace's SPANS rows in primary-key (span ID) order
// until b's span count or bytes, charging work's bytes, and decodes each
// row's single-span ResourceSpans; reason names the limit that stopped it.
func scanTraceOTLP(ctx context.Context, store *recordlayer.FDBRecordStore, op string, trace []byte, b TraceBudget, work *queryWork) ([]*tracepb.ResourceSpans, string, error) {
	out := []*tracepb.ResourceSpans{}
	rt := store.GetRecordMetaData().GetRecordType(spansRecordType)
	if rt == nil {
		return nil, "", failf("ownership", op, "%s is not in the store's metadata", spansRecordType)
	}
	spanF := rt.Descriptor.Fields().ByName("SPAN_ID")
	if spanF == nil {
		return nil, "", failf("ownership", op, "%s lacks SPAN_ID", spansRecordType)
	}
	prefix := recordTypeKey(rt, trace)
	props := recordlayer.ForwardScan()
	// One row past the span budget, so a trace over it is seen.
	props.ExecuteProperties.ReturnedRowLimit = b.MaxSpans + 1
	cur := store.ScanRecordsInRange(prefix, prefix, recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive, nil, props)
	defer cur.Close()
	for {
		res, err := cur.OnNext(ctx)
		if err != nil {
			return nil, "", err
		}
		if !res.HasNext() {
			if reason := res.GetNoNextReason(); reason != recordlayer.SourceExhausted {
				return nil, "", failf("operation", op, "trace scan stopped: %v", reason)
			}
			return out, "", nil
		}
		if len(out) == b.MaxSpans {
			return out, TraceReasonMaxSpans, nil
		}
		rec := res.GetValue()
		n := int64(rec.KeySize + rec.ValueSize)
		if work.bytes+n > b.MaxBytes {
			return out, TraceReasonMaxBytes, nil
		}
		work.candidates++
		work.bytes += n
		m := rec.Record.ProtoReflect()
		rs, err := rowOTLP(op, m, trace, m.Get(spanF).Bytes())
		if err != nil {
			return nil, "", err
		}
		out = append(out, rs)
	}
}
