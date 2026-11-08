// Command seed writes a demo OTEL_V1 run through otel.Exporter for the plugin's
// development stack, its end-to-end tests and the load check: tenants T0 (GenAI
// messages as deltas) and T1 (deltas plus full-text), each with traces carrying
// events, links, error statuses, typed attributes of every scope, GenAI
// messages and correlated logs. IDs are deterministic (see TraceID), times are
// spread over -window ending now; rerunning rewrites the same traces.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/fdbgo/fdb"
)

// TraceID is trace i of tenant: 5eed, the tenant, then i+1, in hex.
func TraceID(tenant, i int) string { return fmt.Sprintf("5eed%02x%026x", tenant, i+1) }

// SpanID is span j of trace i of tenant.
func SpanID(tenant, i, j int) string { return fmt.Sprintf("%02x%06x%08x", tenant, i, j+1) }

var services = []string{"frontend", "checkout", "llm-gateway"}

func main() {
	clusterFile := flag.String("cluster-file", "", "FDB cluster file")
	clusterString := flag.String("cluster-string", "", "FDB cluster string (instead of -cluster-file), e.g. docker:docker@fdb:4500")
	run := flag.String("run", "grafana_dev", "Exporter run: the database is /OTEL/R_<RUN>")
	traces := flag.Int("traces", 200, "traces per tenant")
	window := flag.Duration("window", 6*time.Hour, "the traces' start times spread over this window, ending now")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline")
	flag.Parse()
	if err := seed(*clusterFile, *clusterString, *run, *traces, *window, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func seed(clusterFile, clusterString, run string, traces int, window, timeout time.Duration) error {
	if (clusterFile == "") == (clusterString == "") {
		return fmt.Errorf("set exactly one of -cluster-file and -cluster-string")
	}
	if traces < 1 {
		return fmt.Errorf("-traces must be positive")
	}
	if clusterString != "" {
		dir, err := os.MkdirTemp("", "seed")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		clusterFile = filepath.Join(dir, "fdb.cluster")
		if err := os.WriteFile(clusterFile, []byte(clusterString+"\n"), 0o600); err != nil {
			return err
		}
	}
	if err := fdb.APIVersion(730); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	now := time.Now()
	for tenant, fullText := range []bool{false, true} {
		cfg := otel.DefaultExporterConfig()
		cfg.ClusterFile, cfg.Run, cfg.Tenant = clusterFile, run, tenant
		cfg.EnableMessageDeltas = true
		cfg.EnableMessageFullText = fullText
		ex, err := otel.NewExporter(ctx, cfg)
		if err != nil {
			return fmt.Errorf("T%d: %w", tenant, err)
		}
		spans, logs := tenantData(tenant, traces, window, now)
		if err := ex.ConsumeTraces(ctx, spans); err != nil {
			_ = ex.Shutdown(ctx)
			return fmt.Errorf("T%d traces: %w", tenant, err)
		}
		if err := ex.ConsumeLogs(ctx, logs); err != nil {
			_ = ex.Shutdown(ctx)
			return fmt.Errorf("T%d logs: %w", tenant, err)
		}
		if err := ex.Shutdown(ctx); err != nil {
			return fmt.Errorf("T%d: %w", tenant, err)
		}
		layout := otel.StoreLayoutDeltas
		if fullText {
			layout = otel.StoreLayoutFullText
		}
		fmt.Printf("T%d (%s): %d traces, %d spans, %d logs; newest trace %s\n",
			tenant, layout, traces, 4*traces, countLogs(logs), TraceID(tenant, 0))
	}
	return nil
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

func dv(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}

func bv(b bool) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
}

// messages is a GenAI chat history in the semantic conventions' array form.
func messages(role, text string) *commonpb.AnyValue {
	part := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
		kv("type", sv("text")), kv("content", sv(text)),
	}}}}
	msg := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
		kv("role", sv(role)),
		kv("parts", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{part}}}}),
	}}}}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{msg}}}}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// tenantData is tenant's traces, newest first: a server root with a GenAI
// chat call, a tool call linked to the chat and a database call under the
// tool; every seventh trace fails in the chat call. Logs follow the spans.
func tenantData(tenant, traces int, window time.Duration, now time.Time) ([]*tracepb.ResourceSpans, []*logspb.ResourceLogs) {
	var rs []*tracepb.ResourceSpans
	var rl []*logspb.ResourceLogs
	ms := int64(time.Millisecond)
	for i := range traces {
		tid := mustHex(TraceID(tenant, i))
		sid := func(j int) []byte { return mustHex(SpanID(tenant, i, j)) }
		start := now.Add(-time.Minute - time.Duration(int64(window)*int64(i)/int64(traces))).UnixNano()
		failed := i%7 == 3
		svc := services[i%len(services)]
		status := int64(200)
		if failed {
			status = 500
		}
		root := &tracepb.Span{
			TraceId: tid, SpanId: sid(0), Name: "POST /chat", Kind: tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(start + (120+int64(i%10)*15)*ms),
			Attributes: []*commonpb.KeyValue{
				kv("http.method", sv("POST")), kv("http.status_code", iv(status)),
				kv("sample.rate", dv(0.5)), kv("cache.hit", bv(i%2 == 0)),
			},
		}
		chat := &tracepb.Span{
			TraceId: tid, SpanId: sid(1), ParentSpanId: sid(0), Name: "chat gpt-4o", Kind: tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start + 5*ms), EndTimeUnixNano: uint64(start + 80*ms),
			Attributes: []*commonpb.KeyValue{
				kv("gen_ai.operation.name", sv("chat")), kv("gen_ai.request.model", sv("gpt-4o")),
				kv("gen_ai.usage.input_tokens", iv(int64(100+i%50))), kv("gen_ai.usage.output_tokens", iv(int64(20+i%30))),
				kv("gen_ai.input.messages", messages("user", fmt.Sprintf("question %d for tenant %d", i, tenant))),
				kv("gen_ai.output.messages", messages("assistant", fmt.Sprintf("answer %d", i))),
			},
			Events: []*tracepb.Span_Event{{TimeUnixNano: uint64(start + 70*ms), Name: "gen_ai.choice", Attributes: []*commonpb.KeyValue{kv("index", iv(0))}}},
		}
		if failed {
			chat.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "rate limited"}
			chat.Events = append(chat.Events, &tracepb.Span_Event{TimeUnixNano: uint64(start + 75*ms), Name: "exception", Attributes: []*commonpb.KeyValue{kv("exception.type", sv("RateLimitError"))}})
		}
		tool := &tracepb.Span{
			TraceId: tid, SpanId: sid(2), ParentSpanId: sid(0), Name: "tool search", Kind: tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: uint64(start + 82*ms), EndTimeUnixNano: uint64(start + 110*ms),
			Attributes: []*commonpb.KeyValue{kv("tool.name", sv("search")), kv("tool.results", iv(int64(i%5)))},
			Links:      []*tracepb.Span_Link{{TraceId: tid, SpanId: sid(1), Attributes: []*commonpb.KeyValue{kv("link.kind", sv("follows"))}}},
		}
		db := &tracepb.Span{
			TraceId: tid, SpanId: sid(3), ParentSpanId: sid(2), Name: "SELECT documents", Kind: tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start + 85*ms), EndTimeUnixNano: uint64(start + 105*ms),
			Attributes: []*commonpb.KeyValue{kv("db.system", sv("foundationdb")), kv("db.rows", iv(int64(i%20)))},
		}
		resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", sv(svc)), kv("deployment.environment", sv("dev"))}}
		scope := &commonpb.InstrumentationScope{Name: "seed", Version: "1"}
		rs = append(rs, &tracepb.ResourceSpans{Resource: resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{root, chat, tool, db}}}})

		rec := func(at int64, span []byte, sev logspb.SeverityNumber, text, body, event string) *logspb.LogRecord {
			return &logspb.LogRecord{
				TimeUnixNano: uint64(start + at*ms), TraceId: tid, SpanId: span,
				SeverityNumber: sev, SeverityText: text, EventName: event, Body: sv(body),
				Attributes: []*commonpb.KeyValue{kv("log.kind", sv("app")), kv("user.id", iv(int64(1000+i%25)))},
			}
		}
		records := []*logspb.LogRecord{
			rec(1, sid(0), logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO", fmt.Sprintf("request %d received", i), ""),
			rec(79, sid(1), logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO", fmt.Sprintf("evaluation of answer %d", i), "gen_ai.evaluation.result"),
		}
		if failed {
			records = append(records, rec(76, sid(1), logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR", "chat call failed: rate limited", ""))
		}
		rl = append(rl, &logspb.ResourceLogs{Resource: resource, ScopeLogs: []*logspb.ScopeLogs{{Scope: scope, LogRecords: records}}})
	}
	return rs, rl
}

func countLogs(rl []*logspb.ResourceLogs) int {
	n := 0
	for _, r := range rl {
		for _, s := range r.ScopeLogs {
			n += len(s.LogRecords)
		}
	}
	return n
}
