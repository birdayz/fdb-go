// Package sqlbackend is otelbench's relational backend: the fixture corpus in
// OTEL_V1 tenant schema stores, queried through the SQL driver.
//
// Ingest writes the stores' records directly through the Record Layer
// ("direct", the same SaveRecordBatch path as the FDB backend) or through SQL
// INSERT ("insert"). Every query is SQL; results are checked against the
// same independent oracle as the other backends.
package sqlbackend

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqldriver"
	"google.golang.org/protobuf/proto"
)

// Ingest modes.
const (
	IngestDirect = "direct"
	IngestInsert = "insert"
)

// Query modes.
const (
	// QuerySQL runs every query as SQL through the driver.
	QuerySQL = "sql"
	// QueryDirect runs the fixture's queries as Record Layer scans of the
	// same OTEL_V1 indexes SQL plans: one transaction per query, as the FDB
	// backend's. SQL remains the path for everything else (joins, ad hoc).
	QueryDirect = "direct"
)

// Row representations of direct ingest.
const (
	// RowsCompiled saves the compiled OTEL_V1 types (otelv1pb).
	RowsCompiled = "compiled"
	// RowsDynamic saves dynamic messages of the catalog's descriptor.
	RowsDynamic = "dynamic"
)

// DefaultPageRows is the default SQL page bound (EXECUTION_SCANNED_ROWS_LIMIT):
// a SELECT page otherwise reads every matching row before the first is
// returned, though the top-K queries stop after K distinct traces.
const DefaultPageRows = 256

// Options select the backend's ingest and query paths.
type Options struct {
	Ingest   string // IngestDirect (default) or IngestInsert
	Query    string // QuerySQL (default) or QueryDirect
	PageRows int    // SQL scanned rows per page; 0 is DefaultPageRows, negative unbounded
	// Rows is direct ingest's row representation: RowsCompiled (default) or
	// RowsDynamic.
	Rows string
	// PinnedConn runs every SQL query on one *sql.Conn instead of the pool.
	// database/sql resets a pooled connection's session (and with it the
	// cached schema) at every checkout; a pinned one keeps it, so its
	// statements skip the catalog read and see schema changes only when
	// the session is reset.
	PinnedConn bool
}

// queryer is *sql.DB or a pinned *sql.Conn.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// backendKeys numbers the backends' driver keys within the process.
var backendKeys atomic.Int64

// domain is the relational domain of the runs' databases: /OTELBENCH/R_<RUN>.
const domain = "OTELBENCH"

// Backend implements otel.Backend and otel.IntrinsicBackend over OTEL_V1.
type Backend struct {
	cfg     otel.Config
	cluster string
	// driverKey is the cluster_file key under which the SQL driver runs over
	// this backend's FDB client handle (sqldriver.RegisterBackend).
	driverKey string
	mode      string
	opts      Options
	raw       fdb.Database
	db        *recordlayer.FDBDatabase
	dbPath    string
	query     *sql.DB // schema of cfg.Tenant
	q         queryer // query, or its pinned connection
	pinned    *sql.Conn
	reader    *otel.SpanReader // the direct query path's
	// writeSer is the direct saves' serializer (nil: uncompressed); readSer
	// is the relational engine's, which reads uncompressed and zlib records.
	writeSer, readSer *recordlayer.TransformedRecordSerializer
	lastWork          otel.QueryResult
	explained         map[string]bool // with OTELBENCH_SQL_EXPLAIN, the query texts already explained
	// With OTELBENCH_SQL_RPCS, the client counters at the first query; Close
	// reports the FDB requests per query since (a diagnostic).
	rpcStart *client.ClientMetricsSnapshot
	queries  int64

	unregister func() // the driver's backend registration
}

// Open opens the run's relational database; Ingest creates it.
func Open(ctx context.Context, c otel.Config, cluster string, opts Options) (*Backend, error) {
	c.Backend = "sql"
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if opts.Ingest == "" {
		opts.Ingest = IngestDirect
	}
	if opts.Ingest != IngestDirect && opts.Ingest != IngestInsert {
		return nil, fmt.Errorf("sqlbackend: ingest mode must be %s or %s", IngestDirect, IngestInsert)
	}
	if opts.Query == "" {
		opts.Query = QuerySQL
	}
	if opts.Query != QuerySQL && opts.Query != QueryDirect {
		return nil, fmt.Errorf("sqlbackend: query mode must be %s or %s", QuerySQL, QueryDirect)
	}
	if opts.PageRows == 0 {
		opts.PageRows = DefaultPageRows
	}
	if opts.Rows == "" {
		opts.Rows = RowsCompiled
	}
	if opts.Rows != RowsCompiled && opts.Rows != RowsDynamic {
		return nil, fmt.Errorf("sqlbackend: rows must be %s or %s", RowsCompiled, RowsDynamic)
	}
	mode := opts.Ingest
	// Direct saves write --record-compression (Java-readable zlib or none,
	// both of which SQL reads); SQL INSERT always writes the engine's own
	// zlib-default records.
	writeSer, err := otel.RecordSerializer(c.RecordCompression)
	if err != nil {
		return nil, err
	}
	if mode == IngestInsert && c.RecordCompression != "" && c.RecordCompression != "none" && c.RecordCompression != "zlib-default" {
		return nil, errors.New("sqlbackend: SQL INSERT writes zlib-default records; --record-compression must be none or zlib-default")
	}
	readSer, err := otel.RecordSerializer("zlib-default")
	if err != nil {
		return nil, err
	}
	if cluster == "" {
		return nil, errors.New("sqlbackend: explicit owned FDB cluster file required")
	}
	if err := fdb.APIVersion(730); err != nil {
		return nil, err
	}
	sqldriver.RegisterDomainIfNotExists(domain)
	cf, err := client.ParseClusterFile(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := fdb.OpenDatabaseFromConfig(ctx, cf)
	if err != nil {
		return nil, err
	}
	b := &Backend{
		cfg: c, cluster: cluster, mode: mode, opts: opts, raw: raw, db: recordlayer.NewFDBDatabase(raw),
		dbPath:   "/" + domain + "/R_" + strings.ToUpper(c.Run),
		writeSer: writeSer, readSer: readSer,
	}
	// The SQL driver runs over the same FDB client handle, configured as the
	// driver configures the database it opens itself, so one set of client
	// counters covers every request (OTELBENCH_SQL_RPCS). The driver keys
	// its databases by the DSN's cluster_file, so each backend registers its
	// own key: backends in one process must not share a handle one of them
	// closes.
	driverDB := recordlayer.NewFDBDatabase(raw)
	driverDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	b.driverKey = "otelbench-backend-" + strconv.FormatInt(backendKeys.Add(1), 10)
	b.unregister = sqldriver.RegisterBackend(b.driverKey, driverDB)
	qdsn := b.dsn(tenantSchema(c.Tenant))
	if opts.PageRows > 0 {
		qdsn += "&execution_scanned_rows_limit=" + strconv.Itoa(opts.PageRows)
	}
	if b.query, err = sql.Open("fdbsql", qdsn); err != nil {
		raw.Close()
		return nil, err
	}
	b.q = b.query
	if opts.PinnedConn {
		if b.pinned, err = b.query.Conn(ctx); err != nil {
			b.query.Close()
			raw.Close()
			return nil, err
		}
		b.q = b.pinned
	}
	return b, nil
}

func tenantSchema(tenant int) string { return "T" + strconv.Itoa(tenant) }

// effectiveCompression is the record format ingest writes.
func (b *Backend) effectiveCompression() string {
	if b.mode == IngestInsert {
		return "zlib-default"
	}
	if b.cfg.RecordCompression == "" {
		return "none"
	}
	return b.cfg.RecordCompression
}

func (b *Backend) dsn(schema string) string {
	d := fmt.Sprintf("fdbsql://%s?cluster_file=%s", b.dbPath, b.driverKey)
	if schema != "" {
		d += "&schema=" + schema
	}
	return d
}

func (b *Backend) Close() {
	if b.rpcStart != nil {
		m, n := b.raw.Metrics(), b.queries
		if n == 0 {
			n = 1
		}
		per := func(after, before int64) float64 { return float64(after-before) / float64(n) }
		fmt.Fprintf(os.Stderr, "OTELBENCH_SQL_RPCS queries=%d per query: grv=%.2f get_range=%.2f get_value=%.2f commits=%.2f\n", b.queries,
			per(m.TransactionReadVersionsCompleted, b.rpcStart.TransactionReadVersionsCompleted),
			per(m.TransactionGetRangeRequests, b.rpcStart.TransactionGetRangeRequests),
			per(m.ReadLatency.Count, b.rpcStart.ReadLatency.Count),
			per(m.TransactionsCommitStarted, b.rpcStart.TransactionsCommitStarted))
	}
	if b.pinned != nil {
		b.pinned.Close()
	}
	b.query.Close()
	b.unregister()
	b.raw.Close()
}

func (b *Backend) LastQueryWork() otel.QueryResult { return b.lastWork }

// countQuery counts a query for the OTELBENCH_SQL_RPCS report.
func (b *Backend) countQuery() {
	if b.rpcStart == nil && os.Getenv("OTELBENCH_SQL_RPCS") != "" {
		m := b.raw.Metrics()
		b.rpcStart = &m
	}
	b.queries++
}

// explain writes the plan of each distinct query text to stderr once when
// OTELBENCH_SQL_EXPLAIN is set; a diagnostic, outside the measured results.
func (b *Backend) explain(ctx context.Context, q string, args ...any) {
	if os.Getenv("OTELBENCH_SQL_EXPLAIN") == "" || b.explained[q] {
		return
	}
	if b.explained == nil {
		b.explained = map[string]bool{}
	}
	b.explained[q] = true
	var plan string
	if err := b.q.QueryRowContext(ctx, "EXPLAIN "+q, args...).Scan(&plan); err != nil {
		fmt.Fprintf(os.Stderr, "EXPLAIN %s: %v\n", q, err)
		return
	}
	fmt.Fprintf(os.Stderr, "EXPLAIN %s\n  %s\n", q, plan)
}

// create makes the template (once per cluster), the run's database (which must
// not exist) and one schema per tenant.
func (b *Backend) create(ctx context.Context) error {
	setup, err := sql.Open("fdbsql", b.dsn(""))
	if err != nil {
		return err
	}
	defer setup.Close()
	var apiErr *api.Error
	if err := otelsql.CreateTemplate(ctx, setup); err != nil && (!errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDuplicateSchemaTemplate) {
		return err
	}
	if _, err := setup.ExecContext(ctx, `CREATE DATABASE "`+b.dbPath+`"`); err != nil {
		if errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeDatabaseAlreadyExists {
			return errors.New("sqlbackend: run already exists; choose a fresh namespace or drop it")
		}
		return err
	}
	for t := 0; t < b.cfg.Tenants; t++ {
		if err := otelsql.CreateTenant(ctx, setup, b.dbPath, tenantSchema(t)); err != nil {
			return err
		}
	}
	return nil
}

type batch struct {
	tenant   int
	spans    []otel.Span
	records  []proto.Message
	logical  int64
	estimate int
}

// Ingest writes every fixture span into its tenant's store, one transaction
// per batch of at most Batch spans of one tenant.
func (b *Backend) Ingest(ctx context.Context) (otel.Statistics, error) {
	if err := b.create(ctx); err != nil {
		return otel.Statistics{}, err
	}
	stores := make([]*otelsql.TenantStore, b.cfg.Tenants)
	rows := make([]*otelsql.SpanRows, b.cfg.Tenants)
	inserts := make([]*sql.DB, b.cfg.Tenants)
	compiled := b.mode == IngestDirect && b.opts.Rows != RowsDynamic
	for t := range stores {
		var err error
		open := otelsql.OpenTenantStore
		if compiled {
			open = otelsql.OpenTenantStoreCompiled
		}
		if stores[t], err = open(ctx, b.db, b.dbPath, tenantSchema(t), b.writeSer); err != nil {
			return otel.Statistics{}, err
		}
		if rows[t], err = otelsql.NewSpanRows(stores[t].MetaData); err != nil {
			return otel.Statistics{}, err
		}
		if b.mode == IngestInsert {
			if inserts[t], err = sql.Open("fdbsql", b.dsn(tenantSchema(t))); err != nil {
				return otel.Statistics{}, err
			}
			defer inserts[t].Close()
		}
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *batch, b.cfg.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr, produceErr error
	var count, batches, logical int64
	for w := 0; w < b.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for bt := range jobs {
				var err error
				if b.mode == IngestInsert {
					err = b.insertBatch(workCtx, inserts[bt.tenant], bt)
				} else {
					err = b.saveBatch(workCtx, stores[bt.tenant], bt)
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
				count += int64(len(bt.spans))
				batches++
				logical += bt.logical
				mu.Unlock()
			}
		}()
	}
	pending := make([]*batch, b.cfg.Tenants)
	send := func(bt *batch) bool {
		select {
		case jobs <- bt:
			return true
		case <-workCtx.Done():
			return false
		}
	}
produce:
	for i := 0; i < b.cfg.Count; i++ {
		s := otel.Generate(b.cfg, i)
		cost := 2048
		for _, a := range s.Attributes {
			cost += 2 * (len(a.Key) + len(a.Value) + 256)
		}
		bt := pending[s.Tenant]
		if bt != nil && (len(bt.spans) == b.cfg.Batch || bt.estimate+cost > 300000) {
			if !send(bt) {
				break produce
			}
			bt = nil
		}
		if bt == nil {
			bt = &batch{tenant: s.Tenant}
			pending[s.Tenant] = bt
		}
		wire, err := json.Marshal(s)
		if err != nil {
			produceErr = err
			break
		}
		bt.spans = append(bt.spans, s)
		bt.logical += int64(len(wire))
		bt.estimate += cost
		if b.mode == IngestDirect {
			m, err := buildRow(rows[s.Tenant], compiled, s)
			if err != nil {
				produceErr = err
				break
			}
			bt.records = append(bt.records, m)
		}
	}
	for _, bt := range pending {
		if bt != nil && len(bt.spans) > 0 && produceErr == nil && !send(bt) {
			break
		}
	}
	close(jobs)
	wg.Wait()
	stats := otel.Statistics{Rows: count, Attributes: count * int64(b.cfg.Attributes), LogicalPayloadBytes: logical, Details: map[string]string{
		"ingest":             "OTEL_V1 tenant schema stores; " + b.mode,
		"committed_batches":  strconv.FormatInt(batches, 10),
		"save_mode":          b.cfg.SaveMode,
		"workers":            strconv.Itoa(b.cfg.Workers),
		"record_compression": b.effectiveCompression(),
	}}
	if produceErr != nil {
		return stats, produceErr
	}
	if firstErr != nil {
		return stats, firstErr
	}
	if count != int64(b.cfg.Count) {
		return stats, errors.New("sqlbackend: incomplete source population")
	}
	return stats, ctx.Err()
}

func buildRow(r *otelsql.SpanRows, compiled bool, s otel.Span) (proto.Message, error) {
	rs, wire, err := otel.FixtureOTLP(s)
	if err != nil {
		return nil, err
	}
	ss := rs.ScopeSpans[0]
	span := otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: ss.Spans[0], OTLP: wire}
	if compiled {
		return otelsql.BuildCompiled(span)
	}
	return r.Build(span)
}

func (b *Backend) saveBatch(ctx context.Context, ts *otelsql.TenantStore, bt *batch) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		builder := ts.Builder(rc)
		var prefetch *recordlayer.RecordBatchPrefetch
		if b.cfg.SaveMode == "batch" {
			prefetch = builder.PrefetchRecordBatch(bt.records)
		}
		store, err := builder.Open()
		if err != nil {
			return nil, err
		}
		if prefetch != nil {
			if err := prefetch.Wait(); err != nil {
				return nil, err
			}
		}
		switch b.cfg.SaveMode {
		case "batch":
			_, err = store.SaveRecordBatch(bt.records)
		case "pipelined":
			_, err = store.SaveRecordsPipelined(bt.records, recordlayer.RecordExistenceCheckNone)
		default:
			for _, m := range bt.records {
				if _, err = store.SaveRecord(m); err != nil {
					break
				}
			}
		}
		return nil, err
	})
	return err
}

// insertBatch is one multi-row INSERT of the batch, columns as buildRow sets them.
func (b *Backend) insertBatch(ctx context.Context, db *sql.DB, bt *batch) error {
	var sb strings.Builder
	sb.WriteString("INSERT INTO spans (trace_id, span_id, flags, name, kind, start_ns, end_ns, scope_name, scope_version, " +
		"resource_schema_url, scope_schema_url, dropped_attributes, dropped_events, dropped_links, str_attrs, int_attrs, otlp) VALUES ")
	for i, s := range bt.spans {
		_, wire, err := otel.FixtureOTLP(s)
		if err != nil {
			return err
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "(X'%s', X'%s', 0, 'immutable-fixture', 0, %d, %d, 'otelbench', '2', 'otelbench-resource-v2', 'otelbench-scope-v2', 0, 0, 0, ",
			s.Trace, s.ID, s.Start, s.Start+1000)
		var strs, ints []string
		for _, a := range s.Attributes {
			switch a.Type {
			case "string":
				strs = append(strs, fmt.Sprintf("(%s, %s, %s)", quote(a.Scope), quote(a.Key), quote(a.Value)))
			case "int":
				ints = append(ints, fmt.Sprintf("(%s, %s, %s)", quote(a.Scope), quote(a.Key), a.Value))
			}
		}
		sb.WriteString(arrayLiteral(strs))
		sb.WriteString(", ")
		sb.WriteString(arrayLiteral(ints))
		sb.WriteString(", X'")
		sb.WriteString(hex.EncodeToString(wire))
		sb.WriteString("')")
	}
	_, err := db.ExecContext(ctx, sb.String())
	return err
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func arrayLiteral(elems []string) string {
	if len(elems) == 0 {
		return "NULL"
	}
	return "[" + strings.Join(elems, ", ") + "]"
}

const (
	// attrQuery and timeQuery are top-K bases: topTraces appends the keyset
	// bound, the newest-first order and the batch LIMIT.
	attrQuery = "SELECT s.trace_id, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.%s AS a " +
		"WHERE a.scope = ? AND a.attr_key = ? AND a.%s = ?)"
	traceQuery = "SELECT span_id, start_ns FROM spans WHERE trace_id = ?"
	spanQuery  = "SELECT trace_id, span_id, start_ns FROM spans WHERE span_id = ?"
	timeQuery  = "SELECT s.trace_id, s.start_ns FROM spans AS s WHERE s.start_ns >= ? AND s.start_ns < ?"
)

// Query returns the newest K distinct traces with a span carrying a.
func (b *Backend) Query(ctx context.Context, a otel.Attribute) (otel.QueryResult, error) {
	b.countQuery()
	if b.opts.Query == QueryDirect {
		return b.directQuery(ctx, a)
	}
	var q string
	var v any
	switch a.Type {
	case "string":
		if len(a.Value) > otelsql.MaxIndexedStringBytes {
			return otel.QueryResult{}, errors.New("sqlbackend: long string attributes are not supported by the fixture")
		}
		q, v = fmt.Sprintf(attrQuery, "str_attrs", "str_value"), a.Value
	case "int":
		n, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil {
			// No stored integer equals a non-integer literal.
			return otel.QueryResult{Hits: []otel.Hit{}}, nil
		}
		q, v = fmt.Sprintf(attrQuery, "int_attrs", "int_value"), n
	default:
		return otel.QueryResult{Hits: []otel.Hit{}}, nil
	}
	return b.topTraces(ctx, q, a.Scope, a.Key, v)
}

// Time returns the newest K distinct traces with a span starting in r.
func (b *Backend) Time(ctx context.Context, r otel.TimeRange) (otel.QueryResult, error) {
	b.countQuery()
	if err := r.Validate(); err != nil {
		return otel.QueryResult{}, err
	}
	if b.opts.Query == QueryDirect {
		return b.directTime(ctx, r)
	}
	return b.topTraces(ctx, timeQuery, r.Start, r.End)
}

// topTraces reads base's rows newest-first (start desc, then index order) and
// keeps each trace's first, until K distinct traces. It reads in batches of
// K, 2K, 4K, ... rows: a statement's page is read whole before its first row is
// returned, and a top-K reader usually needs far fewer rows than an unbounded
// page. A later batch resumes at the last start read, inclusive (rows sharing
// it are read again; their traces are already seen), and a batch whose rows
// all share that start doubles the next one, so every batch advances.
func (b *Backend) topTraces(ctx context.Context, base string, args ...any) (otel.QueryResult, error) {
	out := otel.QueryResult{Hits: []otel.Hit{}}
	seen := map[string]bool{}
	limit := b.cfg.K
	var upper *int64
	for {
		q, qargs := base, args
		if upper != nil {
			q, qargs = base+" AND s.start_ns <= ?", append(append([]any(nil), args...), *upper)
		}
		q += " ORDER BY s.start_ns DESC LIMIT " + strconv.Itoa(limit)
		if upper == nil {
			b.explain(ctx, q, qargs...)
		}
		n, last, err := b.topBatch(ctx, q, qargs, seen, &out)
		if err != nil || len(out.Hits) == b.cfg.K || n < limit {
			b.lastWork = out
			return out, err
		}
		if out.Candidates > b.cfg.MaxCandidates {
			b.lastWork = out
			return out, fmt.Errorf("sqlbackend: top-K read %d candidates, budget %d", out.Candidates, b.cfg.MaxCandidates)
		}
		upper = &last
		limit *= 2
	}
}

// topBatch reads one batch into out; n is its row count and last the start
// of its last row.
func (b *Backend) topBatch(ctx context.Context, q string, args []any, seen map[string]bool, out *otel.QueryResult) (n int, last int64, err error) {
	rs, err := b.q.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rs.Close()
	for len(out.Hits) < b.cfg.K && rs.Next() {
		var trace []byte
		if err := rs.Scan(&trace, &last); err != nil {
			return n, last, err
		}
		n++
		out.Candidates++
		out.Bytes += int64(len(trace) + 8)
		t := hex.EncodeToString(trace)
		if !seen[t] {
			seen[t] = true
			out.Hits = append(out.Hits, otel.Hit{Trace: t, Latest: last})
		}
	}
	return n, last, rs.Err()
}

// Trace returns a trace's spans by (start, span).
func (b *Backend) Trace(ctx context.Context, trace string) ([]otel.SpanHit, error) {
	b.countQuery()
	if b.opts.Query == QueryDirect {
		return b.directTrace(ctx, trace)
	}
	id, err := hex.DecodeString(trace)
	if err != nil {
		return nil, err
	}
	b.explain(ctx, traceQuery, id)
	rs, err := b.q.QueryContext(ctx, traceQuery, id)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []otel.SpanHit{}
	work := otel.QueryResult{}
	for rs.Next() {
		var span []byte
		var start int64
		if err := rs.Scan(&span, &start); err != nil {
			return nil, err
		}
		work.Candidates++
		work.Bytes += int64(len(span) + 8)
		out = append(out, otel.SpanHit{ID: hex.EncodeToString(span), Start: start})
	}
	sortSpanHits(out)
	b.lastWork = work
	return out, rs.Err()
}

// sortSpanHits orders a trace's spans by (start, span), as the oracle does.
func sortSpanHits(out []otel.SpanHit) {
	sort.Slice(out, func(i, j int) bool {
		return out[i].Start < out[j].Start || (out[i].Start == out[j].Start && out[i].ID < out[j].ID)
	})
}

// Span returns every span with the identity.
func (b *Backend) Span(ctx context.Context, span string) ([]otel.SpanMatch, error) {
	b.countQuery()
	if b.opts.Query == QueryDirect {
		return b.directSpan(ctx, span)
	}
	id, err := hex.DecodeString(span)
	if err != nil {
		return nil, err
	}
	b.explain(ctx, spanQuery, id)
	rs, err := b.q.QueryContext(ctx, spanQuery, id)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []otel.SpanMatch{}
	work := otel.QueryResult{}
	for rs.Next() {
		var trace, sp []byte
		var start int64
		if err := rs.Scan(&trace, &sp, &start); err != nil {
			return nil, err
		}
		work.Candidates++
		work.Bytes += int64(len(trace) + len(sp) + 8)
		out = append(out, otel.SpanMatch{Trace: hex.EncodeToString(trace), ID: hex.EncodeToString(sp), Start: start})
	}
	b.lastWork = work
	return out, rs.Err()
}

// Audit counts each tenant's rows.
func (b *Backend) Audit(ctx context.Context) (otel.Statistics, error) {
	stats := otel.Statistics{Structures: map[string]otel.Size{}, Details: map[string]string{"storage": "FDB logical KV bytes of the tenant schema stores; catalog, replication/logs/engine compression excluded"}}
	for t := 0; t < b.cfg.Tenants; t++ {
		db, err := sql.Open("fdbsql", b.dsn(tenantSchema(t)))
		if err != nil {
			return stats, err
		}
		var n int64
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spans").Scan(&n)
		db.Close()
		if err != nil {
			return stats, err
		}
		stats.Rows += n
		ts, err := otelsql.OpenTenantStore(ctx, b.db, b.dbPath, tenantSchema(t), nil)
		if err != nil {
			return stats, err
		}
		if err := b.sizeStore(ctx, ts, stats.Structures); err != nil {
			return stats, err
		}
	}
	if stats.Rows != int64(b.cfg.Count) {
		return stats, fmt.Errorf("sqlbackend: audit found %d rows, want %d", stats.Rows, b.cfg.Count)
	}
	stats.Attributes = stats.Rows * int64(b.cfg.Attributes)
	return stats, nil
}

// sizeStore adds a store's KV sizes to out by structure: "records", each
// index by name, other store subspaces by key, and "owned_total".
func (b *Backend) sizeStore(ctx context.Context, ts *otelsql.TenantStore, out map[string]otel.Size) error {
	begin, end := ts.Subspace.FDBRangeKeys()
	lo, hi := begin.FDBKey(), end.FDBKey()
	var prev []byte
	const page = 2000
	for {
		var rows []fdb.KeyValue
		_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			var err error
			rows, err = rc.ReadTransaction(true).GetRange(fdb.KeyRange{Begin: lo, End: hi}, fdb.RangeOptions{Limit: page}).GetSliceWithError()
			return nil, err
		})
		if err != nil {
			return err
		}
		for _, kv := range rows {
			name := "other"
			if tup, err := ts.Subspace.Unpack(kv.Key); err == nil && len(tup) > 0 {
				switch k := tup[0].(type) {
				case int64:
					switch {
					case k == recordlayer.RecordKey:
						name = "records"
					case k == recordlayer.IndexKey && len(tup) > 1:
						name = fmt.Sprint(tup[1])
					default:
						name = "store_" + strconv.FormatInt(k, 10)
					}
				}
			}
			for _, n := range []string{name, "owned_total"} {
				s := out[n]
				s.AddKV(prev, kv.Key, kv.Value)
				out[n] = s
			}
			prev = kv.Key
		}
		if len(rows) < page {
			return nil
		}
		lo = append(append(fdb.Key(nil), rows[len(rows)-1].Key...), 0)
	}
}

// Drop removes the run's database.
func (b *Backend) Drop(ctx context.Context) error {
	cat, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", b.driverKey))
	if err != nil {
		return err
	}
	defer cat.Close()
	_, err = cat.ExecContext(ctx, `DROP DATABASE "`+b.dbPath+`"`)
	return err
}

var _ interface {
	otel.Backend
	otel.IntrinsicBackend
} = (*Backend)(nil)
