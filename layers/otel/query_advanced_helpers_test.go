package otel

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// qaID is a deterministic nonzero lowercase hex identity of size bytes.
func qaID(n, size int) string { return fmt.Sprintf("%0*x", size*2, n+1) }

func qaBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireConfigOrCorruption(t *testing.T, label string, err error) {
	t.Helper()
	var cfg *ConfigError
	var corrupt *CorruptionError
	if !errors.As(err, &cfg) && !errors.As(err, &corrupt) {
		t.Fatalf("%s: want ConfigError or CorruptionError, got %T: %v", label, err, err)
	}
}

func requireTyped[T error](t *testing.T, label string, err error) {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("%s: want %T, got %T: %v", label, target, err, err)
	}
}

type qaFixtureSpan struct {
	match SpanMatch
	attrs map[Attribute]bool
}

func qaGraphSpan(t *testing.T, trace string, id, parent string, start int64, links ...*tracepb.Span_Link) graphSpan {
	sp := &tracepb.Span{TraceId: qaBytes(t, trace), SpanId: qaBytes(t, id), StartTimeUnixNano: uint64(start), Links: links}
	if parent != "" {
		sp.ParentSpanId = qaBytes(t, parent)
	}
	return graphSpan{SpanMatch{trace, id, start}, sp}
}

const qaBase = uint64(1_700_000_000_000_000_000)

// qaCorpus is a seeded corpus with many start-time ties and digest collisions.
func qaCorpus(seed uint64) []*tracepb.ResourceSpans {
	rng := rand.New(rand.NewPCG(seed, 283))
	id := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		b[0] |= 1
		return b
	}
	models := []string{"gpt-4o", "claude-3", collision("gpt-4o")}
	collidingKey := collision("gen_ai.request.model")
	var out []*tracepb.ResourceSpans
	for tr := range 16 {
		traceID := id(16)
		service := []string{"svc-a", "svc-b"}[tr%2]
		rs := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr(service))}}, ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "qa"}}}}
		for range 6 {
			attrs := []*commonpb.KeyValue{exKV("gen_ai.request.model", exStr(models[rng.IntN(len(models))]))}
			if rng.IntN(3) == 0 {
				attrs = append(attrs, exKV(collidingKey, exStr("gpt-4o")))
			}
			switch rng.IntN(3) {
			case 0:
				attrs = append(attrs, exKV("error.type", exStr("timeout")))
			case 1:
				attrs = append(attrs, exKV("error.type", exStr("rate_limit")))
			}
			attrs = append(attrs, exKV("feature.flag", exInt(int64(rng.IntN(3)+1))), exKV("cached", exBool(rng.IntN(2) == 0)))
			sp := &tracepb.Span{TraceId: traceID, SpanId: id(8), Name: "op", StartTimeUnixNano: qaBase + uint64(rng.IntN(40))*1000, Attributes: attrs}
			rs.ScopeSpans[0].Spans = append(rs.ScopeSpans[0].Spans, sp)
		}
		out = append(out, rs)
	}
	return out
}

type qaSpec struct {
	trace, span string
	start       uint64
	attrs       []*commonpb.KeyValue
}

func qaSpans(t *testing.T, specs ...qaSpec) []*tracepb.ResourceSpans {
	t.Helper()
	var out []*tracepb.ResourceSpans
	for _, s := range specs {
		sp := &tracepb.Span{TraceId: qaBytes(t, s.trace), SpanId: qaBytes(t, s.span), Name: "op", StartTimeUnixNano: s.start, Attributes: s.attrs}
		out = append(out, &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}})
	}
	return out
}
