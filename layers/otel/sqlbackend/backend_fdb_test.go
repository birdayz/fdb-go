package sqlbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// One FDB testcontainer: the OTEL_V1 template is cluster-global, so these
// tests do not share a cluster with suites that expect it absent.
func TestFDB_SQLBackend(t *testing.T) {
	t.Parallel()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { foundationdbtc.TeardownT(t, container) })
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	// The template is created once, before the parallel subtests race for it.
	open := func(t *testing.T, c otel.Config, opts Options) *Backend {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		b, err := Open(ctx, c, cluster, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}
	first := open(t, testConfig("sqlb_template"), Options{})
	tmpl, err := sql.Open("fdbsql", first.dsn(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := otelsql.CreateTemplate(setup, tmpl); err != nil {
		t.Fatal(err)
	}
	// Ingest binds its tenants to the revision-named template.
	if err := otelsql.CreateTemplateNamed(setup, tmpl, otelsql.RevisionTemplateName()); err != nil {
		t.Fatal(err)
	}
	tmpl.Close()

	// Not parallel: it reads the driver key the failed Open registered.
	t.Run("failed open unregisters", func(t *testing.T) {
		testOpenFailureUnregisters(t, cluster)
	})
	t.Run("direct writer rows are SQL rows", func(t *testing.T) {
		t.Parallel()
		testDirectWriterRows(t, cluster)
	})
	t.Run("span reader", func(t *testing.T) {
		t.Parallel()
		testSpanReader(t, cluster)
	})
	t.Run("bench census", func(t *testing.T) {
		t.Parallel()
		testBenchCensus(t, open)
	})
	t.Run("failed ingest counts committed work", func(t *testing.T) {
		t.Parallel()
		testFailedIngestAccounting(t, open)
	})
	for _, c := range []struct {
		name string
		opts Options
		cfg  func(otel.Config) otel.Config
	}{
		{"direct ingest, SQL queries", Options{}, func(c otel.Config) otel.Config { return c }},
		{"direct ingest, direct queries", Options{Query: QueryDirect}, func(c otel.Config) otel.Config { return c }},
		{"direct ingest, unbounded SQL pages", Options{PageRows: -1}, func(c otel.Config) otel.Config { return c }},
		{"pipelined saves", Options{}, func(c otel.Config) otel.Config { c.SaveMode = "pipelined"; return c }},
		{"dynamic rows", Options{Rows: RowsDynamic}, func(c otel.Config) otel.Config { return c }},
		{"zlib default records", Options{}, func(c otel.Config) otel.Config { c.RecordCompression = "zlib-default"; return c }},
		{"zlib 1 records, direct queries", Options{Query: QueryDirect}, func(c otel.Config) otel.Config { c.RecordCompression = "zlib-1"; return c }},
		{"pinned connection", Options{PinnedConn: true}, func(c otel.Config) otel.Config { return c }},
		{"SQL INSERT ingest", Options{Ingest: IngestInsert}, func(c otel.Config) otel.Config { c.Count = 60; return c }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cfg := c.cfg(testConfig("sqlb_" + strings.NewReplacer(" ", "_", ",", "").Replace(strings.ToLower(c.name))))
			b := open(t, cfg, c.opts)
			stats, err := b.Ingest(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Rows != int64(cfg.Count) {
				t.Fatalf("ingested %d rows, want %d", stats.Rows, cfg.Count)
			}
			var owned *otel.OwnershipError
			if _, err := b.Ingest(ctx); !errors.As(err, &owned) || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("re-ingest into the run = %v, want an ownership error: already exists", err)
			}
			audit, err := b.Audit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if audit.Rows != int64(cfg.Count) || audit.Structures["records"].Keys != int64(cfg.Count) ||
				audit.Structures["SPANS_TIME"].Keys != int64(cfg.Count) || audit.Structures["SPANS_SPAN_ID"].Keys != int64(cfg.Count) ||
				audit.Structures["SPANS_STR_ATTR"].Keys == 0 || audit.Structures["SPANS_INT_ATTR"].Keys == 0 {
				t.Fatalf("audit %+v", audit)
			}
			// Every query class of the "all" suite against the independent oracle.
			var e otel.Evidence
			if err := otel.MeasureQueries(ctx, b, cfg, "all", &e); err != nil {
				for i, s := range e.Samples {
					if s.Error != "" && i < 40 {
						t.Logf("sample %d %s: %s", i, s.Class, s.Error)
					}
				}
				t.Fatal(err)
			}
			if e.Failures != 0 || len(e.Samples) == 0 {
				t.Fatalf("query evidence: %d failures of %d samples", e.Failures, len(e.Samples))
			}
			if err := b.Drop(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Audit(ctx); err == nil {
				t.Fatal("audit after drop succeeded")
			}
		})
	}
	t.Run("invalid options", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		requireConfig := func(what string, b *Backend, err error) {
			t.Helper()
			if b != nil {
				b.Close()
			}
			var config *otel.ConfigError
			if !errors.As(err, &config) {
				t.Errorf("%s: Open = %v, want an *otel.ConfigError", what, err)
			}
		}
		for _, opts := range []Options{{Ingest: "bulk"}, {Query: "fast"}, {Rows: "generic"}} {
			b, err := Open(ctx, testConfig("sqlb_invalid"), cluster, opts)
			requireConfig(fmt.Sprintf("%+v", opts), b, err)
		}
		b, err := Open(ctx, testConfig("sqlb_invalid"), "", Options{})
		requireConfig("no cluster file", b, err)
		// SQL INSERT writes the engine's zlib-default records, nothing else.
		c := testConfig("sqlb_invalid")
		c.RecordCompression = "zlib-1"
		b, err = Open(ctx, c, cluster, Options{Ingest: IngestInsert})
		requireConfig("SQL INSERT with zlib-1 records", b, err)
		// Brotli is Go-only: SQL could not read it.
		c.RecordCompression = "brotli-1"
		b, err = Open(ctx, c, cluster, Options{})
		requireConfig("Brotli records", b, err)
	})
}

// A failed ingest reports only the spans and transactions that committed,
// and their logical bytes: the committed prefix of a SpanWriter Write that
// fails after committing, nothing of a failed saveBatch. With one worker
// and one tenant the batches are spans 0-15, 16-31, ... in order. The
// partially written run's audit, a top-K over budget and a long-string
// query give their error kinds; a Write that commits fewer spans without an
// error leaves an incomplete population.
func testFailedIngestAccounting(t *testing.T, open func(*testing.T, otel.Config, Options) *Backend) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	injected := errors.New("injected batch failure")
	logical := func(c otel.Config, n int) int64 {
		var sum int64
		for i := range n {
			wire, err := json.Marshal(otel.Generate(c, i))
			if err != nil {
				t.Fatal(err)
			}
			sum += int64(len(wire))
		}
		return sum
	}
	requireStats := func(name string, stats otel.Statistics, rows, batches int64, c otel.Config) {
		t.Helper()
		if stats.Rows != rows || stats.Details["committed_batches"] != strconv.FormatInt(batches, 10) || stats.LogicalPayloadBytes != logical(c, int(rows)) {
			t.Fatalf("%s: rows %d, committed_batches %s, logical %d; want %d, %d, %d",
				name, stats.Rows, stats.Details["committed_batches"], stats.LogicalPayloadBytes, rows, batches, logical(c, int(rows)))
		}
	}
	runConfig := func(run string) otel.Config {
		c := testConfig(run)
		c.Count, c.Tenants, c.Workers, c.SaveMode = 300, 1, 1, "batch"
		c.K, c.MaxCandidates = 20, 5
		return c
	}

	// The SpanWriter commits the second batch's first 5 spans, then fails.
	c := runConfig("sqlb_failed_writer")
	b := open(t, c, Options{})
	writes := 0
	b.spanWrite = func(w *otel.SpanWriter, ctx context.Context, rss []*tracepb.ResourceSpans) (otel.ExportStats, error) {
		if writes++; writes != 2 {
			return w.Write(ctx, rss)
		}
		st, err := w.Write(ctx, rss[:5])
		if err == nil {
			err = injected
		}
		return st, err
	}
	stats, err := b.Ingest(ctx)
	if !errors.Is(err, injected) {
		t.Fatalf("SpanWriter ingest = %v, want the injected failure", err)
	}
	requireStats("SpanWriter", stats, 21, 2, c)
	var audit *otel.AuditError
	if got, err := b.Audit(ctx); !errors.As(err, &audit) || got.Rows != 21 {
		t.Fatalf("audit of the partial run: %d rows, %v; want 21 rows and an audit error", got.Rows, err)
	}
	// 21 spans of 11 traces carry this attribute: a top-K of 20 reads 20
	// rows of 10 traces and passes MaxCandidates 5.
	var budget *otel.QueryBudgetError
	if _, err := b.Query(ctx, otel.Attribute{Scope: "span", Key: "gen_ai.operation.name", Type: "string", Value: "chat"}); !errors.As(err, &budget) {
		t.Fatalf("top-K over budget: %v, want a budget error", err)
	}
	var config *otel.ConfigError
	if _, err := b.Query(ctx, otel.Attribute{Scope: "span", Key: "k", Type: "string", Value: strings.Repeat("x", otelsql.MaxIndexedStringBytes+1)}); !errors.As(err, &config) {
		t.Fatalf("long-string query: %v, want a config error", err)
	}

	// saveBatch fails the second batch before it commits.
	c = runConfig("sqlb_failed_save")
	b = open(t, c, Options{Rows: RowsDynamic})
	saves := 0
	b.save = func(ctx context.Context, ts *otelsql.TenantStore, bt *batch) error {
		if saves++; saves == 2 {
			return injected
		}
		return b.saveBatch(ctx, ts, bt)
	}
	stats, err = b.Ingest(ctx)
	if !errors.Is(err, injected) {
		t.Fatalf("saveBatch ingest = %v, want the injected failure", err)
	}
	requireStats("saveBatch", stats, 16, 1, c)

	// A Write that commits one span less and reports no error.
	c = runConfig("sqlb_short_writer")
	b = open(t, c, Options{})
	b.spanWrite = func(w *otel.SpanWriter, ctx context.Context, rss []*tracepb.ResourceSpans) (otel.ExportStats, error) {
		return w.Write(ctx, rss[:len(rss)-1])
	}
	var operation *otel.OperationError
	if _, err := b.Ingest(ctx); !errors.As(err, &operation) || !strings.Contains(err.Error(), "incomplete source population") {
		t.Fatalf("short writes: %v, want an operation error: incomplete source population", err)
	}
}

// An Open that fails after it registered its driver backend (here sql.Open
// refusing a page size over the driver's int32 option) unregisters it: a DSN
// naming the backend's driver key no longer reaches its database.
func testOpenFailureUnregisters(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	b, err := Open(ctx, testConfig("sqlb_unregister"), cluster, Options{PageRows: math.MaxInt32 + 1})
	if err == nil {
		b.Close()
		t.Fatal("Open with a page size over the driver's limit succeeded")
	}
	key := "otelbench-backend-" + strconv.FormatInt(backendKeys.Load(), 10)
	db, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+key+"&schema=CATALOG")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Unregistered, the driver reads the key as a cluster file path, which
	// does not exist; still registered, it would reach the closed database.
	if err := db.PingContext(ctx); err == nil || !strings.Contains(err.Error(), "open cluster file") {
		t.Fatalf("driver key %s still reaches the failed Open's database: %v", key, err)
	}
}

func testConfig(run string) otel.Config {
	c := otel.DefaultConfig()
	c.Run, c.Backend = run, "sql"
	c.Count, c.Tenants, c.Attributes, c.MatchEvery, c.K, c.Iterations = 240, 2, 8, 20, 5, 34
	c.Workers, c.Batch, c.SaveMode = 3, 16, "batch"
	return c
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// Rows a Record Layer writer saves through otelsql.OpenTenantStore and
// SpanRows are the rows SQL reads: typed columns, every typed attribute array
// and its index, events, links and the lossless otlp envelope.
func testDirectWriterRows(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const path = "/" + domain + "/otel_direct_writer" // Open registered the domain
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s&schema=CATALOG", cluster))
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	if err := otelsql.CreateTenant(ctx, setup, path, "S"); err != nil {
		t.Fatal(err)
	}
	raw, err := fdb.OpenDatabase(cluster)
	if err != nil {
		t.Fatal(err)
	}
	rl := recordlayer.NewFDBDatabase(raw)
	ts, err := otelsql.OpenTenantStore(ctx, rl, path, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otelsql.OpenTenantStore(ctx, rl, path, "MISSING", nil); err == nil {
		t.Fatal("OpenTenantStore of a missing schema succeeded")
	}
	rows, err := otelsql.NewSpanRows(ts.MetaData)
	if err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("x", otelsql.MaxIndexedStringBytes+1)
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strKV("service.name", "agent")}}
	scope := &commonpb.InstrumentationScope{Name: "lib", Version: "1.2", Attributes: []*commonpb.KeyValue{strKV("scope.attr", "s")}}
	spans := []*tracepb.Span{
		{
			TraceId: []byte{1}, SpanId: []byte{0x0a}, Name: "chat", Kind: tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: 100, EndTimeUnixNano: 150,
			Attributes: []*commonpb.KeyValue{
				strKV("gen_ai.request.model", "gpt-4o"), intKV("gen_ai.usage.input_tokens", 50), strKV("prompt", long),
				{Key: "list", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}}}}},
				{Key: "ratio", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}}},
				{Key: "ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
			},
			Events: []*tracepb.Span_Event{{Name: "tokens", TimeUnixNano: 120, Attributes: []*commonpb.KeyValue{intKV("n", 3)}}},
			Links:  []*tracepb.Span_Link{{TraceId: []byte{9}, SpanId: []byte{0x99}, Attributes: []*commonpb.KeyValue{strKV("rel", "parent")}}},
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
		},
		{
			TraceId: []byte{1}, SpanId: []byte{0x0b}, ParentSpanId: []byte{0x0a}, Name: "tool",
			StartTimeUnixNano: 200, EndTimeUnixNano: 260,
			Attributes: []*commonpb.KeyValue{strKV("gen_ai.tool.name", "search"), intKV("gen_ai.usage.input_tokens", 500)},
		},
		{
			TraceId: []byte{2}, SpanId: []byte{0x0c}, Name: "chat", StartTimeUnixNano: 300, EndTimeUnixNano: 310,
			Attributes: []*commonpb.KeyValue{strKV("gen_ai.request.model", "gpt-4o")},
		},
	}
	envelopes := make([][]byte, len(spans))
	records := make([]proto.Message, len(spans))
	for i, sp := range spans {
		envelopes[i], err = proto.Marshal(&tracepb.ResourceSpans{Resource: resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{sp}}}})
		if err != nil {
			t.Fatal(err)
		}
		if records[i], err = rows.Build(otelsql.Span{Resource: resource, Scope: scope, Span: sp, OTLP: envelopes[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rows.Build(otelsql.Span{}); err == nil {
		t.Fatal("Build of a nil span succeeded")
	}
	if _, err := rl.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		return store.SaveRecordBatch(records)
	}); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(path), cluster))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	digest := sha256.Sum256([]byte(long))
	for _, c := range []struct {
		name, sql, rows string
		planHas         []string
	}{
		{
			name: "typed columns",
			sql:  "SELECT name, kind, start_ns, end_ns, status_code, status_message, service_name, scope_name, scope_version FROM spans WHERE trace_id = X'01' AND span_id = X'0a'",
			rows: "[chat 3 100 150 2 boom agent lib 1.2]",
		},
		{
			name: "string attribute newest first",
			sql: "SELECT s.span_id, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.str_attrs AS a " +
				"WHERE a.scope = 'span' AND a.attr_key = 'gen_ai.request.model' AND a.str_value = 'gpt-4o') ORDER BY s.start_ns DESC",
			rows:    "[[12] 300] [[10] 100]",
			planHas: []string{"IndexScan(SPANS_STR_ATTR, [=, =, =,", "COVERING", "REVERSE"},
		},
		{
			name: "resource and scope attributes",
			sql: "SELECT a.scope, a.attr_key, a.str_value FROM spans AS s, s.str_attrs AS a WHERE s.trace_id = X'01' AND s.span_id = X'0b' " +
				"AND a.scope <> 'span'",
			rows: "[resource service.name agent] [scope scope.attr s]",
		},
		{
			name: "integer attribute",
			sql: "SELECT s.span_id FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.int_attrs AS a " +
				"WHERE a.scope = 'span' AND a.attr_key = 'gen_ai.usage.input_tokens' AND a.int_value = 500)",
			rows:    "[[11]]",
			planHas: []string{"IndexScan(SPANS_INT_ATTR, [=, =, ="},
		},
		{
			name: "long string by digest",
			sql: fmt.Sprintf("SELECT s.span_id, a.str_value FROM spans AS s, s.long_str_attrs AS a WHERE a.scope = 'span' "+
				"AND a.attr_key = 'prompt' AND a.digest = X'%x'", digest[:otelsql.LongStringDigestBytes]),
			rows: "[[10] " + long + "]",
		},
		{
			name: "double, bool and any attribute keys",
			sql: "SELECT d.attr_key, d.double_value, b.attr_key, b.bool_value, x.attr_key FROM spans AS s, s.double_attrs AS d, s.bool_attrs AS b, s.any_attrs AS x " +
				"WHERE s.trace_id = X'01' AND s.span_id = X'0a'",
			rows: "[ratio 0.5 ok true list]",
		},
		{
			name: "events and links",
			sql: "SELECT e.name, e.time_ns, ea.int_value, l.trace_id, la.str_value FROM spans AS s, s.events AS e, e.int_attrs AS ea, " +
				"s.links AS l, l.str_attrs AS la WHERE s.trace_id = X'01' AND s.span_id = X'0a'",
			rows: "[tokens 120 3 [9] parent]",
		},
		{
			name:    "span by id",
			sql:     "SELECT trace_id, start_ns FROM spans WHERE span_id = X'0b'",
			rows:    "[[1] 200]",
			planHas: []string{"IndexScan(SPANS_SPAN_ID, [="},
		},
		{
			name:    "time range newest first",
			sql:     "SELECT span_id FROM spans WHERE start_ns >= 150 AND start_ns < 400 ORDER BY start_ns DESC",
			rows:    "[[12]] [[11]]",
			planHas: []string{"IndexScan(SPANS_TIME,", "REVERSE"},
		},
	} {
		if len(c.planHas) > 0 {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+c.sql).Scan(&plan); err != nil {
				t.Fatalf("%s: explain: %v", c.name, err)
			}
			for _, want := range c.planHas {
				if !strings.Contains(plan, want) {
					t.Errorf("%s: plan lacks %q: %s", c.name, want, plan)
				}
			}
		}
		if got := queryRows(t, ctx, db, c.sql); got != c.rows {
			t.Errorf("%s: rows %s, want %s", c.name, got, c.rows)
		}
	}
	// An array value is its canonical JSON, equality-safe across writers.
	var j string
	if err := db.QueryRowContext(ctx, "SELECT x.json_value FROM spans AS s, s.any_attrs AS x WHERE s.trace_id = X'01' AND s.span_id = X'0a'").Scan(&j); err != nil {
		t.Fatal(err)
	}
	if j != "[1]" || j != string(otelsql.CanonicalJSON(spans[0].Attributes[3].Value)) {
		t.Errorf("any_attrs json %s, want [1]", j)
	}
	// The envelope is stored as given: SQL returns the writer's bytes.
	for i, sp := range spans {
		var got []byte
		if err := db.QueryRowContext(ctx, "SELECT otlp FROM spans WHERE trace_id = ? AND span_id = ?", sp.TraceId, sp.SpanId).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, envelopes[i]) {
			t.Errorf("span %d: otlp envelope differs from the written bytes", i)
		}
	}
}

func queryRows(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return strings.Join(out, " ")
}

// The exact census and ingest details that the ops benchmark guards
// (the census, write_path and committed_batches checks in
// bench/ops/horizontal/policy.py) require of a one-tenant,
// match_every=100 corpus written by compiled batch direct ingest: records,
// SPANS_TIME and SPANS_SPAN_ID N, an int attribute per span plus the int-typed
// query key of nonmatching traces' every third span, one store header key,
// and 11N+1 owned keys. With --batch 64 sqlbackend cuts 7 batches by its
// attribute estimate and the SpanWriter commits each 45-span one in two
// transactions by its row estimate: committed_batches counts the 13
// transactions that committed.
func testBenchCensus(t *testing.T, open func(*testing.T, otel.Config, Options) *Backend) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := testConfig("sqlb_bench_census")
	c.Count, c.Tenants, c.MatchEvery, c.SaveMode, c.RecordCompression, c.Batch = 300, 1, 100, "batch", "zlib-1", 64
	b := open(t, c, Options{})
	stats, err := b.Ingest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	batches, err := strconv.ParseInt(stats.Details["committed_batches"], 10, 64)
	if stats.Rows != 300 || stats.Attributes != 8*300 || stats.Details["write_path"] != "otel.SpanWriter" ||
		err != nil || batches != 13 || strconv.FormatInt(batches, 10) != stats.Details["committed_batches"] {
		t.Fatalf("ingest stats %+v", stats)
	}
	audit, err := b.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// ints = N + |{i < N, i%3 == 0, (i/2)%100 != 0}|: for N = 300, i = 0 and
	// i = 201 are matching traces' spans, so 98 of the 100 count.
	want := map[string]int64{"records": 300, "SPANS_STR_ATTR": 8*300 - 398, "SPANS_INT_ATTR": 398, "SPANS_SPAN_ID": 300, "SPANS_TIME": 300, "store_0": 1, "owned_total": 11*300 + 1}
	for name, keys := range want {
		if got := audit.Structures[name].Keys; got != keys {
			t.Errorf("%s: %d keys, want %d", name, got, keys)
		}
	}
	ints := int64(c.Count)
	for i := 0; i < c.Count; i += 3 {
		if (i/2)%100 != 0 {
			ints++
		}
	}
	if ints != want["SPANS_INT_ATTR"] {
		t.Fatalf("the guards' formula gives %d int keys, the pinned census %d", ints, want["SPANS_INT_ATTR"])
	}
}
