package sqlbackend

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otel.SpanReader over a tenant store a Record Layer writer fills: every read
// sees the writes this database handle committed before it (its read version
// may come from the GRV cache, which the handle's commits refresh), typed
// attribute equality, long strings verified against the record, and the
// budgets.
func testSpanReader(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const path = "/" + domain + "/otel_span_reader" // Open registered the domain
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
	ts, err := otelsql.OpenTenantStoreCompiled(ctx, rl, path, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := otel.DefaultConfig()
	cfg.K, cfg.MaxCandidates, cfg.MaxBytes = 2, 100, 1<<20
	reader := otel.NewSpanReader(rl, ts, cfg)

	long := strings.Repeat("y", otelsql.MaxIndexedStringBytes+1)
	resource := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strKV("service.name", "agent")}}
	scope := &commonpb.InstrumentationScope{Name: "lib"}
	save := func(trace, span byte, start uint64, attrs ...*commonpb.KeyValue) {
		t.Helper()
		sp := &tracepb.Span{
			TraceId: append(make([]byte, 15), trace), SpanId: append(make([]byte, 7), span), Name: "op",
			StartTimeUnixNano: start, EndTimeUnixNano: start + 1, Attributes: attrs,
		}
		wire, err := proto.Marshal(&tracepb.ResourceSpans{Resource: resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{sp}}}})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := otelsql.BuildCompiled(otelsql.Span{Resource: resource, Scope: scope, Span: sp, OTLP: wire})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rl.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := ts.Builder(rc).Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(rec)
		}); err != nil {
			t.Fatal(err)
		}
	}
	traceHex := func(b byte) string { return hex.EncodeToString(append(make([]byte, 15), b)) }
	spanHex := func(b byte) string { return hex.EncodeToString(append(make([]byte, 7), b)) }
	hits := func(r otel.QueryResult) string {
		var s []string
		for _, h := range r.Hits {
			s = append(s, fmt.Sprintf("%s@%d", h.Trace[30:], h.Latest))
		}
		return strings.Join(s, ",")
	}
	ratio := &commonpb.KeyValue{Key: "ratio", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}}}
	ok := &commonpb.KeyValue{Key: "ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}
	model := otel.Attribute{Scope: "span", Key: "model", Type: "string", Value: "m"}

	// Each read right after its write: the cached read version must not hide it.
	for i := byte(1); i <= 3; i++ {
		save(i, i, uint64(i)*100, strKV("model", "m"), intKV("n", int64(i)), strKV("prompt", long), ratio, ok)
		r, err := reader.Query(ctx, model)
		if err != nil {
			t.Fatal(err)
		}
		if r.Hits[0].Trace != traceHex(i) || r.Hits[0].Latest != int64(i)*100 {
			t.Fatalf("after writing trace %d, Query = %s", i, hits(r))
		}
		spans, _, err := reader.Trace(ctx, traceHex(i))
		if err != nil || len(spans) != 1 || spans[0].ID != spanHex(i) {
			t.Fatalf("after writing trace %d, Trace = %v, %v", i, spans, err)
		}
		matches, _, err := reader.Span(ctx, spanHex(i))
		if err != nil || len(matches) != 1 || matches[0].Trace != traceHex(i) {
			t.Fatalf("after writing span %d, Span = %v, %v", i, matches, err)
		}
	}
	// A second span of trace 1, newest of all: trace 1 is the top hit once.
	save(1, 9, 1000, strKV("model", "m"))

	for _, c := range []struct {
		a    otel.Attribute
		want string
	}{
		{model, "01@1000,03@300"},
		{otel.Attribute{Scope: "span", Key: "n", Type: "int", Value: "2"}, "02@200"},
		{otel.Attribute{Scope: "span", Key: "n", Type: "int", Value: "two"}, ""},
		{otel.Attribute{Scope: "span", Key: "ratio", Type: "double", Value: "0.5"}, "03@300,02@200"},
		{otel.Attribute{Scope: "span", Key: "ok", Type: "bool", Value: "true"}, "03@300,02@200"},
		{otel.Attribute{Scope: "span", Key: "ok", Type: "string", Value: "true"}, ""},
		{otel.Attribute{Scope: "span", Key: "prompt", Type: "string", Value: long}, "03@300,02@200"},
		{otel.Attribute{Scope: "span", Key: "prompt", Type: "string", Value: long + "z"}, ""},
		{otel.Attribute{Scope: "resource", Key: "service.name", Type: "string", Value: "agent"}, "01@1000,03@300"},
	} {
		r, err := reader.Query(ctx, c.a)
		if err != nil {
			t.Fatalf("Query(%s %s=%.12s): %v", c.a.Type, c.a.Key, c.a.Value, err)
		}
		if got := hits(r); got != c.want {
			t.Errorf("Query(%s %s=%.12s) = %q, want %q", c.a.Type, c.a.Key, c.a.Value, got, c.want)
		}
	}
	r, err := reader.Time(ctx, otel.TimeRange{Start: 200, End: 1000})
	if err != nil || hits(r) != "03@300,02@200" {
		t.Fatalf("Time [200,1000) = %s, %v", hits(r), err)
	}
	if r, err := reader.Time(ctx, otel.TimeRange{Start: 5, End: 5}); err != nil || len(r.Hits) != 0 || r.Candidates != 0 {
		t.Fatalf("empty Time range = %+v, %v", r, err)
	}
	if _, err := reader.Time(ctx, otel.TimeRange{Start: 6, End: 5}); err == nil {
		t.Fatal("inverted Time range accepted")
	}
	spans, _, err := reader.Trace(ctx, traceHex(1))
	if err != nil || len(spans) != 2 || spans[0].Start != 100 || spans[1].Start != 1000 {
		t.Fatalf("Trace(1) = %v, %v", spans, err)
	}
	// Stored identities are lowercase hex; other spellings name no span.
	save(0xab, 0xcd, 50)
	if spans, _, err := reader.Trace(ctx, traceHex(0xab)); err != nil || len(spans) != 1 {
		t.Fatalf("Trace(ab) = %v, %v", spans, err)
	}
	if matches, _, err := reader.Span(ctx, strings.ToUpper(spanHex(0xcd))); err != nil || len(matches) != 0 {
		t.Fatalf("Span(CD) = %v, %v", matches, err)
	}
	for _, id := range []string{strings.ToUpper(traceHex(0xab)), "01", "zz" + traceHex(1)[2:]} {
		if spans, _, err := reader.Trace(ctx, id); err != nil || len(spans) != 0 {
			t.Fatalf("Trace(%q) = %v, %v", id, spans, err)
		}
	}

	// Budgets: one candidate past the limit fails the read, never truncates it.
	tight := cfg
	tight.MaxCandidates = 2
	budget := otel.NewSpanReader(rl, ts, tight)
	var be *otel.QueryBudgetError
	if _, err := budget.Query(ctx, otel.Attribute{Scope: "span", Key: "model", Type: "string", Value: "m"}); err != nil {
		t.Fatalf("a top-2 query stopping at its second trace failed: %v", err)
	}
	if _, _, err := budget.Trace(ctx, traceHex(1)); err != nil {
		t.Fatalf("a two-span trace within two candidates failed: %v", err)
	}
	tight.MaxCandidates = 1
	budget = otel.NewSpanReader(rl, ts, tight)
	if _, _, err := budget.Trace(ctx, traceHex(1)); !errors.As(err, &be) {
		t.Fatalf("a two-span trace over one candidate: %v", err)
	}
	tight.MaxCandidates, tight.MaxBytes = 100, 10
	budget = otel.NewSpanReader(rl, ts, tight)
	if _, err := budget.Time(ctx, otel.TimeRange{Start: 0, End: 2000}); !errors.As(err, &be) {
		t.Fatalf("a time scan over 10 bytes: %v", err)
	}

	// An unreadable index and a deleted store are ownership failures.
	var oe *otel.OwnershipError
	inStore := func(fn func(*recordlayer.FDBRecordContext, *recordlayer.FDBRecordStore) error) {
		t.Helper()
		if _, err := rl.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := ts.Builder(rc).Open()
			if err != nil {
				return nil, err
			}
			return nil, fn(rc, store)
		}); err != nil {
			t.Fatal(err)
		}
	}
	inStore(func(_ *recordlayer.FDBRecordContext, s *recordlayer.FDBRecordStore) error {
		_, err := s.MarkIndexDisabled("SPANS_TIME")
		return err
	})
	if _, err := reader.Time(ctx, otel.TimeRange{Start: 0, End: 2000}); !errors.As(err, &oe) {
		t.Fatalf("Time over a disabled SPANS_TIME: %v", err)
	}
	if r, err := reader.Query(ctx, model); err != nil || len(r.Hits) != 2 {
		t.Fatalf("Query beside a disabled SPANS_TIME = %+v, %v", r, err)
	}
	inStore(func(rc *recordlayer.FDBRecordContext, _ *recordlayer.FDBRecordStore) error {
		return recordlayer.DeleteStore(rc, ts.Subspace)
	})
	if _, err := reader.Query(ctx, model); !errors.As(err, &oe) {
		t.Fatalf("Query of a deleted store: %v", err)
	}
}
