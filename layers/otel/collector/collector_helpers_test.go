package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"fdb.dev/layers/otel"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	collectorAgents   = 8
	collectorRequests = 5
	collectorSpans    = 6
	collectorBase     = int64(1_700_000_000_000_000_000)
)

const realCollectorYAML = `
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
  batch:
    send_batch_size: 1024
exporters:
  debug:
    verbosity: basic
  fdb_otel:
    cluster_file: ${env:FDB_OTEL_COLLECTOR_CLUSTER_FILE}
    run: ${env:FDB_OTEL_COLLECTOR_RUN}
    tenant: ${env:FDB_OTEL_COLLECTOR_TEST_UNSET_TENANT:-7}
    record_compression: zlib-default
    batch_size: 16
    workers: 4
    k: 100
    timeout: 60s
    sending_queue:
      enabled: true
      num_consumers: 4
      queue_size: ${env:FDB_OTEL_COLLECTOR_TEST_UNSET_QUEUE:-2000}
      block_on_overflow: true
      batch:
        flush_timeout: 25ms
        min_size: 16
        max_size: 37
    retry_on_failure:
      enabled: true
      initial_interval: 50ms
      max_interval: 400ms
      max_elapsed_time: 60s
  fdb_otel/secondary:
    cluster_file: ${env:FDB_OTEL_COLLECTOR_CLUSTER_FILE}
    run: ${env:FDB_OTEL_COLLECTOR_RUN}_b
    tenant: 0
    batch_size: 8
    workers: 2
    sending_queue:
      enabled: false
    retry_on_failure:
      enabled: true
      initial_interval: 50ms
      max_interval: 200ms
      max_elapsed_time: 60s
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [fdb_otel, fdb_otel/secondary, debug]
`

func freshCollectorRun(t *testing.T) string {
	t.Helper()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return "coll_" + hex.EncodeToString(nonce[:])
}

func agentTraceID(agent, req int) []byte {
	return []byte{0xc0, byte(agent), byte(req), 15: 0x01}
}

func agentSpanID(agent, req, i int) []byte {
	return []byte{0x5a, byte(agent), byte(req), byte(i), 7: 0x01}
}

func agentStart(agent, req, i int) int64 {
	return collectorBase + int64(agent)*1e9 + int64(req)*1e7 + int64(i)*1e3
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func boolKV(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}

func doubleKV(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

// agentRequest spans two instrumentation scopes with non-canonical attribute types,
// duplicate keys and wrapper unknown fields to check fidelity.
func agentRequest(agent, req int) []*tracepb.ResourceSpans {
	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			strKV("service.name", fmt.Sprintf("svc-%d", agent)),
			strKV("service.instance.id", fmt.Sprintf("agent-%d", agent)),
			intKV("host.cores", int64(agent+1)),
			boolKV("deployment.canary", agent%2 == 0),
		}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.26.0",
	}
	rs.ProtoReflect().SetUnknown(unknownField(1001, uint64(agent+1)))
	for i, tier := range []string{"http", "db"} {
		ss := &tracepb.ScopeSpans{
			Scope:     &commonpb.InstrumentationScope{Name: "io.example." + tier, Version: "1.2.3", Attributes: []*commonpb.KeyValue{strKV("library.tier", tier)}},
			SchemaUrl: "scope-" + tier,
		}
		ss.ProtoReflect().SetUnknown(unknownField(1002, uint64(i+1)))
		rs.ScopeSpans = append(rs.ScopeSpans, ss)
	}
	for i := range collectorSpans {
		start := agentStart(agent, req, i)
		sp := &tracepb.Span{
			TraceId:           agentTraceID(agent, req),
			SpanId:            agentSpanID(agent, req, i),
			Name:              fmt.Sprintf("op-%d", i),
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(start),
			EndTimeUnixNano:   uint64(start + 500),
			Attributes: []*commonpb.KeyValue{
				strKV("http.route", fmt.Sprintf("/api/v1/r%d", req)),
				intKV("request.index", int64(req)),
				boolKV("error", req%2 == 1 && i == collectorSpans-1),
				doubleKV("latency.ms", float64(i)+0.25),
				strKV("dup", "first"),
				strKV("dup", "second"),
			},
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		}
		if i > 0 {
			sp.ParentSpanId = agentSpanID(agent, req, 0)
			sp.Kind = tracepb.Span_SPAN_KIND_CLIENT
		} else {
			sp.Events = []*tracepb.Span_Event{{Name: "request.received", TimeUnixNano: uint64(start + 10), Attributes: []*commonpb.KeyValue{intKV("bytes", 512)}}}
		}
		scope := 0
		if i >= 4 {
			scope = 1
		}
		rs.ScopeSpans[scope].Spans = append(rs.ScopeSpans[scope].Spans, sp)
	}
	return []*tracepb.ResourceSpans{rs}
}

type modelSpan struct {
	trace, id string
	start     int64
	attrs     map[otel.Attribute]bool
	stored    *tracepb.ResourceSpans // the LoadSpanOTLP answer
}

func canonicalSet(scope string, kvs []*commonpb.KeyValue) map[string]otel.Attribute {
	out := map[string]otel.Attribute{}
	for _, kv := range kvs {
		out[kv.Key] = otel.CanonicalAttribute(scope, kv.Key, kv.Value) // last value wins
	}
	return out
}

func collectorModel() []modelSpan {
	var out []modelSpan
	for agent := range collectorAgents {
		for req := range collectorRequests {
			for _, rs := range agentRequest(agent, req) {
				res := canonicalSet("resource", rs.Resource.Attributes)
				for _, ss := range rs.ScopeSpans {
					scope := canonicalSet("scope", ss.Scope.Attributes)
					for _, sp := range ss.Spans {
						attrs := map[otel.Attribute]bool{}
						for _, set := range []map[string]otel.Attribute{res, scope, canonicalSet("span", sp.Attributes)} {
							for _, a := range set {
								attrs[a] = true
							}
						}
						stored := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
						stored.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
						stored.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
						out = append(out, modelSpan{trace: hex.EncodeToString(sp.TraceId), id: hex.EncodeToString(sp.SpanId), start: int64(sp.StartTimeUnixNano), attrs: attrs, stored: stored})
					}
				}
			}
		}
	}
	return out
}

// modelHits mirrors the exporter answer: one hit per trace at its latest
// matching start, newest first, at most k.
func modelHits(model []modelSpan, k int, match func(modelSpan) bool) []otel.Hit {
	latest := map[string]int64{}
	for _, s := range model {
		if match(s) {
			if prev, ok := latest[s.trace]; !ok || s.start > prev {
				latest[s.trace] = s.start
			}
		}
	}
	out := []otel.Hit{}
	for tr, at := range latest {
		out = append(out, otel.Hit{Trace: tr, Latest: at})
	}
	slices.SortFunc(out, func(a, b otel.Hit) int {
		if a.Latest != b.Latest {
			if a.Latest > b.Latest {
				return -1
			}
			return 1
		}
		return -strings.Compare(a.Trace, b.Trace)
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func verifyCollectorStore(t *testing.T, ctx context.Context, name string, exp *otel.Exporter, model []modelSpan) {
	t.Helper()
	k := exp.Config().K
	anyBool := func(b bool) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
	}
	queries := []otel.Attribute{
		otel.CanonicalAttribute("resource", "deployment.canary", anyBool(true)),
		otel.CanonicalAttribute("resource", "deployment.canary", anyBool(false)),
		{Scope: "resource", Key: "host.cores", Type: "int", Value: "4"},
		{Scope: "resource", Key: "service.name", Type: "string", Value: "svc-99"},
		{Scope: "scope", Key: "library.tier", Type: "string", Value: "db"},
		{Scope: "scope", Key: "library.tier", Type: "string", Value: "http"},
		{Scope: "span", Key: "request.index", Type: "int", Value: "3"},
		{Scope: "span", Key: "request.index", Type: "string", Value: "3"},
		{Scope: "span", Key: "http.route", Type: "string", Value: "/api/v1/r4"},
		otel.CanonicalAttribute("span", "error", anyBool(true)),
		otel.CanonicalAttribute("span", "latency.ms", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 5.25}}),
		{Scope: "span", Key: "dup", Type: "string", Value: "second"},
		{Scope: "span", Key: "dup", Type: "string", Value: "first"},
		{Scope: "resource", Key: "dup", Type: "string", Value: "second"},
	}
	for agent := range collectorAgents {
		queries = append(queries, otel.Attribute{Scope: "resource", Key: "service.name", Type: "string", Value: fmt.Sprintf("svc-%d", agent)})
	}
	nonEmpty := 0
	for _, a := range queries {
		got, err := exp.Query(ctx, a)
		if err != nil {
			t.Fatalf("%s: Query(%+v): %v", name, a, err)
		}
		want := modelHits(model, k, func(s modelSpan) bool { return s.attrs[a] })
		if !slices.Equal(got.Hits, want) {
			t.Fatalf("%s: Query(%+v)\n got %v\nwant %v", name, a, got.Hits, want)
		}
		if len(want) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < len(queries)-4 {
		t.Fatalf("%s: only %d of %d queries had answers; the model is too sparse", name, nonEmpty, len(queries))
	}

	traces := map[string][]otel.SpanHit{}
	for _, s := range model {
		traces[s.trace] = append(traces[s.trace], otel.SpanHit{ID: s.id, Start: s.start})
	}
	for trace, want := range traces {
		slices.SortFunc(want, func(a, b otel.SpanHit) int { return int(a.Start - b.Start) })
		got, err := exp.Trace(ctx, trace)
		if err != nil {
			t.Fatalf("%s: Trace(%s): %v", name, trace, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: Trace(%s)\n got %v\nwant %v", name, trace, got, want)
		}
	}
	if got, err := exp.Trace(ctx, hex.EncodeToString(agentTraceID(99, 0))); err != nil || len(got) != 0 {
		t.Fatalf("%s: Trace(unknown) = %v, %v", name, got, err)
	}

	for _, s := range model {
		got, err := exp.Span(ctx, s.id)
		if err != nil {
			t.Fatalf("%s: Span(%s): %v", name, s.id, err)
		}
		if want := []otel.SpanMatch{{Trace: s.trace, ID: s.id, Start: s.start}}; !slices.Equal(got, want) {
			t.Fatalf("%s: Span(%s) = %v, want %v", name, s.id, got, want)
		}
		rs, err := exp.LoadSpanOTLP(ctx, s.trace, s.id)
		if err != nil {
			t.Fatalf("%s: LoadSpanOTLP(%s, %s): %v", name, s.trace, s.id, err)
		}
		if !proto.Equal(rs, s.stored) {
			t.Fatalf("%s: LoadSpanOTLP(%s, %s) lost fidelity\n got %v\nwant %v", name, s.trace, s.id, rs, s.stored)
		}
	}
	missingTrace, missingSpan := hex.EncodeToString(agentTraceID(0, 0)), hex.EncodeToString(agentSpanID(0, 0, 99))
	var notFound *otel.SpanNotFoundError
	if _, err := exp.LoadSpanOTLP(ctx, missingTrace, missingSpan); !errors.As(err, &notFound) || notFound.Trace != missingTrace || notFound.Span != missingSpan {
		t.Fatalf("%s: LoadSpanOTLP(missing) = %T %v", name, err, err)
	}
	if _, err := exp.LoadSpanOTLP(ctx, "XYZ", missingSpan); err == nil {
		t.Fatalf("%s: LoadSpanOTLP accepted a malformed trace id", name)
	}
	if got, err := exp.Span(ctx, missingSpan); err != nil || len(got) != 0 {
		t.Fatalf("%s: Span(missing) = %v, %v", name, got, err)
	}

	windows := []otel.TimeRange{
		{Start: collectorBase - 1e9, End: collectorBase},
		{Start: model[0].start, End: model[0].start},
		{Start: model[7].start, End: model[7].start + 1},
		{Start: collectorBase, End: collectorBase + collectorAgents*1e9},
	}
	for agent := range collectorAgents {
		lo := collectorBase + int64(agent)*1e9
		windows = append(windows, otel.TimeRange{Start: lo, End: lo + 1e9}, otel.TimeRange{Start: lo + 1e7 + 2e3, End: lo + 3e7 + 3e3})
	}
	for _, w := range windows {
		got, err := exp.Time(ctx, w)
		if err != nil {
			t.Fatalf("%s: Time(%+v): %v", name, w, err)
		}
		want := modelHits(model, k, func(s modelSpan) bool { return s.start >= w.Start && s.start < w.End })
		if !slices.Equal(got.Hits, want) {
			t.Fatalf("%s: Time(%+v)\n got %v\nwant %v", name, w, got.Hits, want)
		}
	}
	if _, err := exp.Time(ctx, otel.TimeRange{Start: 2, End: 1}); err == nil {
		t.Fatalf("%s: Time accepted an inverted range", name)
	}
}
