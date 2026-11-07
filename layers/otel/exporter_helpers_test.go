package otel

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// OTLP value builders
// ---------------------------------------------------------------------------

func exStr(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func exInt(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

func exBool(b bool) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
}

func exDouble(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}

func exBytes(b []byte) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: b}}
}

func exArray(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
}

func exKvlist(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func exKV(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

// exRandomID returns n random bytes with the top bit forced so it is nonzero.
func exRandomID(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[0] |= 0x80
	return b
}

func exFind(kvs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

// exSingle is the single-span ResourceSpans an exporter stores for sp.
func exSingle(rs *tracepb.ResourceSpans, ss *tracepb.ScopeSpans, sp *tracepb.Span) *tracepb.ResourceSpans {
	out := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
	out.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
	out.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
	return out
}

func requireProtoEqual(t *testing.T, label string, got, want proto.Message) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Fatalf("%s mismatch\n got: %v\nwant: %v", label, got, want)
	}
}

func exSpanContext(t *testing.T, sampled, remote bool, state string) trace.SpanContext {
	t.Helper()
	var tid trace.TraceID
	var sid trace.SpanID
	copy(tid[:], exRandomID(t, 16))
	copy(sid[:], exRandomID(t, 8))
	cfg := trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: remote}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	if state != "" {
		ts, err := trace.ParseTraceState(state)
		if err != nil {
			t.Fatal(err)
		}
		cfg.TraceState = ts
	}
	return trace.NewSpanContext(cfg)
}

// ---------------------------------------------------------------------------
// Real FDB integration
// ---------------------------------------------------------------------------

func startExporterCluster(t *testing.T) string {
	t.Helper()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func openTestExporter(t *testing.T, ctx context.Context, cluster string, mutate func(*ExporterConfig)) *Exporter {
	t.Helper()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile = cluster
	cfg.Run = freshRun(t)
	cfg.Tenant = 11
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := NewExporter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e.Config() != cfg {
		t.Fatalf("Config() = %+v, want %+v", e.Config(), cfg)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return e
}

func requireHits(t *testing.T, ctx context.Context, e *Exporter, a Attribute, want ...Hit) QueryResult {
	t.Helper()
	got, err := e.Query(ctx, a)
	if err != nil {
		t.Fatalf("query %+v: %v", a, err)
	}
	if got.Hits == nil {
		t.Fatalf("query %+v returned nil hits", a)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("query %+v\n got %+v\nwant %+v", a, got.Hits, want)
	}
	return got
}

func requireTrace(t *testing.T, ctx context.Context, e *Exporter, traceID string, want ...SpanHit) {
	t.Helper()
	got, err := e.Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("trace %s: %v", traceID, err)
	}
	if len(want) == 0 {
		want = []SpanHit{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %s\n got %+v\nwant %+v", traceID, got, want)
	}
}

func requireSpan(t *testing.T, ctx context.Context, e *Exporter, spanID string, want ...SpanMatch) {
	t.Helper()
	got, err := e.Span(ctx, spanID)
	if err != nil {
		t.Fatalf("span %s: %v", spanID, err)
	}
	if len(want) == 0 {
		want = []SpanMatch{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("span %s\n got %+v\nwant %+v", spanID, got, want)
	}
}

func requireTime(t *testing.T, ctx context.Context, e *Exporter, r TimeRange, want ...Hit) {
	t.Helper()
	got, err := e.Time(ctx, r)
	if err != nil {
		t.Fatalf("time %+v: %v", r, err)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("time %+v\n got %+v\nwant %+v", r, got.Hits, want)
	}
}

// requireRecorded checks LoadSpanOTLP against the SDK's own view of every span.
func requireRecorded(t *testing.T, ctx context.Context, e *Exporter, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, ro := range spans {
		want := ResourceSpansFromReadOnly([]sdktrace.ReadOnlySpan{ro})
		sc := ro.SpanContext()
		got, err := e.LoadSpanOTLP(ctx, sc.TraceID().String(), sc.SpanID().String())
		if err != nil {
			t.Fatalf("load %s/%s (%s): %v", sc.TraceID(), sc.SpanID(), ro.Name(), err)
		}
		requireProtoEqual(t, "LoadSpanOTLP "+ro.Name(), got, want[0])
	}
}

func sortedSpanHits(hits ...SpanHit) []SpanHit {
	sortSpanHits(hits)
	return hits
}
