package plugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/fdbgo/fdb"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// The plugin against a real cluster: stores seeded by otel.Exporter, every
// query type through QueryData, resources, health, tenant switching and the
// read-only guarantees, with a key audit around the reads.

const itRun = "gd_plugin_it"

var itStart = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC).UnixNano()

func startCluster(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(APIVersion))
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
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func sv(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func iv(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

// spanIDOf descends as i (and start time) ascends, so the reader's span-ID
// order differs from the frame's start-time order.
func spanIDOf(tenant, i int) []byte {
	return idBytes(fmt.Sprintf("%02x%02x0000000000%02x", tenant, 9-i, i+1))
}

func traceIDOf(tenant int) []byte { return idBytes(fmt.Sprintf("%02x%030x", tenant, 0xabc)) }

// itMessages is a GenAI chat history in the array form.
func itMessages(role, text string) *commonpb.AnyValue {
	msg := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
		kv("role", sv(role)),
		kv("parts", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
			{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kv("type", sv("text")), kv("content", sv(text))}}}},
		}}}}),
	}}}}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{msg}}}}
}

// itTrace is tenant's trace: a root and two children with events, a link,
// an error status, typed attributes of every scope and GenAI messages.
func itTrace(tenant int) []*tracepb.ResourceSpans {
	trace := traceIDOf(tenant)
	var out []*tracepb.ResourceSpans
	for i := range 3 {
		sp := &tracepb.Span{
			TraceId: trace, SpanId: spanIDOf(tenant, i), Name: fmt.Sprintf("op %d", i), Kind: tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(itStart + int64(i)*1_000_000), EndTimeUnixNano: uint64(itStart + int64(i)*1_000_000 + 400_000),
			Attributes: []*commonpb.KeyValue{
				kv("gen_ai.operation.name", sv("chat")),
				kv("turn", iv(int64(i))),
				kv("tenant.mark", sv(fmt.Sprintf("t%d", tenant))),
				kv("gen_ai.input.messages", itMessages("user", fmt.Sprintf("question %d for tenant %d", i, tenant))),
			},
			Events: []*tracepb.Span_Event{{TimeUnixNano: uint64(itStart + int64(i)*1_000_000 + 10), Name: "gen_ai.choice", Attributes: []*commonpb.KeyValue{kv("index", iv(int64(i)))}}},
		}
		if i == 0 {
			sp.Kind = tracepb.Span_SPAN_KIND_SERVER
		} else {
			sp.ParentSpanId = spanIDOf(tenant, 0)
			sp.Links = []*tracepb.Span_Link{{TraceId: trace, SpanId: spanIDOf(tenant, i-1), Attributes: []*commonpb.KeyValue{kv("link.kind", sv("previous"))}}}
		}
		if i == 1 {
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "tool failed"}
		}
		out = append(out, &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", sv(fmt.Sprintf("svc-%d", tenant)))}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "it", Version: "1"},
				Spans: []*tracepb.Span{sp},
			}},
		})
	}
	return out
}

func itLogs(tenant int) []*logspb.ResourceLogs {
	trace := traceIDOf(tenant)
	rec := func(i int, span []byte, event string) *logspb.LogRecord {
		return &logspb.LogRecord{
			TimeUnixNano: uint64(itStart + int64(i)*1_000_000 + 50), TraceId: trace, SpanId: span,
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_WARN, EventName: event,
			Body:       sv(fmt.Sprintf("log %d of tenant %d", i, tenant)),
			Attributes: []*commonpb.KeyValue{kv("log.kind", sv("it"))},
		}
	}
	return []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", sv(fmt.Sprintf("svc-%d", tenant)))}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			rec(0, spanIDOf(tenant, 0), ""),
			rec(1, spanIDOf(tenant, 1), "gen_ai.evaluation.result"),
			rec(2, spanIDOf(tenant, 1), ""),
		}}},
	}}
}

// seed writes tenant's trace and logs with an Exporter of the given layout.
func seed(t *testing.T, cluster string, tenant int, fullText bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cfg := otel.DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, itRun, tenant
	cfg.EnableMessageDeltas = true
	cfg.EnableMessageFullText = fullText
	ex, err := otel.NewExporter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ex.ConsumeTraces(ctx, itTrace(tenant)); err != nil {
		t.Fatal(err)
	}
	if err := ex.ConsumeLogs(ctx, itLogs(tenant)); err != nil {
		t.Fatal(err)
	}
	if err := ex.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// auditKeys is every user key and value of the cluster at one version.
func auditKeys(t *testing.T, cluster string) []fdb.KeyValue {
	t.Helper()
	db, err := fdb.OpenDatabase(cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatal(err)
	}
	kvs := out.([]fdb.KeyValue)
	if len(kvs) == 0 {
		t.Fatal("the key audit read no keys")
	}
	return kvs
}

type itHarness struct {
	ds      *Datasource
	cluster string
	deps    deps
	reg     *prometheus.Registry
}

func (h *itHarness) query(t *testing.T, model map[string]any) backend.DataResponse {
	t.Helper()
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	inst := h.ds.inst
	tr := backend.TimeRange{From: time.Unix(0, itStart-time.Hour.Nanoseconds()), To: time.Unix(0, itStart+time.Hour.Nanoseconds())}
	resp, err := h.ds.QueryData(t.Context(), &backend.QueryDataRequest{
		PluginContext: backend.PluginContext{DataSourceInstanceSettings: &inst},
		Queries:       []backend.DataQuery{{RefID: "A", QueryType: model["queryType"].(string), JSON: raw, TimeRange: tr, Interval: time.Second, MaxDataPoints: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Responses["A"]
}

func (h *itHarness) resource(t *testing.T, method, path, query string, body []byte) (int, string) {
	t.Helper()
	var c captured
	url := path
	if query != "" {
		url += "?" + query
	}
	if err := h.ds.CallResource(t.Context(), &backend.CallResourceRequest{Method: method, Path: path, URL: url, Body: body}, &c); err != nil {
		t.Fatal(err)
	}
	return c.resp.Status, strings.TrimSpace(string(c.resp.Body))
}

func TestPluginAgainstFDB(t *testing.T) {
	t.Parallel()
	cluster := startCluster(t)
	seed(t, cluster, 1, false)
	seed(t, cluster, 2, true)

	inst := instanceSettings(t, map[string]any{
		"clusterString": nil, "clusterFilePath": cluster, "database": "/OTEL/R_" + strings.ToUpper(itRun),
		"defaultSchema": "T1", "allowedSchemas": []string{"T1", "T2"}, "otelMode": "on",
		"transactionTags": []string{"grafana"},
	})
	reg := prometheus.NewRegistry()
	itDeps := defaultDeps()
	itDeps.metrics = newMetrics(reg)
	h := &itHarness{ds: newDatasource(inst, itDeps), cluster: cluster, deps: itDeps, reg: reg}
	t.Cleanup(h.ds.Dispose)

	before := auditKeys(t, cluster)

	t.Run("health", func(t *testing.T) {
		res, err := h.ds.CheckHealth(t.Context(), &backend.CheckHealthRequest{})
		if err != nil || res.Status != backend.HealthStatusOk || !strings.Contains(res.Message, otel.StoreLayoutDeltas) {
			t.Fatalf("%v %v: %s", err, res.Status, res.Message)
		}
	})
	t.Run("trace", func(t *testing.T) { itTraceQuery(t, h) })
	t.Run("trace_search", func(t *testing.T) { itSearch(t, h) })
	t.Run("logs", func(t *testing.T) { itLogsQuery(t, h) })
	t.Run("tenant_switching", func(t *testing.T) { itTenants(t, h) })
	t.Run("resources", func(t *testing.T) { itResources(t, h) })
	t.Run("sql", func(t *testing.T) { itSQL(t, h) })
	t.Run("sql_read_only", func(t *testing.T) { itSQLReadOnly(t, h) })
	t.Run("metrics", func(t *testing.T) { itMetrics(t, h) })

	if after := auditKeys(t, cluster); !slices.EqualFunc(before, after, func(a, b fdb.KeyValue) bool {
		return bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Value, b.Value)
	}) {
		t.Fatalf("the plugin wrote: %d keys before, %d after", len(before), len(after))
	}
}

// The trace frame equals the frame of the spans the Exporter was given:
// the stored OTLP, GenAI messages restored, reaches TraceView whole.
func itTraceQuery(t *testing.T, h *itHarness) {
	for _, tenant := range []int{1, 2} {
		id := hex.EncodeToString(traceIDOf(tenant))
		f := onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "tenant": fmt.Sprintf("T%d", tenant), "traceId": id}))
		want, err := traceFrame(itTrace(tenant))
		if err != nil {
			t.Fatal(err)
		}
		for i, field := range want.Fields {
			got := f.Fields[i]
			if got.Name != field.Name || got.Len() != field.Len() {
				t.Fatalf("T%d field %s: %d rows, want %d", tenant, got.Name, got.Len(), field.Len())
			}
			for row := range field.Len() {
				g, w := fmt.Sprint(got.At(row)), fmt.Sprint(field.At(row))
				if gr, ok := got.At(row).(json.RawMessage); ok {
					g, w = string(gr), string(field.At(row).(json.RawMessage))
				}
				if g != w {
					t.Errorf("T%d %s[%d]:\n got %s\nwant %s", tenant, field.Name, row, g, w)
				}
			}
		}
		// The reader returns span-ID order; TraceView gets start order.
		start, _ := f.FieldByName("startTime")
		ids, _ := f.FieldByName("spanID")
		for row := 1; row < start.Len(); row++ {
			if start.At(row).(float64) < start.At(row-1).(float64) {
				t.Fatalf("T%d rows not in start order at %d", tenant, row)
			}
		}
		if ids.At(0) != hex.EncodeToString(spanIDOf(tenant, 0)) {
			t.Fatalf("T%d first row %v, want the root", tenant, ids.At(0))
		}
		if len(f.Meta.Notices) != 0 {
			t.Errorf("notices %+v", f.Meta.Notices)
		}
		span, _ := f.FieldByName("spanID")
		if len(span.Config.Links) != 2 {
			t.Fatalf("span links %+v", span.Config.Links)
		}
	}
	// A tiny budget returns a partial trace with a notice.
	small := instanceSettings(t, map[string]any{"clusterString": nil, "clusterFilePath": h.cluster, "database": "/OTEL/R_" + strings.ToUpper(itRun), "traceMaxSpans": 2})
	hs := &itHarness{ds: newDatasource(small, h.deps), cluster: h.cluster}
	defer hs.ds.Dispose()
	f := onlyFrame(t, hs.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": hex.EncodeToString(traceIDOf(1))}))
	if n, _ := f.RowLen(); n != 2 || len(f.Meta.Notices) != 1 || !strings.Contains(f.Meta.Notices[0].Text, "traceMaxSpans") {
		t.Fatalf("budgeted trace: %d rows, %+v", n, f.Meta.Notices)
	}
	// traceMaxBytes bounds what Grafana receives (the decoded spans).
	tiny := instanceSettings(t, map[string]any{"clusterString": nil, "clusterFilePath": h.cluster, "database": "/OTEL/R_" + strings.ToUpper(itRun), "traceMaxBytes": 600})
	ht := &itHarness{ds: newDatasource(tiny, h.deps), cluster: h.cluster}
	defer ht.ds.Dispose()
	f = onlyFrame(t, ht.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": hex.EncodeToString(traceIDOf(1))}))
	if n, _ := f.RowLen(); n >= 3 || len(f.Meta.Notices) != 1 || !strings.Contains(f.Meta.Notices[0].Text, "traceMaxBytes") {
		t.Fatalf("decoded-size budget: %d rows, %+v", n, f.Meta.Notices)
	}
}

func itSearch(t *testing.T, h *itHarness) {
	trace := hex.EncodeToString(traceIDOf(1))
	check := func(name string, model map[string]any, matched int64) {
		t.Helper()
		f := onlyFrame(t, h.query(t, model))
		if n, _ := f.RowLen(); n != 1 || f.Fields[0].At(0) != trace {
			t.Fatalf("%s: %d rows, %v", name, n, f.Fields[0].At(0))
		}
		if m := f.Fields[2].At(0).(*int64); matched > 0 && (m == nil || *m != matched) || matched == 0 && m != nil {
			t.Fatalf("%s: matched %v, want %d", name, m, matched)
		}
		svc, _ := f.FieldByName("traceService")
		nm, _ := f.FieldByName("traceName")
		dur, _ := f.FieldByName("traceDuration")
		if svc == nil || *svc.At(0).(*string) != "svc-1" || *nm.At(0).(*string) != "op 0" || *dur.At(0).(*float64) != 2.4 {
			t.Fatalf("%s: summary missing or wrong", name)
		}
	}
	chat := map[string]any{"scope": "span", "key": "gen_ai.operation.name", "type": "string", "value": "chat"}
	check("sameSpan", searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{chat, map[string]any{"scope": "span", "key": "turn", "type": "int", "value": "1"}}}), 1)
	check("sameSpan all", searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{chat}}), 3)
	check("existential", searchModel(modeExistential, map[string]any{
		"spanPredicates": []any{chat},
		"logPredicates":  []any{map[string]any{"scope": "event_name", "key": "event_name", "type": "string", "value": "gen_ai.evaluation.result"}},
	}), 3)
	check("attribute", searchModel(modeAttribute, map[string]any{"spanPredicates": []any{map[string]any{"scope": "resource", "key": "service.name", "type": "string", "value": "svc-1"}}}), 0)
	check("time", searchModel(modeTime, nil), 0)
	r := h.query(t, searchModel(modeSameSpan, map[string]any{"spanPredicates": []any{map[string]any{"scope": "span", "key": "turn", "type": "int", "value": "007"}}}))
	if r.Error == nil {
		t.Fatal("a non-canonical int was accepted")
	}
}

func itLogsQuery(t *testing.T, h *itHarness) {
	trace := hex.EncodeToString(traceIDOf(1))
	rows := func(model map[string]any) *data.Frame {
		model["queryType"] = queryTypeLogs
		return onlyFrame(t, h.query(t, model))
	}
	f := rows(map[string]any{"traceId": trace})
	if n, _ := f.RowLen(); n != 3 || f.Meta.Type != data.FrameTypeLogLines {
		t.Fatalf("logs by trace: %d rows, %+v", n, f.Meta)
	}
	body, _ := f.FieldByName("body")
	sev, _ := f.FieldByName("severity")
	if body.At(0) != "log 2 of tenant 1" || sev.At(0) != "warn" {
		t.Fatalf("newest log %v %v", body.At(0), sev.At(0))
	}
	f = rows(map[string]any{"traceId": trace, "spanId": hex.EncodeToString(spanIDOf(1, 1))})
	if n, _ := f.RowLen(); n != 2 {
		t.Fatalf("logs by span: %d rows", n)
	}
	f = rows(map[string]any{"attribute": map[string]any{"scope": "event_name", "key": "event_name", "type": "string", "value": "gen_ai.evaluation.result"}})
	if n, _ := f.RowLen(); n != 1 {
		t.Fatalf("logs by event name: %d rows", n)
	}
	f = rows(map[string]any{"attribute": map[string]any{"scope": "log", "key": "log.kind", "type": "string", "value": "it"}, "limit": 2})
	if n, _ := f.RowLen(); n != 2 {
		t.Fatalf("logs by attribute, limit 2: %d rows", n)
	}
	tf, _ := f.FieldByName("traceId")
	if tf.At(0) != trace || tf.Config.Links[0].Internal.Query.(map[string]any)["queryType"] != queryTypeTrace {
		t.Fatalf("log → trace link on %v", tf.At(0))
	}
}

func itTenants(t *testing.T, h *itHarness) {
	t2 := hex.EncodeToString(traceIDOf(2))
	f := onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": t2}))
	if n, _ := f.RowLen(); n != 0 || len(f.Meta.Notices) != 1 {
		t.Fatalf("T2's trace visible through T1: %d rows", n)
	}
	f = onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "tenant": "T2", "traceId": t2}))
	svc, _ := f.FieldByName("serviceName")
	if n, _ := f.RowLen(); n != 3 || svc.At(0) != "svc-2" {
		t.Fatalf("T2: %d rows", n)
	}
	r := h.query(t, map[string]any{"queryType": queryTypeTrace, "tenant": "T3", "traceId": t2})
	if r.Error == nil || !strings.Contains(r.Error.Error(), "allowedSchemas") {
		t.Fatalf("T3: %v", r.Error)
	}
	if slices.Contains(h.ds.readers.keys(), "T3") {
		t.Fatal("opened a reader on a refused tenant")
	}
}

func itResources(t *testing.T, h *itHarness) {
	status, body := h.resource(t, http.MethodGet, "tenants", "", nil)
	if status != http.StatusOK || body != `{"tenants":["T1","T2"]}` {
		t.Fatalf("tenants %d %s", status, body)
	}
	status, body = h.resource(t, http.MethodGet, "attribute-keys", "scope=span&type=string&prefix=gen_ai.op", nil)
	if status != http.StatusOK || body != `{"keys":["gen_ai.operation.name"],"truncated":false}` {
		t.Fatalf("keys %d %s", status, body)
	}
	status, body = h.resource(t, http.MethodGet, "attribute-keys", "tenant=T2&signal=log&scope=log", nil)
	if status != http.StatusOK || !strings.Contains(body, `"log.kind"`) {
		t.Fatalf("log keys %d %s", status, body)
	}
	status, body = h.resource(t, http.MethodGet, "attribute-values", "tenant=T2&scope=span&key=tenant.mark&type=string", nil)
	if status != http.StatusOK || body != `{"values":["t2"],"truncated":false}` {
		t.Fatalf("values %d %s", status, body)
	}
	status, body = h.resource(t, http.MethodPost, "tables", "", []byte(`{"schema":"T2"}`))
	if status != http.StatusOK || !strings.Contains(body, `"SPANS"`) && !strings.Contains(body, `"spans"`) {
		t.Fatalf("tables %d %s", status, body)
	}
	status, body = h.resource(t, http.MethodPost, "columns", "", []byte(`{"schema":"T1","table":"spans"}`))
	if status != http.StatusOK || !strings.Contains(strings.ToLower(body), `"start_ns"`) {
		t.Fatalf("columns %d %s", status, body)
	}
	if status, _ = h.resource(t, http.MethodPost, "tables", "", []byte(`{"schema":"T9"}`)); status != http.StatusBadRequest {
		t.Fatalf("tables of a refused tenant: %d", status)
	}
}

func itSQL(t *testing.T, h *itHarness) {
	r := h.query(t, map[string]any{
		"queryType": queryTypeSQL, "format": 1,
		"rawSql": "SELECT $__time(start_ns), name, trace_id FROM spans WHERE $__timeFilter(start_ns) AND trace_id = $__hex(" + hex.EncodeToString(traceIDOf(1)) + ")",
	})
	f := onlyFrame(t, r)
	if n, _ := f.RowLen(); n != 3 {
		t.Fatalf("%d rows", n)
	}
	if f.Fields[0].Type() != data.FieldTypeNullableTime && f.Fields[0].Type() != data.FieldTypeTime {
		t.Fatalf("time column %s", f.Fields[0].Type())
	}
	var traceField *data.Field
	for _, fl := range f.Fields {
		if strings.EqualFold(fl.Name, "trace_id") {
			traceField = fl
		}
	}
	if traceField == nil || len(traceField.Config.Links) != 1 {
		t.Fatalf("trace_id column or its link missing: %+v", f.Fields)
	}
	if v := traceField.At(0).(*string); *v != hex.EncodeToString(traceIDOf(1)) {
		t.Fatalf("trace_id rendered %q", *v)
	}

	// format=logs over the LOGS table is a data-plane log-lines frame.
	r = h.query(t, map[string]any{
		"queryType": queryTypeSQL, "format": 2,
		"rawSql": `SELECT effective_ns AS "timestamp", body_text AS "body", severity_text AS "severity", event_id AS "id" FROM logs WHERE $__timeFilter(effective_ns)`,
	})
	f = onlyFrame(t, r)
	if n, _ := f.RowLen(); n != 3 || f.Meta.Type != data.FrameTypeLogLines {
		t.Fatalf("SQL logs: %d rows, meta %+v", n, f.Meta)
	}
	if body, _ := f.FieldByName("body"); body == nil || *body.At(0).(*string) == "" {
		t.Fatal("SQL logs: no body")
	}

	// Time series: buckets in ns become a time field for long-to-wide.
	r = h.query(t, map[string]any{
		"queryType": queryTypeSQL, "format": 0, "tenant": "T2",
		"rawSql": "SELECT $__timeGroupAlias(start_ns, 1s), COUNT(*) AS n FROM spans WHERE $__timeFilter(start_ns) GROUP BY $__timeGroup(start_ns, 1s)",
	})
	f = onlyFrame(t, r)
	if f.TimeSeriesSchema().Type == data.TimeSeriesTypeNot {
		t.Fatalf("not a time series: %s", f.Meta.ExecutedQueryString)
	}
}

// Writes are refused before execution (read_only), and a T1 connection
// cannot read T2's rows (R3). sqlds flattens the driver error to text, so
// the SQLSTATE is checked in the message.
func itSQLReadOnly(t *testing.T, h *itHarness) {
	for stmt, code := range map[string]string{
		"INSERT INTO spans (trace_id, span_id) VALUES (X'01', X'02')":             "0A000",
		"UPDATE spans SET name = 'x'":                                             "0A000",
		"DELETE FROM spans":                                                       "0A000",
		"SELECT 1; DELETE FROM spans":                                             "0A000",
		"CREATE SCHEMA /OTEL/R_" + strings.ToUpper(itRun) + "/T9 WITH TEMPLATE X": "0A000",
		"SELECT name FROM T2.spans":                                               "42703",
	} {
		r := h.query(t, map[string]any{"queryType": queryTypeSQL, "format": 1, "rawSql": stmt})
		if r.Error == nil || !strings.Contains(r.Error.Error(), code) {
			t.Errorf("%s: want %s, got %v", stmt, code, r.Error)
		}
	}
}

// sumCounter adds every series of the counter name whose labels include want.
func sumCounter(t *testing.T, reg *prometheus.Registry, name string, want prometheus.Labels) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			have := map[string]string{}
			for _, lp := range m.GetLabel() {
				have[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if have[k] != v {
					continue series
				}
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// The §9 counters moved for the reads above, under the right labels.
func itMetrics(t *testing.T, h *itHarness) {
	m := h.deps.metrics
	val := func(c *prometheus.CounterVec, l prometheus.Labels) float64 { return testutil.ToFloat64(c.With(l)) }
	sqlT1 := h.ds.labels(queryTypeSQL, "T1")
	if val(m.sqlScannedRows, sqlT1) <= 0 || val(m.sqlScannedBytes, sqlT1) <= 0 || val(m.sqlPages, sqlT1) <= 0 {
		t.Fatalf("SQL scan counters: rows %v bytes %v pages %v", val(m.sqlScannedRows, sqlT1), val(m.sqlScannedBytes, sqlT1), val(m.sqlPages, sqlT1))
	}
	if n := val(m.sqlFailures, withLabel(sqlT1, "sqlstate", "0A000")); n != 5 {
		t.Fatalf("0A000 failures %v, want the 5 refused writes", n)
	}
	if n := val(m.sqlFailures, withLabel(sqlT1, "sqlstate", "42703")); n != 1 {
		t.Fatalf("42703 failures %v", n)
	}
	// A statement failing as it executes is counted once, not by both the
	// statistics hook and QueryContext.
	before := sumCounter(t, h.reg, "fdb_datasource_sql_failures_total", sqlT1)
	if r := h.query(t, map[string]any{"queryType": queryTypeSQL, "format": 1, "rawSql": "SELECT start_ns / 0 FROM spans"}); r.Error == nil {
		t.Fatal("division by zero succeeded")
	}
	if after := sumCounter(t, h.reg, "fdb_datasource_sql_failures_total", sqlT1); after != before+1 {
		t.Fatalf("failures %v → %v, want +1", before, after)
	}
	for _, tenant := range []string{"T1", "T2"} {
		tr := h.ds.labels(queryTypeTrace, tenant)
		if val(m.readBytes, tr) <= 0 || val(m.decodedBytes, tr) <= 0 {
			t.Fatalf("%s trace bytes %v decoded %v", tenant, val(m.readBytes, tr), val(m.decodedBytes, tr))
		}
	}
	tr1 := h.ds.labels(queryTypeTrace, "T1")
	for _, reason := range []string{otel.TraceReasonMaxSpans, otel.TraceReasonMaxDecodedBytes} {
		if val(m.truncations, withLabel(tr1, "reason", reason)) < 1 {
			t.Fatalf("no %s truncation counted", reason)
		}
	}
	ts := h.ds.labels(queryTypeTraceSearch, "T1")
	if val(m.candidates, ts) <= 0 || val(m.readBytes, ts) <= 0 || val(m.decodedBytes, ts) <= 0 {
		t.Fatalf("search candidates %v bytes %v decoded %v", val(m.candidates, ts), val(m.readBytes, ts), val(m.decodedBytes, ts))
	}
}
