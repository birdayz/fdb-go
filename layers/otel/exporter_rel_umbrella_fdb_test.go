package otel

import (
	"context"
	"reflect"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Ports of the umbrella subtests (integration_test.go) that §2.1 keeps as
// Exporter tests. The umbrella ran each twice, for order trace and time; D2
// removes the order key, and the relational reader always stops at the K-th
// distinct trace (§1.3), so each runs once, as order time. D2 also removes
// hash_bytes: the indexes hold attribute values, not digests.

// relFixtureSpan is s's single-span envelope, as every backend stores it.
func relFixtureSpan(t *testing.T, s Span) *tracepb.ResourceSpans {
	t.Helper()
	rs, _, err := FixtureOTLP(s)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// relExportCorpus exports the spans Generate assigns to c.Tenant through e,
// whose tenant store holds that half alone, and returns them.
func relExportCorpus(t *testing.T, ctx context.Context, e *relExporter, c Config) []Span {
	t.Helper()
	var spans []Span
	var rss []*tracepb.ResourceSpans
	for i := 0; i < c.Count; i++ {
		if s := Generate(c, i); s.Tenant == c.Tenant {
			spans = append(spans, s)
			rss = append(rss, relFixtureSpan(t, s))
		}
	}
	if stats, err := e.ExportResourceSpans(ctx, rss); err != nil || stats.Spans != int64(len(rss)) || len(rss) == 0 {
		t.Fatalf("export corpus of %d spans: %+v %v", len(rss), stats, err)
	}
	return spans
}

// Port of testOtelScopedQueries (integration_test.go:1115) on the umbrella
// corpus (Count 260, K 3). D1: equality is typed, so the int-typed spellings
// are answered by SPANS_INT_ATTR and the string-typed ones never match them.
func testRelTypedScopedControls(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := testConfig()
	c.Count, c.K = 260, 3
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K = c.K })
	relExportCorpus(t, ctx, e, c)
	for _, a := range []Attribute{
		{"span", QueryKey, "string", QueryValue},
		{"span", QueryKey, "int", QueryValue},
		{"span", QueryKey + ".int", "int", QueryValue},
		{"resource", QueryKey, "string", QueryValue},
		{"scope", QueryKey, "string", QueryValue},
	} {
		want := Expected(c, a.Scope, a.Key, a.Type, a.Value)
		if len(want) == 0 {
			t.Fatalf("vacuous positive control: %+v", a)
		}
		got, err := e.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, want) {
			t.Fatalf("typed/scoped positive %+v: %+v want %+v: %v", a, got, want, err)
		}
	}
	for _, a := range []Attribute{
		{"span", QueryKey + ".int", "string", QueryValue},
		{"resource", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "string", "definitely_absent"},
	} {
		got, err := e.Query(ctx, a)
		if err != nil || len(got.Hits) != 0 {
			t.Fatalf("typed/scoped negative %+v: %+v %v", a, got, err)
		}
	}
}

// Port of testOtelSelectiveV2 (selectivity_test.go:148). Its trace-order
// branch, which exhausted the two-candidate budget on a common term, has no
// counterpart (D2): every common term returns the K=1 frontier after one
// index entry.
func testRelSelectiveV2(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.MatchEvery = 64, 1, 8, 1
	c.MaxCandidates, c.K, c.Iterations = 2, 1, 2
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K, x.MaxCandidates = c.K, c.MaxCandidates })
	maxBytes := e.Config().MaxBytes
	relExportCorpus(t, ctx, e, c)
	selective, err := queryCases(ctx, c, "selective")
	if err != nil {
		t.Fatal(err)
	}
	q := selective[1]
	source := Generate(c, q.sourceOrdinal)
	if q.kind != "attribute" || q.class != "custom_varying" || q.attribute != (Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}) {
		t.Fatalf("real selective predicate is not the unique source response ID: %+v", q)
	}
	got, err := e.Query(ctx, q.attribute)
	want := []Hit{{Trace: source.Trace, Latest: source.Start}}
	if err != nil || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > maxBytes {
		t.Fatalf("unique response query did not fit bounded work: got=%+v want=%+v error=%v", got, want, err)
	}
	mixed, err := queryCases(ctx, c, "mixed")
	if err != nil {
		t.Fatal(err)
	}
	genaiConfig := c
	genaiConfig.Iterations = 7
	genai, err := queryCases(ctx, genaiConfig, "genai")
	if err != nil {
		t.Fatal(err)
	}
	for _, common := range []queryCase{mixed[1], genai[6]} {
		if population := selectivitySourcePopulation(c, []Attribute{common.attribute}); population[common.attribute] != c.Count {
			t.Fatalf("%s common source population=%d want %d", common.class, population[common.attribute], c.Count)
		}
		got, err := e.Query(ctx, common.attribute)
		want := bruteAttributeOracle(c, common.attribute)
		if err != nil || len(want) != 1 || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > maxBytes {
			t.Fatalf("%s must return the sound K=1 frontier: got=%+v want=%+v error=%v", common.class, got, want, err)
		}
	}
}
