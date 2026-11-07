package otel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
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

func qaBackend(order string) *FDBBackend {
	return &FDBBackend{cfg: Config{Run: "qa_run_0001", Tenant: 3, Order: order, K: 100}}
}

// qaCanonical is the indexed attribute set of one stored single-span ResourceSpans.
func qaCanonical(rs *tracepb.ResourceSpans) map[Attribute]bool {
	out := map[Attribute]bool{}
	add := func(scope string, kvs []*commonpb.KeyValue) {
		for _, kv := range canonicalAttributes(kvs).indexed {
			out[CanonicalAttribute(scope, kv.Key, kv.Value)] = true
		}
	}
	add("resource", rs.GetResource().GetAttributes())
	add("scope", rs.ScopeSpans[0].GetScope().GetAttributes())
	add("span", rs.ScopeSpans[0].Spans[0].Attributes)
	return out
}

type qaFixtureSpan struct {
	match SpanMatch
	attrs map[Attribute]bool
}

func qaFixture(rss []*tracepb.ResourceSpans) []qaFixtureSpan {
	var out []qaFixtureSpan
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				single := &tracepb.ResourceSpans{Resource: rs.Resource, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, Spans: []*tracepb.Span{sp}}}}
				out = append(out, qaFixtureSpan{SpanMatch{hex.EncodeToString(sp.TraceId), hex.EncodeToString(sp.SpanId), int64(sp.StartTimeUnixNano)}, qaCanonical(single)})
			}
		}
	}
	return out
}

func qaSortSpans(order string, xs []SpanMatch) {
	sort.Slice(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if order == "time" {
			if a.Start != b.Start {
				return a.Start > b.Start
			}
			if a.Trace != b.Trace {
				return a.Trace > b.Trace // reverseTrace ascending
			}
			return a.ID < b.ID
		}
		if a.Trace != b.Trace {
			return a.Trace < b.Trace
		}
		return a.ID < b.ID
	})
}

func qaSameSpanOracle(spans []qaFixtureSpan, preds []Attribute, w *TimeRange, order string) []SpanMatch {
	out := []SpanMatch{}
	for _, s := range spans {
		ok := w == nil || (s.match.Start >= w.Start && s.match.Start < w.End)
		for _, p := range preds {
			ok = ok && s.attrs[p]
		}
		if ok {
			out = append(out, s.match)
		}
	}
	qaSortSpans(order, out)
	return out
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

func qaExport(t *testing.T, ctx context.Context, e *Exporter, rss []*tracepb.ResourceSpans) {
	t.Helper()
	want := int64(0)
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			want += int64(len(ss.Spans))
		}
	}
	stats, err := e.ExportResourceSpans(ctx, rss)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Spans != want {
		t.Fatalf("exported %d spans, want %d", stats.Spans, want)
	}
}

// qaPaginate drains a same-span query and checks the page contract.
func qaPaginate(t *testing.T, ctx context.Context, e *Exporter, q SameSpanQuery) ([]SpanMatch, int) {
	t.Helper()
	limit := q.Limit
	if limit == 0 {
		limit = e.Config().K
	}
	all := []SpanMatch{}
	for pages := 1; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination did not terminate")
		}
		page, err := e.QuerySameSpan(ctx, q)
		if err != nil {
			t.Fatalf("page %d of %+v: %v", pages, q.Predicates, err)
		}
		if page.Order != e.Config().Order || page.ReadVersion <= 0 || page.Spans == nil {
			t.Fatalf("page metadata: %+v", page)
		}
		if len(page.Spans) > limit || (len(page.Spans) == limit) != (page.Continuation != nil) {
			t.Fatalf("page %d: %d spans, limit %d, continuation=%v", pages, len(page.Spans), limit, page.Continuation != nil)
		}
		all = append(all, page.Spans...)
		if page.Continuation == nil {
			return all, pages
		}
		q.Continuation = page.Continuation
	}
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
