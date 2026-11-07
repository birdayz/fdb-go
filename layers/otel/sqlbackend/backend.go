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

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

// DefaultPageRows is the default SQL page bound (EXECUTION_SCANNED_ROWS_LIMIT):
// a SELECT page otherwise reads every matching row before the first is
// returned, though the top-K queries stop after K distinct traces.
const DefaultPageRows = 256

// Options select the backend's ingest and query paths.
type Options struct {
	Ingest   string // IngestDirect (default) or IngestInsert
	Query    string // QuerySQL (default) or QueryDirect
	PageRows int    // SQL scanned rows per page; 0 is DefaultPageRows, negative unbounded
}

// domain is the relational domain of the runs' databases: /OTELBENCH/R_<RUN>.
const domain = "OTELBENCH"

// Backend implements otel.Backend and otel.IntrinsicBackend over OTEL_V1.
type Backend struct {
	cfg       otel.Config
	cluster   string
	mode      string
	opts      Options
	raw       fdb.Database
	db        *recordlayer.FDBDatabase
	dbPath    string
	query     *sql.DB // schema of cfg.Tenant
	qstore    *otelsql.TenantStore
	lastWork  otel.QueryResult
	explained map[string]bool // with OTELBENCH_SQL_EXPLAIN, the query texts already explained
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
	mode := opts.Ingest
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
		dbPath: "/" + domain + "/R_" + strings.ToUpper(c.Run),
	}
	qdsn := b.dsn(tenantSchema(c.Tenant))
	if opts.PageRows > 0 {
		qdsn += "&execution_scanned_rows_limit=" + strconv.Itoa(opts.PageRows)
	}
	if b.query, err = sql.Open("fdbsql", qdsn); err != nil {
		raw.Close()
		return nil, err
	}
	return b, nil
}

func tenantSchema(tenant int) string { return "T" + strconv.Itoa(tenant) }

func (b *Backend) dsn(schema string) string {
	d := fmt.Sprintf("fdbsql://%s?cluster_file=%s", b.dbPath, b.cluster)
	if schema != "" {
		d += "&schema=" + schema
	}
	return d
}

func (b *Backend) Close() {
	b.query.Close()
	b.raw.Close()
}

func (b *Backend) LastQueryWork() otel.QueryResult { return b.lastWork }

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
	if err := b.query.QueryRowContext(ctx, "EXPLAIN "+q, args...).Scan(&plan); err != nil {
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
	for t := range stores {
		var err error
		if stores[t], err = otelsql.OpenTenantStore(ctx, b.db, b.dbPath, tenantSchema(t), nil); err != nil {
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
			m, err := buildRow(rows[s.Tenant], s)
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
		"ingest":            "OTEL_V1 tenant schema stores; " + b.mode,
		"committed_batches": strconv.FormatInt(batches, 10),
		"save_mode":         b.cfg.SaveMode,
		"workers":           strconv.Itoa(b.cfg.Workers),
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

func buildRow(r *otelsql.SpanRows, s otel.Span) (proto.Message, error) {
	rs, wire, err := otel.FixtureOTLP(s)
	if err != nil {
		return nil, err
	}
	ss := rs.ScopeSpans[0]
	return r.Build(otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: ss.Spans[0], OTLP: wire})
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
	attrQuery = "SELECT s.trace_id, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.%s AS a " +
		"WHERE a.scope = ? AND a.attr_key = ? AND a.%s = ?) ORDER BY s.start_ns DESC"
	traceQuery = "SELECT span_id, start_ns FROM spans WHERE trace_id = ?"
	spanQuery  = "SELECT trace_id, span_id, start_ns FROM spans WHERE span_id = ?"
	timeQuery  = "SELECT trace_id, start_ns FROM spans WHERE start_ns >= ? AND start_ns < ? ORDER BY start_ns DESC"
)

// Query returns the newest K distinct traces with a span carrying a.
func (b *Backend) Query(ctx context.Context, a otel.Attribute) (otel.QueryResult, error) {
	var q, index string
	var v any
	switch a.Type {
	case "string":
		if len(a.Value) > otelsql.MaxIndexedStringBytes {
			return otel.QueryResult{}, errors.New("sqlbackend: long string attributes are not supported by the fixture")
		}
		q, index, v = fmt.Sprintf(attrQuery, "str_attrs", "str_value"), "SPANS_STR_ATTR", a.Value
	case "int":
		n, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil {
			// No stored integer equals a non-integer literal.
			return otel.QueryResult{Hits: []otel.Hit{}}, nil
		}
		q, index, v = fmt.Sprintf(attrQuery, "int_attrs", "int_value"), "SPANS_INT_ATTR", n
	default:
		return otel.QueryResult{Hits: []otel.Hit{}}, nil
	}
	if b.opts.Query == QueryDirect {
		return b.directTop(ctx, index, recordlayer.TupleRangeAllOf(tuple.Tuple{a.Scope, a.Key, v}), 3, 4)
	}
	return b.topTraces(ctx, q, a.Scope, a.Key, v)
}

// Time returns the newest K distinct traces with a span starting in r.
func (b *Backend) Time(ctx context.Context, r otel.TimeRange) (otel.QueryResult, error) {
	if err := r.Validate(); err != nil {
		return otel.QueryResult{}, err
	}
	if b.opts.Query == QueryDirect {
		if r.Start == r.End {
			b.lastWork = otel.QueryResult{}
			return otel.QueryResult{Hits: []otel.Hit{}}, nil
		}
		return b.directTop(ctx, "SPANS_TIME", recordlayer.TupleRangeBetween(tuple.Tuple{r.Start}, tuple.Tuple{r.End}), 0, 1)
	}
	return b.topTraces(ctx, timeQuery, r.Start, r.End)
}

// topTraces reads newest-first (start desc, trace desc) rows and keeps each
// trace's first, until K distinct traces.
func (b *Backend) topTraces(ctx context.Context, q string, args ...any) (otel.QueryResult, error) {
	out := otel.QueryResult{Hits: []otel.Hit{}}
	b.explain(ctx, q, args...)
	rs, err := b.query.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rs.Close()
	seen := map[string]bool{}
	for len(out.Hits) < b.cfg.K && rs.Next() {
		var trace []byte
		var start int64
		if err := rs.Scan(&trace, &start); err != nil {
			return out, err
		}
		out.Candidates++
		out.Bytes += int64(len(trace) + 8)
		t := hex.EncodeToString(trace)
		if !seen[t] {
			seen[t] = true
			out.Hits = append(out.Hits, otel.Hit{Trace: t, Latest: start})
		}
	}
	b.lastWork = out
	return out, rs.Err()
}

// Trace returns a trace's spans by (start, span).
func (b *Backend) Trace(ctx context.Context, trace string) ([]otel.SpanHit, error) {
	id, err := hex.DecodeString(trace)
	if err != nil {
		return nil, err
	}
	if b.opts.Query == QueryDirect {
		return b.directTrace(ctx, id)
	}
	b.explain(ctx, traceQuery, id)
	rs, err := b.query.QueryContext(ctx, traceQuery, id)
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
	id, err := hex.DecodeString(span)
	if err != nil {
		return nil, err
	}
	if b.opts.Query == QueryDirect {
		return b.directSpan(ctx, id)
	}
	b.explain(ctx, spanQuery, id)
	rs, err := b.query.QueryContext(ctx, spanQuery, id)
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
				s.Keys++
				s.KeyBytes += int64(len(kv.Key))
				s.ValueBytes += int64(len(kv.Value))
				out[n] = s
			}
		}
		if len(rows) < page {
			return nil
		}
		lo = append(append(fdb.Key(nil), rows[len(rows)-1].Key...), 0)
	}
}

// Drop removes the run's database.
func (b *Backend) Drop(ctx context.Context) error {
	cat, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", b.cluster))
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
