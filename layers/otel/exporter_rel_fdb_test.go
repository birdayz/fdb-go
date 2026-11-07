package otel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/recordlayer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Ports of TestExporterRealFDB (exporter_test.go) and its neighbours to the
// relational exporter. One FDB testcontainer serves every subtest; each opens
// a fresh run, whose tenant store EnsureTenant creates.
func TestRelExporterRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	for name, test := range map[string]func(*testing.T, string){
		"sdk_syncer_genai":          testRelSDKSyncer,
		"sdk_batch_span_processor":  testRelBatchSpanProcessor,
		"otlp_fidelity":             testRelOTLPFidelity,
		"producer_fidelity_tag":     testRelProducerFidelityTag,
		"batching_dedup_idempotent": testRelBatching,
		"shutdown_and_reopen":       testRelShutdown,
		"concurrent_exports":        testRelConcurrent,
		"ownership":                 testRelOwnership,
		"hmac_race":                 testRelHMACRace,
		"messages":                  testRelMessages,
		"admission_caps":            testRelAdmissionCaps,
		"query_budget_edges":        testRelQueryBudgetEdges,
		"transaction_guard":         testRelTransactionGuard,
		"typed_scoped_controls":     testRelTypedScopedControls,
		"selective_v2":              testRelSelectiveV2,
		"public_query_replay":       testRelPublicQueryReplay,
		"intrinsic_v2":              testRelIntrinsicV2,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, cluster)
		})
	}
}

// Port of testExporterSDKSyncer. D1: bools and doubles are queried typed;
// arrays, maps and bytes are not queryable, so their v1 canonical-string
// queries find nothing. The layout matrix is gone (D2).
func testRelSDKSyncer(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	rec := tracetest.NewSpanRecorder()
	res := resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.26.0",
		attribute.String("service.name", "genai-gateway"),
		attribute.String("deployment.environment.name", "integration"),
		attribute.Int64("service.shard", 3),
		attribute.Bool("service.canary", true),
		attribute.StringSlice("service.regions", []string{"us-east-1", "eu-west-1"}),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(e), sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	tpOpen := true
	t.Cleanup(func() {
		if tpOpen {
			_ = tp.Shutdown(context.Background())
		}
	})
	tracer := tp.Tracer(
		"fdb.dev/layers/otel/exporter_test",
		trace.WithInstrumentationVersion("1.4.0"),
		trace.WithSchemaURL("https://opentelemetry.io/schemas/1.26.0"),
		trace.WithInstrumentationAttributes(attribute.String("scope.team", "ml-platform"), attribute.Bool("scope.experimental", true), attribute.Float64("scope.sample_ratio", 0.25)),
	)

	base := time.Unix(1_750_000_000, 0)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	ns := func(d time.Duration) int64 { return at(d).UnixNano() }
	ms := time.Millisecond
	responseID := "chatcmpl-" + freshRun(t)

	rootCtx, agent := tracer.Start(context.Background(), "invoke_agent planner", trace.WithTimestamp(at(0)), trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", "planner"),
		attribute.String("gen_ai.request.model", "gpt-4o"),
		attribute.String("custom.session", "session-1"),
	))
	_, chat := tracer.Start(rootCtx, "chat gpt-4o", trace.WithTimestamp(at(10*ms)), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.system", "openai"),
		attribute.String("gen_ai.request.model", "gpt-4o"),
		attribute.String("gen_ai.response.model", "gpt-4o-2024-08-06"),
		attribute.String("gen_ai.response.id", responseID),
		attribute.Int64("gen_ai.usage.input_tokens", 1234),
		attribute.Int64("gen_ai.usage.output_tokens", 56),
		attribute.Float64("gen_ai.request.temperature", 0.7),
		attribute.Bool("gen_ai.request.stream", true),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{"stop", "length"}),
		attribute.Int64Slice("custom.shard_ids", []int64{3, 1, 4}),
		attribute.Float64("custom.score", math.NaN()),
		attribute.Float64("custom.ceiling", math.Inf(1)),
		attribute.Float64("custom.floor", math.Inf(-1)),
		attribute.Map("custom.labels", attribute.String("tier", "gold"), attribute.Int("rank", 2)),
	))
	chat.AddEvent("gen_ai.content.prompt", trace.WithTimestamp(at(11*ms)), trace.WithAttributes(attribute.String("gen_ai.prompt", "What is FoundationDB?")))
	chat.AddEvent("gen_ai.content.completion", trace.WithTimestamp(at(39*ms)), trace.WithAttributes(attribute.String("gen_ai.completion", "A distributed ordered KV store."), attribute.Int64("gen_ai.choice.index", 0)))
	chat.SetStatus(codes.Error, "rate limited")
	chat.End(trace.WithTimestamp(at(40 * ms)))

	// Same start as chat: Trace orders the tie by span ID.
	_, tool := tracer.Start(rootCtx, "execute_tool search", trace.WithTimestamp(at(10*ms)), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", "search"),
		attribute.ByteSlice("custom.digest", []byte{0xde, 0xad, 0xbe, 0xef}),
	))
	tool.SetStatus(codes.Ok, "")
	tool.End(trace.WithTimestamp(at(20 * ms)))
	agent.AddEvent("agent.plan", trace.WithTimestamp(at(1*ms)))
	agent.SetStatus(codes.Ok, "")
	agent.End(trace.WithTimestamp(at(50 * ms)))

	// A later, separate trace links back to the agent span.
	_, embed := tracer.Start(context.Background(), "embeddings text-embedding-3-small", trace.WithTimestamp(at(time.Second)), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithLinks(trace.Link{SpanContext: agent.SpanContext(), Attributes: []attribute.KeyValue{attribute.String("link.reason", "follow-up")}}),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "embeddings"),
			attribute.String("gen_ai.request.model", "text-embedding-3-small"),
			attribute.String("gen_ai.response.id", responseID+"-embed"),
			attribute.Bool("gen_ai.request.stream", false),
			attribute.Float64("gen_ai.request.temperature", 0.7),
		))
	embed.End(trace.WithTimestamp(at(time.Second + 5*ms)))

	t1, t2 := agent.SpanContext().TraceID().String(), embed.SpanContext().TraceID().String()
	agentID, chatID, toolID, embedID := agent.SpanContext().SpanID().String(), chat.SpanContext().SpanID().String(), tool.SpanContext().SpanID().String(), embed.SpanContext().SpanID().String()
	if t1 == t2 || chat.SpanContext().TraceID().String() != t1 || tool.SpanContext().TraceID().String() != t1 {
		t.Fatal("unexpected SDK trace topology")
	}
	hit := func(tr string, d time.Duration) Hit { return Hit{Trace: tr, Latest: ns(d)} }
	span := func(k, v string) Attribute { return Attribute{Scope: "span", Key: k, Type: "string", Value: v} }
	typed := func(scope, k string, v *commonpb.AnyValue) Attribute { return typedAttr(t, scope, k, v) }
	both := []Hit{hit(t2, time.Second), hit(t1, 10*ms)}

	queries := []struct {
		name string
		a    Attribute
		want []Hit
	}{
		{"operation chat", span("gen_ai.operation.name", "chat"), []Hit{hit(t1, 10*ms)}},
		{"operation invoke_agent", span("gen_ai.operation.name", "invoke_agent"), []Hit{hit(t1, 0)}},
		{"operation execute_tool", span("gen_ai.operation.name", "execute_tool"), []Hit{hit(t1, 10*ms)}},
		{"operation embeddings", span("gen_ai.operation.name", "embeddings"), []Hit{hit(t2, time.Second)}},
		{"operation absent", span("gen_ai.operation.name", "text_completion"), nil},
		{"request model on parent and child", span("gen_ai.request.model", "gpt-4o"), []Hit{hit(t1, 10*ms)}},
		{"request model embeddings", span("gen_ai.request.model", "text-embedding-3-small"), []Hit{hit(t2, time.Second)}},
		{"response id", span("gen_ai.response.id", responseID), []Hit{hit(t1, 10*ms)}},
		{"response id embeddings", span("gen_ai.response.id", responseID+"-embed"), []Hit{hit(t2, time.Second)}},
		{"response id absent", span("gen_ai.response.id", responseID+"-missing"), nil},
		{"int attribute", Attribute{"span", "gen_ai.usage.input_tokens", "int", "1234"}, []Hit{hit(t1, 10*ms)}},
		{"int attribute is not a string", span("gen_ai.usage.input_tokens", "1234"), nil},
		{"bool true", typed("span", "gen_ai.request.stream", exBool(true)), []Hit{hit(t1, 10*ms)}},
		{"bool false", typed("span", "gen_ai.request.stream", exBool(false)), []Hit{hit(t2, time.Second)}},
		// D1: a bool is no longer its canonical string.
		{"bool as string", span("gen_ai.request.stream", "true"), nil},
		{"float shared by both traces", typed("span", "gen_ai.request.temperature", exDouble(0.7)), both},
		{"float NaN", typed("span", "custom.score", exDouble(math.NaN())), []Hit{hit(t1, 10*ms)}},
		{"float +Inf", typed("span", "custom.ceiling", exDouble(math.Inf(1))), []Hit{hit(t1, 10*ms)}},
		{"float -Inf", typed("span", "custom.floor", exDouble(math.Inf(-1))), []Hit{hit(t1, 10*ms)}},
		// D1: arrays, maps and bytes are reported but not queryable.
		{"string array", CanonicalAttribute("span", "gen_ai.response.finish_reasons", exArray(exStr("stop"), exStr("length"))), nil},
		{"string array is ordered", CanonicalAttribute("span", "gen_ai.response.finish_reasons", exArray(exStr("length"), exStr("stop"))), nil},
		{"int array", CanonicalAttribute("span", "custom.shard_ids", exArray(exInt(3), exInt(1), exInt(4))), nil},
		{"map", CanonicalAttribute("span", "custom.labels", exKvlist(exKV("rank", exInt(2)), exKV("tier", exStr("gold")))), nil},
		{"bytes", CanonicalAttribute("span", "custom.digest", exBytes([]byte{0xde, 0xad, 0xbe, 0xef})), nil},
		{"span attribute is not a resource attribute", Attribute{"resource", "gen_ai.operation.name", "string", "chat"}, nil},
		{"resource string", Attribute{"resource", "service.name", "string", "genai-gateway"}, both},
		{"resource int", Attribute{"resource", "service.shard", "int", "3"}, both},
		{"resource bool", typed("resource", "service.canary", exBool(true)), both},
		{"resource array", CanonicalAttribute("resource", "service.regions", exArray(exStr("us-east-1"), exStr("eu-west-1"))), nil},
		{"resource value absent", Attribute{"resource", "service.name", "string", "other"}, nil},
		{"scope string", Attribute{"scope", "scope.team", "string", "ml-platform"}, both},
		{"scope bool", typed("scope", "scope.experimental", exBool(true)), both},
		{"scope float", typed("scope", "scope.sample_ratio", exDouble(0.25)), both},
		{"event attributes are not indexed", span("gen_ai.prompt", "What is FoundationDB?"), nil},
		{"link attributes are not indexed", span("link.reason", "follow-up"), nil},
	}
	for _, q := range queries {
		t.Run("query "+q.name, func(t *testing.T) { reHits(t, ctx, e, q.a, q.want...) })
	}

	reTrace(t, ctx, e, t1, append([]SpanHit{{agentID, ns(0)}}, sortedSpanHits(SpanHit{chatID, ns(10 * ms)}, SpanHit{toolID, ns(10 * ms)})...)...)
	reTrace(t, ctx, e, t2, SpanHit{embedID, ns(time.Second)})
	reTrace(t, ctx, e, hex.EncodeToString(exRandomID(t, 16)))

	reSpan(t, ctx, e, agentID, SpanMatch{t1, agentID, ns(0)})
	reSpan(t, ctx, e, chatID, SpanMatch{t1, chatID, ns(10 * ms)})
	reSpan(t, ctx, e, toolID, SpanMatch{t1, toolID, ns(10 * ms)})
	reSpan(t, ctx, e, embedID, SpanMatch{t2, embedID, ns(time.Second)})
	reSpan(t, ctx, e, hex.EncodeToString(exRandomID(t, 8)))

	window := func(from, to int64) TimeRange { return TimeRange{Start: from, End: to} }
	reTime(t, ctx, e, window(ns(0), ns(10*ms)), hit(t1, 0))             // end is exclusive
	reTime(t, ctx, e, window(ns(0), ns(10*ms)+1), hit(t1, 10*ms))       // latest start in window
	reTime(t, ctx, e, window(ns(0), ns(time.Second)), hit(t1, 10*ms))   // excludes t2's start
	reTime(t, ctx, e, window(ns(0), ns(time.Second)+1), both...)        // includes t2's start
	reTime(t, ctx, e, window(ns(10*ms), ns(time.Second)+1), both...)    // start is inclusive
	reTime(t, ctx, e, window(ns(0)+1, ns(10*ms)), nil...)               // gap between spans
	reTime(t, ctx, e, window(ns(10*ms)+1, ns(time.Second)), nil...)     // gap between traces
	reTime(t, ctx, e, window(ns(time.Second), ns(time.Second)), nil...) // empty window
	reTime(t, ctx, e, window(ns(-time.Hour), ns(0)), nil...)            // before everything
	reTime(t, ctx, e, window(math.MinInt64, math.MaxInt64), both...)    // unbounded
	if _, err := e.Time(ctx, window(ns(time.Second), ns(0))); err == nil {
		t.Fatal("inverted time window accepted")
	} else {
		requireOtelKind(t, err, "config")
	}

	ended := rec.Ended()
	if len(ended) != 4 {
		t.Fatalf("recorder saw %d spans", len(ended))
	}
	reRecorded(t, ctx, e, ended)

	// Spot-check the restored chat span directly against the SDK inputs.
	got, err := e.LoadSpanOTLP(ctx, t1, chatID)
	if err != nil {
		t.Fatal(err)
	}
	sp := got.ScopeSpans[0].Spans[0]
	agentSID := agent.SpanContext().SpanID()
	if sp.Name != "chat gpt-4o" || sp.Kind != tracepb.Span_SPAN_KIND_CLIENT || !reflect.DeepEqual(sp.ParentSpanId, agentSID[:]) ||
		sp.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || sp.Status.GetMessage() != "rate limited" ||
		sp.StartTimeUnixNano != uint64(ns(10*ms)) || sp.EndTimeUnixNano != uint64(ns(40*ms)) {
		t.Fatalf("restored chat span: %v", sp)
	}
	if len(sp.Events) != 2 || sp.Events[0].Name != "gen_ai.content.prompt" || sp.Events[1].Name != "gen_ai.content.completion" || exFind(sp.Events[1].Attributes, "gen_ai.choice.index").GetIntValue() != 0 {
		t.Fatalf("restored chat events: %v", sp.Events)
	}
	if v := exFind(sp.Attributes, "custom.score"); v == nil || !math.IsNaN(v.GetDoubleValue()) {
		t.Fatalf("NaN attribute not restored as double: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.request.temperature"); v.GetDoubleValue() != 0.7 {
		t.Fatalf("temperature not restored as double: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.request.stream"); v == nil || v.GetValue() == nil || !v.GetBoolValue() {
		t.Fatalf("stream not restored as bool: %v", v)
	}
	if v := exFind(sp.Attributes, "gen_ai.response.finish_reasons"); len(v.GetArrayValue().GetValues()) != 2 {
		t.Fatalf("finish reasons not restored as array: %v", v)
	}
	if v := exFind(sp.Attributes, "custom.labels"); len(v.GetKvlistValue().GetValues()) != 2 {
		t.Fatalf("labels not restored as map: %v", v)
	}
	if v := exFind(got.Resource.Attributes, "service.canary"); !v.GetBoolValue() {
		t.Fatalf("resource bool not restored: %v", got.Resource)
	}
	if sc := got.ScopeSpans[0].Scope; sc.Name != "fdb.dev/layers/otel/exporter_test" || sc.Version != "1.4.0" || !exFind(sc.Attributes, "scope.experimental").GetBoolValue() {
		t.Fatalf("restored scope: %v", sc)
	}
	embedded, err := e.LoadSpanOTLP(ctx, t2, embedID)
	if err != nil {
		t.Fatal(err)
	}
	links := embedded.ScopeSpans[0].Spans[0].Links
	agentTID := agent.SpanContext().TraceID()
	if len(links) != 1 || !reflect.DeepEqual(links[0].TraceId, agentTID[:]) || !reflect.DeepEqual(links[0].SpanId, agentSID[:]) || exFind(links[0].Attributes, "link.reason").GetStringValue() != "follow-up" {
		t.Fatalf("restored links: %v", links)
	}

	// TracerProvider shutdown shuts the exporter down too.
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	tpOpen = false
	_, err = e.Query(ctx, span("gen_ai.operation.name", "chat"))
	requireOtelKind(t, err, "operation")
}

// Port of testExporterBatchSpanProcessor; D1: the bool and double are typed.
func testRelBatchSpanProcessor(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 2, 3 })
	rec := tracetest.NewSpanRecorder()
	bsp := sdktrace.NewBatchSpanProcessor(e, sdktrace.WithMaxExportBatchSize(5), sdktrace.WithBatchTimeout(time.Hour))
	res := resource.NewSchemaless(attribute.String("service.name", "batch-agent"), attribute.Float64("service.version_ratio", 1.5))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(bsp), sdktrace.WithSpanProcessor(rec), sdktrace.WithResource(res))
	tpOpen := true
	t.Cleanup(func() {
		if tpOpen {
			_ = tp.Shutdown(context.Background())
		}
	})
	tracer := tp.Tracer("batch", trace.WithInstrumentationAttributes(attribute.String("scope.kind", "batch")))
	base := time.Unix(1_760_000_000, 0)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }

	const tools = 11
	rootCtx, root := tracer.Start(context.Background(), "invoke_agent batcher", trace.WithTimestamp(at(0)), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.request.model", "claude-sonnet"),
	))
	var want []SpanHit
	var calls []trace.Span
	for i := 1; i <= tools; i++ {
		_, s := tracer.Start(rootCtx, fmt.Sprintf("execute_tool step-%d", i), trace.WithTimestamp(at(i)), trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.call.id", fmt.Sprintf("call-%02d", i)),
			attribute.Int("gen_ai.tool.step", i),
			attribute.Bool("gen_ai.tool.even", i%2 == 0),
		))
		s.AddEvent("tool.result", trace.WithTimestamp(at(i)), trace.WithAttributes(attribute.Int("bytes", i*10)))
		if i == tools {
			s.SetStatus(codes.Error, "tool failed")
		}
		s.End(trace.WithTimestamp(at(i + 1)))
		calls = append(calls, s)
		want = append(want, SpanHit{s.SpanContext().SpanID().String(), at(i).UnixNano()})
	}
	root.End(trace.WithTimestamp(at(tools + 2)))
	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	tr := root.SpanContext().TraceID().String()
	want = append([]SpanHit{{root.SpanContext().SpanID().String(), at(0).UnixNano()}}, want...)
	reTrace(t, ctx, e, tr, want...)
	reHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "execute_tool"}, Hit{tr, at(tools).UnixNano()})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.request.model", "string", "claude-sonnet"}, Hit{tr, at(0).UnixNano()})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.tool.call.id", "string", "call-07"}, Hit{tr, at(7).UnixNano()})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.tool.step", "int", "3"}, Hit{tr, at(3).UnixNano()})
	reHits(t, ctx, e, typedAttr(t, "span", "gen_ai.tool.even", exBool(true)), Hit{tr, at(10).UnixNano()})
	reHits(t, ctx, e, typedAttr(t, "resource", "service.version_ratio", exDouble(1.5)), Hit{tr, at(tools).UnixNano()})
	reHits(t, ctx, e, Attribute{"scope", "scope.kind", "string", "batch"}, Hit{tr, at(tools).UnixNano()})
	for i, s := range calls {
		reSpan(t, ctx, e, s.SpanContext().SpanID().String(), SpanMatch{tr, s.SpanContext().SpanID().String(), at(i + 1).UnixNano()})
	}
	reTime(t, ctx, e, TimeRange{at(3).UnixNano(), at(6).UnixNano()}, Hit{tr, at(5).UnixNano()})
	if len(rec.Ended()) != tools+1 {
		t.Fatalf("recorder saw %d spans", len(rec.Ended()))
	}
	reRecorded(t, ctx, e, rec.Ended())

	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	tpOpen = false
	_, err := e.Trace(ctx, tr)
	requireOtelKind(t, err, "operation")
}

// Port of testExporterOTLPFidelity. The row's otlp is the original span, so
// LoadSpanOTLP returns duplicates and unindexed values without a fidelity
// payload (§1.2). D1: bytes, kvlists and unset values are reported as
// unindexed (UnindexedAttributes 5, v1 2) and their v1 canonical-string
// queries find nothing; bools and doubles are queried typed.
func testRelOTLPFidelity(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	traceID, spanA, spanB := exRandomID(t, 16), exRandomID(t, 8), exRandomID(t, 8)
	tr, idA, idB := hex.EncodeToString(traceID), hex.EncodeToString(spanA), hex.EncodeToString(spanB)
	const start = int64(1_770_000_000_000_000_000)
	longKey := strings.Repeat("k", maxIndexedKeyBytes+1)
	hugeValue := strings.Repeat("v", maxIndexedValueBytes+1)
	nested := exKvlist(exKV("list", exArray(exInt(1), exDouble(2.5), exBool(false))), exKV("who", exStr("me")), exKV("list", exStr("again")))

	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			exKV("service.name", exStr("fidelity-svc")),
			exKV("res.dup", exInt(1)),
			exKV("res.ratio", exDouble(0.25)),
			exKV("res.dup", exStr("two")),
		}, DroppedAttributesCount: 1},
		SchemaUrl: "https://example.test/resource",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: "fidelity", Version: "9", Attributes: []*commonpb.KeyValue{exKV("scope.flag", exBool(false)), exKV("scope.blob", exBytes([]byte{1, 2, 3}))}},
			SchemaUrl: "https://example.test/scope",
			Spans: []*tracepb.Span{{
				TraceId: traceID, SpanId: spanA, TraceState: "vendor=x", Name: "chat fidelity", Kind: tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(start + 1000), Flags: 0x101,
				Attributes: []*commonpb.KeyValue{
					exKV("gen_ai.operation.name", exStr("chat")),
					exKV("gen_ai.response.id", exStr("resp-fidelity")),
					exKV("dup", exInt(1)),
					exKV("flag", exBool(true)),
					exKV("nested", nested),
					exKV("nanv", exDouble(math.NaN())),
					exKV("dup", exInt(2)),
					exKV(longKey, exStr("v")),
					exKV("huge", exStr(hugeValue)),
					exKV("min", exInt(math.MinInt64)),
					exKV("empty", &commonpb.AnyValue{}),
				},
				DroppedAttributesCount: 2,
				Events:                 []*tracepb.Span_Event{{TimeUnixNano: uint64(start + 10), Name: "evt", Attributes: []*commonpb.KeyValue{exKV("ratio", exDouble(1.5))}}},
				Links:                  []*tracepb.Span_Link{{TraceId: exRandomID(t, 16), SpanId: exRandomID(t, 8), Attributes: []*commonpb.KeyValue{exKV("kind", exBool(true))}}},
				Status:                 &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
			}},
		}},
	}
	// Unknown wrapper fields are preserved.
	rs.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 4242, protowire.VarintType), 7))
	rs.ScopeSpans[0].ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 4343, protowire.BytesType), "scope-extra"))
	plain := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("plain-svc")), exKV("service.shard", exInt(4))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "plain"}, Spans: []*tracepb.Span{{
			TraceId: traceID, SpanId: spanB, ParentSpanId: spanA, Name: "plain child", StartTimeUnixNano: uint64(start + 100), EndTimeUnixNano: uint64(start + 200),
			Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("embeddings")), exKV("gen_ai.usage.input_tokens", exInt(9))},
		}}}},
	}
	original := proto.Clone(rs).(*tracepb.ResourceSpans)

	stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs, plain})
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalBytes <= int64(len(hugeValue)) {
		t.Fatalf("logical bytes %d omit the stored payload", stats.LogicalBytes)
	}
	stats.LogicalBytes = 0
	if want := (ExportStats{ReceivedSpans: 2, Spans: 2, Batches: 1, NormalizedSpans: 1, UnindexedAttributes: 5}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}
	if !proto.Equal(rs, original) {
		t.Fatal("export mutated its input")
	}

	hitA, hitB := Hit{tr, start}, Hit{tr, start + 100}
	typed := func(scope, k string, v *commonpb.AnyValue) Attribute { return typedAttr(t, scope, k, v) }
	for _, q := range []struct {
		a    Attribute
		want []Hit
	}{
		{typed("span", "gen_ai.operation.name", exStr("chat")), []Hit{hitA}},
		{typed("span", "gen_ai.operation.name", exStr("embeddings")), []Hit{hitB}},
		{typed("span", "gen_ai.response.id", exStr("resp-fidelity")), []Hit{hitA}},
		{typed("span", "dup", exInt(2)), []Hit{hitA}},
		{typed("span", "dup", exInt(1)), nil},
		{typed("span", "flag", exBool(true)), []Hit{hitA}},
		{typed("span", "nanv", exDouble(math.NaN())), []Hit{hitA}},
		{typed("span", "min", exInt(math.MinInt64)), []Hit{hitA}},
		{CanonicalAttribute("span", "nested", nested), nil},
		{Attribute{"span", "nested", "string", `{"list":[1,2.5,false],"who":"me","list":"again"}`}, nil},
		{Attribute{"span", "empty", "string", ""}, nil},
		{Attribute{"span", longKey, "string", "v"}, nil},
		{Attribute{"span", "huge", "string", hugeValue}, nil},
		{typed("span", "ratio", exDouble(1.5)), nil},
		{typed("span", "gen_ai.usage.input_tokens", exInt(9)), []Hit{hitB}},
		{typed("resource", "service.name", exStr("fidelity-svc")), []Hit{hitA}},
		{typed("resource", "service.name", exStr("plain-svc")), []Hit{hitB}},
		{typed("resource", "service.shard", exInt(4)), []Hit{hitB}},
		{typed("resource", "res.dup", exStr("two")), []Hit{hitA}},
		{typed("resource", "res.dup", exInt(1)), nil},
		{typed("resource", "res.ratio", exDouble(0.25)), []Hit{hitA}},
		{typed("scope", "scope.flag", exBool(false)), []Hit{hitA}},
		{Attribute{"scope", "scope.blob", "string", "AQID"}, nil},
	} {
		reHits(t, ctx, e, q.a, q.want...)
	}
	reTrace(t, ctx, e, tr, SpanHit{idA, start}, SpanHit{idB, start + 100})

	got, err := e.LoadSpanOTLP(ctx, tr, idA)
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "normalized span", got, exSingle(original, original.ScopeSpans[0], original.ScopeSpans[0].Spans[0]))
	attrs := got.ScopeSpans[0].Spans[0].Attributes
	if len(attrs) != 11 || attrs[2].Value.GetIntValue() != 1 || attrs[6].Value.GetIntValue() != 2 || attrs[7].Key != longKey || attrs[8].Value.GetStringValue() != hugeValue {
		t.Fatalf("restored span attributes lost duplicates or unindexed values")
	}
	if ra := got.Resource.Attributes; len(ra) != 4 || ra[1].Value.GetIntValue() != 1 || ra[3].Value.GetStringValue() != "two" {
		t.Fatalf("restored resource attributes: %v", ra)
	}
	if _, ok := got.ScopeSpans[0].Scope.Attributes[1].Value.GetValue().(*commonpb.AnyValue_BytesValue); !ok {
		t.Fatalf("restored scope attribute type: %v", got.ScopeSpans[0].Scope.Attributes[1])
	}
	got, err = e.LoadSpanOTLP(ctx, tr, idB)
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "plain span", got, exSingle(plain, plain.ScopeSpans[0], plain.ScopeSpans[0].Spans[0]))

	// SDK ReadOnlySpans may carry duplicate keys; ExportSpans keeps them too.
	stubSC := exSpanContext(t, true, false, "")
	stub := tracetest.SpanStub{
		Name: "stub chat", SpanContext: stubSC, StartTime: time.Unix(0, start+200), EndTime: time.Unix(0, start+300),
		Attributes:           []attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat"), attribute.Int64("dup", 1), attribute.String("dup", "last"), attribute.Float64Slice("scores", []float64{math.Inf(1), 0.5})},
		Resource:             resource.NewSchemaless(attribute.String("service.name", "stub-svc")),
		InstrumentationScope: instrumentation.Scope{Name: "stub"},
	}.Snapshot()
	if err := e.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub}); err != nil {
		t.Fatal(err)
	}
	stubTrace, stubID := stubSC.TraceID().String(), stubSC.SpanID().String()
	stubHit := Hit{stubTrace, start + 200}
	reHits(t, ctx, e, Attribute{"span", "dup", "string", "last"}, stubHit)
	reHits(t, ctx, e, Attribute{"span", "dup", "int", "1"})                  // superseded in both spans
	reHits(t, ctx, e, Attribute{"span", "scores", "string", `["+Inf",0.5]`}) // D1: arrays are not queryable
	reHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, stubHit, hitA)
	reRecorded(t, ctx, e, []sdktrace.ReadOnlySpan{stub})
	got, err = e.LoadSpanOTLP(ctx, stubTrace, stubID)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.ScopeSpans[0].Spans[0].Attributes; len(a) != 4 || a[1].Value.GetIntValue() != 1 || a[2].Value.GetStringValue() != "last" {
		t.Fatalf("stub duplicates not restored: %v", a)
	}

	// Lookups of absent or malformed identities.
	missing := hex.EncodeToString(exRandomID(t, 8))
	_, err = e.LoadSpanOTLP(ctx, tr, missing)
	var notFound *SpanNotFoundError
	if !errors.As(err, &notFound) || notFound.Trace != tr || notFound.Span != missing {
		t.Fatalf("want SpanNotFoundError for %s/%s, got %T: %v", tr, missing, err, err)
	}
	requireOtelKind(t, err, "operation")
	for _, ids := range [][2]string{
		{strings.ToUpper(tr), idA},
		{tr, strings.ToUpper(idA)},
		{tr[:30], idA},
		{tr, idA + "00"},
		{strings.Repeat("0", 32), idA},
		{tr, strings.Repeat("0", 16)},
		{"zz" + tr[2:], idA},
		{"", ""},
	} {
		rs, err := e.LoadSpanOTLP(ctx, ids[0], ids[1])
		if rs != nil {
			t.Fatalf("load %q/%q returned a span", ids[0], ids[1])
		}
		requireOtelKind(t, err, "config")
	}

	// An inadmissible span fails the whole call without partial writes.
	goodID := exRandomID(t, 8)
	good := &tracepb.Span{TraceId: traceID, SpanId: goodID, StartTimeUnixNano: uint64(start + 500), Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("partial"))}}
	for name, bad := range map[string]*tracepb.Span{
		"zero span id":       {TraceId: traceID, SpanId: make([]byte, 8), StartTimeUnixNano: 1},
		"short trace id":     {TraceId: traceID[:15], SpanId: exRandomID(t, 8), StartTimeUnixNano: 1},
		"zero trace id":      {TraceId: make([]byte, 16), SpanId: exRandomID(t, 8), StartTimeUnixNano: 1},
		"start beyond int64": {TraceId: traceID, SpanId: exRandomID(t, 8), StartTimeUnixNano: math.MaxUint64},
	} {
		_, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{
			{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{good}}}},
			{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{bad}}}},
		})
		if err == nil {
			t.Fatalf("%s: inadmissible span accepted", name)
		}
		requireOtelKind(t, err, "admission")
		reSpan(t, ctx, e, hex.EncodeToString(goodID))
		reHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "partial"})
	}
	reTrace(t, ctx, e, tr, SpanHit{idA, start}, SpanHit{idB, start + 100})
}

// Port of TestExporterPreservesProducerFidelityTag: producer unknown fields,
// v1's internal fidelity tag 536870911 included, survive in the otlp column.
func testRelProducerFidelityTag(t *testing.T, cluster string) {
	for _, normalize := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "normalized"}[normalize], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			e := openRelExporter(t, ctx, cluster, nil)
			rs := deltaSpan(1)
			if normalize {
				rs.ScopeSpans[0].Spans[0].Attributes = []*commonpb.KeyValue{exKV("duplicate", exInt(1)), exKV("duplicate", exInt(2))}
			}
			var unknown []byte
			for _, payload := range [][]byte{[]byte("producer-owned"), {0xff}, nil} {
				unknown = protowire.AppendTag(unknown, fidelityField, protowire.BytesType)
				unknown = protowire.AppendBytes(unknown, payload)
			}
			rs.ProtoReflect().SetUnknown(unknown)
			before := proto.Clone(rs).(*tracepb.ResourceSpans)
			if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs}); err != nil {
				t.Fatal(err)
			}
			sp := rs.ScopeSpans[0].Spans[0]
			got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
			if err != nil {
				t.Fatal(err)
			}
			requireProtoEqual(t, "producer unknown tag", got, before)
			requireProtoEqual(t, "input remains unchanged", rs, before)
		})
	}
}

// Port of testExporterBatching, unchanged: batches, within-call last-wins,
// idempotent re-export, in-place replacement and canceled exports.
func testRelBatching(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 2, 3 })
	const start = int64(1_780_000_000_000_000_000)
	t1, t2 := exRandomID(t, 16), exRandomID(t, 16)
	ids := make([][]byte, 5)
	for i := range ids {
		ids[i] = exRandomID(t, 8)
	}
	mk := func(trace, span []byte, at int64, name, resp string) *tracepb.Span {
		return &tracepb.Span{TraceId: trace, SpanId: span, Name: name, StartTimeUnixNano: uint64(at), EndTimeUnixNano: uint64(at + 5), Attributes: []*commonpb.KeyValue{
			exKV("gen_ai.operation.name", exStr("chat")),
			exKV("gen_ai.response.id", exStr(resp)),
			exKV("gen_ai.usage.output_tokens", exInt(at-start)),
		}}
	}
	rsA := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("batch-a"))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "batch"}, Spans: []*tracepb.Span{
			mk(t1, ids[0], start, "s0", "resp-0"),
			mk(t1, ids[1], start+10, "s1 original", "resp-old"),
			mk(t1, ids[2], start+20, "s2", "resp-2"),
		}}},
	}
	rsB := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("batch-b"))}},
		ScopeSpans: []*tracepb.ScopeSpans{nil, {Scope: &commonpb.InstrumentationScope{Name: "batch"}, Spans: []*tracepb.Span{
			mk(t2, ids[3], start+30, "s3", "resp-3"),
			nil,
			mk(t1, ids[1], start+15, "s1 replacement", "resp-new"), // same identity as s1: later wins
			mk(t2, ids[4], start+40, "s4", "resp-4"),
		}}},
	}
	input := []*tracepb.ResourceSpans{rsA, nil, rsB}
	stats, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalBytes <= 0 {
		t.Fatalf("logical bytes %d", stats.LogicalBytes)
	}
	logical := stats.LogicalBytes
	stats.LogicalBytes = 0
	if want := (ExportStats{ReceivedSpans: 6, DuplicateSpans: 1, Spans: 5, Batches: 3}); stats != want {
		t.Fatalf("stats %+v want %+v", stats, want)
	}

	tr1, tr2 := hex.EncodeToString(t1), hex.EncodeToString(t2)
	id := func(i int) string { return hex.EncodeToString(ids[i]) }
	resp := func(v string) Attribute { return Attribute{"span", "gen_ai.response.id", "string", v} }
	type snapshot struct {
		traces  [2][]SpanHit
		queries []QueryResult
		time    QueryResult
	}
	queries := []Attribute{resp("resp-old"), resp("resp-new"), {"span", "gen_ai.operation.name", "string", "chat"}, {"resource", "service.name", "string", "batch-a"}, {"resource", "service.name", "string", "batch-b"}, {"span", "gen_ai.usage.output_tokens", "int", "15"}}
	take := func() snapshot {
		t.Helper()
		var s snapshot
		var err error
		if s.traces[0], err = e.Trace(ctx, tr1); err != nil {
			t.Fatal(err)
		}
		if s.traces[1], err = e.Trace(ctx, tr2); err != nil {
			t.Fatal(err)
		}
		for _, q := range queries {
			r, err := e.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			s.queries = append(s.queries, QueryResult{Hits: r.Hits, Candidates: r.Candidates})
		}
		r, err := e.Time(ctx, TimeRange{start, start + 41})
		if err != nil {
			t.Fatal(err)
		}
		s.time = QueryResult{Hits: r.Hits, Candidates: r.Candidates}
		return s
	}

	reTrace(t, ctx, e, tr1, SpanHit{id(0), start}, SpanHit{id(1), start + 15}, SpanHit{id(2), start + 20})
	reTrace(t, ctx, e, tr2, SpanHit{id(3), start + 30}, SpanHit{id(4), start + 40})
	reHits(t, ctx, e, resp("resp-old"))
	reHits(t, ctx, e, resp("resp-new"), Hit{tr1, start + 15})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.usage.output_tokens", "int", "10"})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.usage.output_tokens", "int", "15"}, Hit{tr1, start + 15})
	reHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, Hit{tr2, start + 40}, Hit{tr1, start + 20})
	reHits(t, ctx, e, Attribute{"resource", "service.name", "string", "batch-a"}, Hit{tr1, start + 20})
	reHits(t, ctx, e, Attribute{"resource", "service.name", "string", "batch-b"}, Hit{tr2, start + 40}, Hit{tr1, start + 15})
	reSpan(t, ctx, e, id(1), SpanMatch{tr1, id(1), start + 15})
	reTime(t, ctx, e, TimeRange{start + 10, start + 15}) // the replaced start was never written
	reTime(t, ctx, e, TimeRange{start + 10, start + 16}, Hit{tr1, start + 15})
	got, err := e.LoadSpanOTLP(ctx, tr1, id(1))
	if err != nil {
		t.Fatal(err)
	}
	requireProtoEqual(t, "deduplicated span", got, exSingle(rsB, rsB.ScopeSpans[1], rsB.ScopeSpans[1].Spans[2]))
	before := take()
	if q := before.queries[2]; q.Candidates != 5 {
		t.Fatalf("operation entries = %d, want one per stored span", q.Candidates)
	}

	// Re-exporting the same input is idempotent: same stats, rows and entries.
	again, err := e.ExportResourceSpans(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if again.LogicalBytes != logical {
		t.Fatalf("re-export logical bytes %d, want %d", again.LogicalBytes, logical)
	}
	again.LogicalBytes = 0
	if again != stats {
		t.Fatalf("re-export stats %+v want %+v", again, stats)
	}
	if err := e.ConsumeTraces(ctx, input); err != nil {
		t.Fatal(err)
	}
	if after := take(); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-export changed state\nbefore %+v\n after %+v", before, after)
	}

	// Re-exporting one identity with new content replaces it in place.
	update := &tracepb.ResourceSpans{Resource: rsA.Resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: rsA.ScopeSpans[0].Scope, Spans: []*tracepb.Span{mk(t1, ids[0], start+1, "s0 v2", "resp-0-v2")}}}}
	if err := e.ConsumeTraces(ctx, []*tracepb.ResourceSpans{update}); err != nil {
		t.Fatal(err)
	}
	reHits(t, ctx, e, resp("resp-0"))
	reHits(t, ctx, e, resp("resp-0-v2"), Hit{tr1, start + 1})
	reTrace(t, ctx, e, tr1, SpanHit{id(0), start + 1}, SpanHit{id(1), start + 15}, SpanHit{id(2), start + 20})
	reTime(t, ctx, e, TimeRange{start, start + 1})
	if r, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"}); err != nil || r.Candidates != 5 {
		t.Fatalf("operation entries after update: %+v %v", r, err)
	}

	// Empty and all-nil inputs commit nothing.
	for name, in := range map[string][]*tracepb.ResourceSpans{
		"nil":       nil,
		"empty":     {},
		"nil items": {nil, {}, {ScopeSpans: []*tracepb.ScopeSpans{nil, {}, {Spans: []*tracepb.Span{nil}}}}},
	} {
		s, err := e.ExportResourceSpans(ctx, in)
		if err != nil || s != (ExportStats{}) {
			t.Fatalf("%s: stats %+v err %v", name, s, err)
		}
	}
	if err := e.ExportSpans(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// A canceled export writes nothing.
	canceled, cancelExport := context.WithCancel(ctx)
	cancelExport()
	fresh := &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{mk(t1, exRandomID(t, 8), start+99, "late", "resp-late")}}}}
	_, err = e.ExportResourceSpans(canceled, []*tracepb.ResourceSpans{fresh})
	requireOtelKind(t, err, "canceled")
	reHits(t, ctx, e, resp("resp-late"))
}

// Port of testExporterShutdown. The layout identity is the STORE_META
// layout row (D10, §3.4), so the refused reopen changes the message flags
// (v1 changed hash_bytes, a knob D2 removes).
func testRelShutdown(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	traceID, spanID := exRandomID(t, 16), exRandomID(t, 8)
	tr, id := hex.EncodeToString(traceID), hex.EncodeToString(spanID)
	const start = int64(1_790_000_000_000_000_000)
	in := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: traceID, SpanId: spanID, StartTimeUnixNano: uint64(start), Attributes: []*commonpb.KeyValue{exKV("gen_ai.operation.name", exStr("chat"))}}}}}}}
	if _, err := e.ExportResourceSpans(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	requireShutDown := func(name string, err error) {
		t.Helper()
		requireOtelKind(t, err, "operation")
		if !strings.Contains(err.Error(), "shut down") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	stats, err := e.ExportResourceSpans(ctx, in)
	requireShutDown("ExportResourceSpans", err)
	if stats != (ExportStats{}) {
		t.Fatalf("rejected export reported stats %+v", stats)
	}
	requireShutDown("ExportResourceSpans empty", func() error { _, err := e.ExportResourceSpans(ctx, nil); return err }())
	requireShutDown("ConsumeTraces", e.ConsumeTraces(ctx, in))
	stub := tracetest.SpanStub{SpanContext: exSpanContext(t, true, false, ""), StartTime: time.Unix(0, start)}.Snapshot()
	requireShutDown("ExportSpans", e.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub}))
	q, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
	requireShutDown("Query", err)
	if q.Hits == nil || len(q.Hits) != 0 {
		t.Fatalf("rejected query hits %+v", q.Hits)
	}
	hits, err := e.Trace(ctx, tr)
	requireShutDown("Trace", err)
	if hits == nil || len(hits) != 0 {
		t.Fatalf("rejected trace %+v", hits)
	}
	matches, err := e.Span(ctx, id)
	requireShutDown("Span", err)
	if matches == nil || len(matches) != 0 {
		t.Fatalf("rejected span %+v", matches)
	}
	tq, err := e.Time(ctx, TimeRange{start, start + 1})
	requireShutDown("Time", err)
	if tq.Hits == nil || len(tq.Hits) != 0 {
		t.Fatalf("rejected time %+v", tq.Hits)
	}
	loaded, err := e.LoadSpanOTLP(ctx, tr, id)
	requireShutDown("LoadSpanOTLP", err)
	if loaded != nil {
		t.Fatal("rejected load returned a span")
	}
	page, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.operation.name", "string", "chat"}}})
	requireShutDown("QuerySameSpan", err)
	if page.Spans == nil || len(page.Spans) != 0 {
		t.Fatalf("rejected same-span page %+v", page)
	}
	_, err = e.QueryTraceExistential(ctx, TraceExistentialQuery{Clauses: []Attribute{{"span", "gen_ai.operation.name", "string", "chat"}}})
	requireShutDown("QueryTraceExistential", err)
	_, err = e.TraverseTraceGraph(ctx, tr)
	requireShutDown("TraverseTraceGraph", err)

	// Committed data outlives the handle; reopening verifies the store layout.
	reopened := startRelExporter(t, ctx, e.Config(), relParts{})
	reTrace(t, ctx, reopened, tr, SpanHit{id, start})
	reHits(t, ctx, reopened, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, Hit{tr, start})

	// Another tenant of the same run sees none of it.
	other := e.Config()
	other.Tenant++
	neighbor := startRelExporter(t, ctx, other, relParts{})
	reTrace(t, ctx, neighbor, tr)
	reSpan(t, ctx, neighbor, id)
	reHits(t, ctx, neighbor, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
	reTime(t, ctx, neighbor, TimeRange{start, start + 1})
	if _, err := neighbor.LoadSpanOTLP(ctx, tr, id); err == nil {
		t.Fatal("neighbor tenant loaded another tenant's span")
	}

	// A layout change on an existing store is refused, either way.
	mismatch := e.Config()
	mismatch.EnableMessageDeltas = true
	if x, err := newExporter(ctx, mismatch, reMessageParts(&reMessages{})); err == nil {
		_ = x.Shutdown(context.Background())
		t.Fatal("layout mismatch accepted")
	} else {
		requireOtelKind(t, err, "ownership")
	}
	deltas := relTestConfig(t, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	startRelExporter(t, ctx, deltas, reMessageParts(&reMessages{}))
	deltas.EnableMessageDeltas = false
	if x, err := newExporter(ctx, deltas, relParts{}); err == nil {
		_ = x.Shutdown(context.Background())
		t.Fatal("dropping messages from a deltas store accepted")
	} else {
		requireOtelKind(t, err, "ownership")
	}
	// Brotli records are refused before opening. (Messages need no supplied
	// hooks: relParts.messages defaults to newRelMessageStore.)
	for name, mutate := range map[string]func(*ExporterConfig){
		"brotli": func(c *ExporterConfig) { c.RecordCompression = "brotli-4" },
	} {
		_, err := newExporter(ctx, relTestConfig(t, cluster, mutate), relParts{})
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		requireOtelKind(t, err, "config")
	}
}

// Port of testExporterConcurrent. D1: the ratio double is typed, so no span
// is normalized (v1 normalized every one).
func testRelConcurrent(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize, c.Workers = 3, 4 })
	const writers, perTrace = 4, 7
	const start = int64(1_800_000_000_000_000_000)
	type traceWant struct {
		id   string
		hits []SpanHit
	}
	wants := make([]traceWant, writers)
	inputs := make([][]*tracepb.ResourceSpans, writers)
	for w := range writers {
		traceID := exRandomID(t, 16)
		wants[w].id = hex.EncodeToString(traceID)
		ss := &tracepb.ScopeSpans{}
		for i := range perTrace {
			spanID := exRandomID(t, 8)
			at := start + int64(w*100+i)
			ss.Spans = append(ss.Spans, &tracepb.Span{TraceId: traceID, SpanId: spanID, StartTimeUnixNano: uint64(at), Attributes: []*commonpb.KeyValue{
				exKV("gen_ai.operation.name", exStr("chat")),
				exKV("writer", exInt(int64(w))),
				exKV("ratio", exDouble(float64(i)/2)),
			}})
			wants[w].hits = append(wants[w].hits, SpanHit{hex.EncodeToString(spanID), at})
		}
		sortSpanHits(wants[w].hits)
		inputs[w] = []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{ss}}}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*writers)
	for w := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			stats, err := e.ExportResourceSpans(ctx, inputs[w])
			if err == nil && (stats.Spans != perTrace || stats.NormalizedSpans != 0) {
				err = fmt.Errorf("writer %d stats %+v", w, stats)
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var all, third []Hit
	for w, want := range wants {
		reTrace(t, ctx, e, want.id, want.hits...)
		last := want.hits[len(want.hits)-1].Start
		reHits(t, ctx, e, Attribute{"span", "writer", "int", fmt.Sprint(w)}, Hit{want.id, last})
		all = append(all, Hit{want.id, last})
		third = append(third, Hit{want.id, start + int64(w*100+3)}) // ratio 1.5 is span i=3
	}
	byLatest := func(h []Hit) { sort.Slice(h, func(i, j int) bool { return h[i].Latest > h[j].Latest }) }
	byLatest(all)
	byLatest(third)
	reHits(t, ctx, e, Attribute{"span", "gen_ai.operation.name", "string", "chat"}, all...)
	reHits(t, ctx, e, typedAttr(t, "span", "ratio", exDouble(1.5)), third...)
}

// §3.4, §4.5: a removed store and an unreadable index are ownership; the
// store's other indexes stay readable, and the reopen of a removed store is
// refused.
func testRelOwnership(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	rs := deltaSpan(1, exKV("gen_ai.operation.name", exStr("chat")), exKV("n", exInt(3)))
	sp := rs.ScopeSpans[0].Spans[0]
	tr, id := hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId)
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs}); err != nil {
		t.Fatal(err)
	}
	chat := Attribute{"span", "gen_ai.operation.name", "string", "chat"}
	inStore := func(fn func(*recordlayer.FDBRecordContext, *recordlayer.FDBRecordStore) error) {
		t.Helper()
		if _, err := e.store.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := e.store.ts.Builder(rc).Open()
			if err != nil {
				return nil, err
			}
			return nil, fn(rc, store)
		}); err != nil {
			t.Fatal(err)
		}
	}
	inStore(func(_ *recordlayer.FDBRecordContext, s *recordlayer.FDBRecordStore) error {
		_, err := s.MarkIndexDisabled(spansStrAttrIndex)
		return err
	})
	_, err := e.Query(ctx, chat)
	requireOtelKind(t, err, "ownership")
	_, err = e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{chat}})
	requireOtelKind(t, err, "ownership")
	reHits(t, ctx, e, Attribute{"span", "n", "int", "3"}, Hit{tr, int64(sp.StartTimeUnixNano)})
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(2)}); err != nil {
		t.Fatalf("a write beside a disabled index: %v", err)
	}

	inStore(func(rc *recordlayer.FDBRecordContext, _ *recordlayer.FDBRecordStore) error {
		return recordlayer.DeleteStore(rc, e.store.ts.Subspace)
	})
	_, err = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(3)})
	requireOtelKind(t, err, "ownership")
	_, err = e.Query(ctx, Attribute{"span", "n", "int", "3"})
	requireOtelKind(t, err, "ownership")
	_, err = e.LoadSpanOTLP(ctx, tr, id)
	requireOtelKind(t, err, "ownership")
	_, err = newExporter(ctx, e.Config(), relParts{})
	requireOtelKind(t, err, "ownership")
}

// Two handles of one store race to publish its continuation key: the first
// has saved its candidate and waits before committing while the second
// publishes; the first's retry returns the winner, so both handles' tokens
// resume on both.
func testRelHMACRace(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cfg := relTestConfig(t, cluster, nil)
	a := startRelExporter(t, ctx, cfg, relParts{})
	b := startRelExporter(t, ctx, cfg, relParts{})
	rss := qaCorpus(9)
	// Each handle commits the corpus (idempotently): a read's cached version
	// is only as new as its own client's last commit (tenant.go read).
	for _, e := range []*Exporter{a, b} {
		if _, err := e.ExportResourceSpans(ctx, rss); err != nil {
			t.Fatal(err)
		}
	}
	q := SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.request.model", "string", "gpt-4o"}}, Limit: 2}
	want := qrSameSpanOracle(qrFixture(rss), q.Predicates, nil)
	// a's page read is its first transaction, the key's publish its second.
	gate := &smHoldTransactor{Database: a.raw, held: make(chan struct{}), release: make(chan struct{}), skip: 1}
	a.store.db = recordlayer.NewFDBDatabaseWithTransactor(gate, a.raw)
	var pageA SpanQueryPage
	var errA error
	done := make(chan struct{})
	go func() {
		defer close(done)
		pageA, errA = a.QuerySameSpan(ctx, q)
	}()
	select {
	case <-gate.held:
	case <-ctx.Done():
		t.Fatal("a never reached its key publish")
	}
	pageB, errB := b.QuerySameSpan(ctx, q)
	close(gate.release)
	<-done
	if errA != nil || errB != nil || pageA.Continuation == nil || pageB.Continuation == nil {
		t.Fatalf("pages: %v %v", errA, errB)
	}
	if n := gate.executions.Load(); n != 2 {
		t.Fatalf("a's publish ran %d times; want a conflict and one retry", n)
	}
	for _, token := range [][]byte{pageA.Continuation, pageB.Continuation} {
		for name, e := range map[string]*Exporter{"a": a, "b": b} {
			page, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: q.Predicates, Limit: 2, Continuation: token})
			if err != nil || !reflect.DeepEqual(page.Spans, want[2:4]) {
				t.Fatalf("resume on %s: %+v %v", name, page.Spans, err)
			}
		}
	}
}

// The message flow through relMessageHooks: spans are admitted with
// placeholder manifests before persist, written with final manifests and
// (fulltext) ref_chunks, GenAI content attributes out of the indexes; a
// persist failure writes no span; LoadSpanOTLP reconstructs; Shutdown closes
// the hooks.
func testRelMessages(t *testing.T, cluster string) {
	for _, fulltext := range []bool{false, true} {
		t.Run(map[bool]string{false: "deltas", true: "fulltext"}[fulltext], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			hooks := &reMessages{}
			cfg := relTestConfig(t, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas, c.EnableMessageFullText = true, fulltext })
			e := startRelExporter(t, ctx, cfg, reMessageParts(hooks))
			const turns = 4
			input := deltaConversation(turns, "array")
			before := cloneDeltaSpans(input)
			stats, err := e.ExportResourceSpans(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Spans != turns || stats.MessageDeltas.TotalMessages != turns*(turns+1)+3*turns || stats.MessageDeltas.UniqueMessages != 2*turns+3 || stats.Messages.Written != 2*turns+3 {
				t.Fatalf("stats %+v", stats)
			}
			wantRefs := int64(0)
			if fulltext {
				wantRefs = turns*(turns+1) + 3*turns
			}
			if stats.Messages.References != wantRefs {
				t.Fatalf("references %d, want %d", stats.Messages.References, wantRefs)
			}
			for i, want := range before {
				sp := want.ScopeSpans[0].Spans[0]
				got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
				if err != nil {
					t.Fatalf("turn %d: %v", i+1, err)
				}
				requireProtoEqual(t, fmt.Sprintf("turn %d reconstructed", i+1), got, want)
			}
			// Rows: manifests in otlp, no GenAI content in the indexes, chunks with fulltext.
			sp := input[0].ScopeSpans[0].Spans[0]
			if err := e.store.read(ctx, "test", func(store *recordlayer.FDBRecordStore) error {
				pk, err := tableKey(store, "test", spansRecordType, sp.TraceId, sp.SpanId)
				if err != nil {
					return err
				}
				rec, err := store.LoadRecord(pk)
				if err != nil || rec == nil {
					return fmt.Errorf("row: %v %v", rec, err)
				}
				row := rec.Record.(*otelv1pb.SPANS)
				for _, a := range rowAttributes(row) {
					if IsMessageDeltaAttribute(a.Key) {
						return fmt.Errorf("indexed message attribute %+v", a)
					}
				}
				if (row.GetREF_CHUNKS() != nil) != fulltext {
					return fmt.Errorf("ref chunks %v with fulltext %v", row.GetREF_CHUNKS(), fulltext)
				}
				rs, err := rowOTLP("test", row.ProtoReflect(), sp.TraceId, sp.SpanId)
				if err != nil {
					return err
				}
				m, err := parseMessageManifest(exFind(rs.ScopeSpans[0].Spans[0].Attributes, "gen_ai.input.messages"))
				if err != nil || len(m.ids) != 1 || !m.ids[0].valid() || m.ids[0] == messagePlaceholderID {
					return fmt.Errorf("stored manifest %+v %v", m, err)
				}
				if fulltext {
					refs, err := otelsql.UnpackRefChunk(row.GetREF_CHUNKS().GetValues()[0].GetREFS())
					if err != nil || len(refs) == 0 || refs[0].Writer != 1 {
						return fmt.Errorf("stored refs %+v %v", refs, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err = e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}})
			requireOtelKind(t, err, "config")

			// A failed persist leaves the spans unwritten.
			hooks.persistErr = fail("transient", "message persist", "injected")
			fresh := deltaSpan(40, deltaKV("gen_ai.input.messages", deltaArray(deltaStr("unwritten"))))
			_, err = e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{fresh})
			requireOtelKind(t, err, "transient")
			reSpan(t, ctx, e, hex.EncodeToString(fresh.ScopeSpans[0].Spans[0].SpanId))
			hooks.persistErr = nil

			hooks.mu.Lock()
			calls := append([]string(nil), hooks.calls...)
			hooks.mu.Unlock()
			if calls[0] != "persist" || len(calls) != turns+2 {
				t.Fatalf("hook calls %v", calls)
			}
			if err := e.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if !hooks.closed {
				t.Fatal("shutdown did not close the message hooks")
			}
		})
	}
}

// Ports of testOtelAdmission and the index-key half of
// TestEncodedTraceCoveringAdmissionBeforeAnyWrites (§2.1): an attribute at the
// 1,024-byte key and 64,000-byte value caps is indexed, one byte over is
// stored but not indexed, and tuple escaping of zero bytes cannot push an
// index key past FDB's limit, so nothing is refused for it.
func testRelAdmissionCaps(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.BatchSize = 1 })
	key, value := strings.Repeat("\x00", maxIndexedKeyBytes), strings.Repeat("\x00", maxIndexedValueBytes)
	short := strings.Repeat("\x00", maxIndexedKeyBytes)
	for i, c := range []struct {
		key, value string
		indexed    bool
	}{
		{key, value, true},
		{key, short, true},
		{key + "x", short, false},
		{key, value + "x", false},
	} {
		rs := deltaSpan(i+1, exKV(c.key, exStr(c.value)))
		sp := rs.ScopeSpans[0].Spans[0]
		stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		wantUnindexed := int64(1)
		if c.indexed {
			wantUnindexed = 0
		}
		if stats.Spans != 1 || stats.UnindexedAttributes != wantUnindexed || (stats.NormalizedSpans == 1) == c.indexed {
			t.Fatalf("case %d stats %+v", i, stats)
		}
		var want []Hit
		if c.indexed {
			want = []Hit{{hex.EncodeToString(sp.TraceId), int64(sp.StartTimeUnixNano)}}
		}
		if len(c.key) <= maxIndexedKeyBytes {
			reHits(t, ctx, e, Attribute{"span", c.key, "string", c.value}, want...)
		}
		got, err := e.LoadSpanOTLP(ctx, hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId))
		if err != nil {
			t.Fatal(err)
		}
		requireProtoEqual(t, fmt.Sprintf("case %d stored", i), got, rs)
	}
	_, err := e.QuerySameSpan(ctx, SameSpanQuery{Predicates: []Attribute{{"span", key, "string", value + "x"}}})
	requireOtelKind(t, err, "config")
}

// Port of testOtelOrderAndBudgets (integration_test.go): the literal top-K
// tie boundary, a work receipt and its N-1/N candidate and byte budget edges
// for Query and Trace, N recomputed for the store's indexes. The K-th
// distinct trace stops the scan after six of eight entries, as v1's time
// order did.
func testRelQueryBudgetEdges(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.K = 3 })
	predicate := exKV("literal_order_probe", exStr("yes"))
	const timestamp = 1700000000000000000
	ids := [][2]int{{1, 1}, {2, 1}, {3, 1}, {4, 1}, {4, 4}, {4, 3}, {4, 2}, {5, 1}}
	starts := []uint64{timestamp, timestamp, timestamp, timestamp, timestamp + 2, timestamp + 1, timestamp + 1, timestamp}
	var specs []qaSpec
	for i, id := range ids {
		specs = append(specs, qaSpec{qaID(id[0]-1, 16), qaID(id[1]-1, 8), starts[i], []*commonpb.KeyValue{predicate}})
	}
	if _, err := e.ExportResourceSpans(ctx, qaSpans(t, specs...)); err != nil {
		t.Fatal(err)
	}
	a := Attribute{"span", "literal_order_probe", "string", "yes"}
	want := []Hit{{qaID(3, 16), timestamp + 2}, {qaID(4, 16), timestamp}, {qaID(2, 16), timestamp}}
	baseline := reHits(t, ctx, e, a, want...)
	if baseline.Candidates != 6 || baseline.Bytes <= 0 {
		t.Fatalf("the K-th trace must stop the scan at six of eight entries: %+v", baseline)
	}
	reader := func(mutate func(*Config)) *SpanReader {
		r := *e.reader
		mutate(&r.cfg)
		return &r
	}
	_, err := reader(func(c *Config) { c.MaxCandidates = baseline.Candidates - 1 }).Query(ctx, a)
	requireOtelBudget(t, err)
	if got, err := reader(func(c *Config) { c.MaxCandidates = baseline.Candidates }).Query(ctx, a); err != nil || !reflect.DeepEqual(got, baseline) {
		t.Fatalf("exact candidate budget: %+v %v", got, err)
	}
	_, err = reader(func(c *Config) { c.MaxBytes = baseline.Bytes - 1 }).Query(ctx, a)
	requireOtelBudget(t, err)
	if got, err := reader(func(c *Config) { c.MaxBytes = baseline.Bytes }).Query(ctx, a); err != nil || !reflect.DeepEqual(got, baseline) {
		t.Fatalf("exact byte budget: %+v %v", got, err)
	}

	trace := qaID(3, 16)
	wantSpans := []SpanHit{{qaID(0, 8), timestamp}, {qaID(1, 8), timestamp + 1}, {qaID(2, 8), timestamp + 1}, {qaID(3, 8), timestamp + 2}}
	reTrace(t, ctx, e, trace, wantSpans...)
	var sourceBytes int64
	if err := e.store.read(ctx, "test", func(store *recordlayer.FDBRecordStore) error {
		sourceBytes = 0
		for _, s := range wantSpans {
			pk, err := tableKey(store, "test", spansRecordType, qaBytes(t, trace), qaBytes(t, s.ID))
			if err != nil {
				return err
			}
			rec, err := store.LoadRecord(pk)
			if err != nil || rec == nil {
				return fmt.Errorf("trace source %s: %v %v", s.ID, rec, err)
			}
			sourceBytes += int64(rec.KeySize + rec.ValueSize)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, work, err := e.reader.Trace(ctx, trace); err != nil || work.Candidates != 4 || work.Bytes != sourceBytes || sourceBytes <= 0 {
		t.Fatalf("trace work %+v want four rows/%d bytes: %v", work, sourceBytes, err)
	}
	for _, c := range []struct {
		mutate func(*Config)
		ok     bool
	}{
		{func(c *Config) { c.MaxCandidates = 3 }, false},
		{func(c *Config) { c.MaxCandidates = 4 }, true},
		{func(c *Config) { c.MaxBytes = 1 }, false},
		{func(c *Config) { c.MaxBytes = sourceBytes - 1 }, false},
		{func(c *Config) { c.MaxBytes = sourceBytes }, true},
	} {
		got, _, err := reader(c.mutate).Trace(ctx, trace)
		if c.ok && (err != nil || !reflect.DeepEqual(got, wantSpans)) {
			t.Fatalf("exact trace budget: %+v %v", got, err)
		}
		if !c.ok {
			requireOtelBudget(t, err)
		}
	}
}

// The actual-size guard (§1.2: 500 KB by GetApproximateSize) fails a batch
// as budget, committing nothing.
func testRelTransactionGuard(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	rs := deltaSpan(1)
	e.writer.maxTxnBytes = 1
	_, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs})
	requireOtelKind(t, err, "budget")
	sp := rs.ScopeSpans[0].Spans[0]
	reSpan(t, ctx, e, hex.EncodeToString(sp.SpanId))
	e.writer.maxTxnBytes = maxSpanTxnBytes
	if stats, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{rs}); err != nil || stats.Spans != 1 {
		t.Fatalf("under the guard: %+v %v", stats, err)
	}
}
