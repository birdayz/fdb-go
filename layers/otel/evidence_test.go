package otel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// evidenceBackend returns source-derived answers, never the measurement's cases.
// Embedding Backend makes an unexpected lifecycle call fail immediately.
type evidenceBackend struct {
	Backend
	cfg       Config
	mode      string
	traceMode string
	ctx       context.Context
	cancel    context.CancelFunc
	calls     []evidenceCall
	lastWork  QueryResult
}

type evidenceCall struct {
	kind      string
	attribute Attribute
	trace     string
	span      string
	interval  TimeRange
	work      QueryResult
	hits      []Hit
	spans     []SpanHit
	matches   []SpanMatch
	err       error
}

func evidenceConfig() Config {
	c := DefaultConfig()
	c.Run = "evidence_fixture_283"
	c.Count, c.Tenants, c.Attributes = 48, 2, 8
	c.MatchEvery, c.K, c.Iterations = 3, 3, 20
	return c
}

func evidenceHits(c Config, a Attribute) []Hit {
	latest := map[string]int64{}
	for i := 0; i < c.Count; i++ {
		s := Generate(c, i)
		if s.Tenant != c.Tenant {
			continue
		}
		for _, candidate := range s.Attributes {
			if candidate == a {
				if ns, ok := latest[s.Trace]; !ok || s.Start > ns {
					latest[s.Trace] = s.Start
				}
			}
		}
	}
	out := []Hit{}
	for id, ns := range latest {
		out = append(out, Hit{id, ns})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Latest == out[j].Latest {
			return out[i].Trace > out[j].Trace
		}
		return out[i].Latest > out[j].Latest
	})
	if len(out) > c.K {
		out = out[:c.K]
	}
	return out
}

func evidenceSpans(c Config, trace string) []SpanHit {
	out := []SpanHit{}
	for i := 0; i < c.Count; i++ {
		s := Generate(c, i)
		if s.Tenant == c.Tenant && s.Trace == trace {
			out = append(out, SpanHit{s.ID, s.Start})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start == out[j].Start {
			return out[i].ID < out[j].ID
		}
		return out[i].Start < out[j].Start
	})
	return out
}

func (b *evidenceBackend) response(ctx context.Context) (evidenceCall, string) {
	if ctx != b.ctx {
		panic("measurement did not propagate context")
	}
	n := len(b.calls)
	mode := b.mode
	if mode == "mixed" {
		mode = []string{"success", "identity", "order", "transport"}[n%4]
	}
	call := evidenceCall{work: QueryResult{
		Candidates: 31 + n, Bytes: int64(101 + n),
		ServerReadRows: int64(201 + n), ServerReadBytes: int64(301 + n),
		ServerElapsed: int64(401 + n), ResponseBytes: int64(501 + n),
		ServerSummary: true,
		QueryID:       fmt.Sprintf("fake-query-%03d", n), QueryTransport: "http", ServerMetricsSource: "http_summary",
	}}
	switch n % 3 {
	case 1:
		call.work.QueryTransport = "native"
		call.work.ResponseBytes = 0
		fallthrough
	case 2:
		call.work.ServerReadRows, call.work.ServerReadBytes, call.work.ServerElapsed = 0, 0, 0
		call.work.ServerSummary, call.work.ServerMetricsSource = false, "unavailable"
	}
	if mode == "transport" {
		call.err = errors.New("injected transport failure")
	}
	if mode == "ownership_after_success" && n > 0 {
		mode = "ownership"
		call.work = QueryResult{}
		call.err = fail("ownership", "fake query", "run has no ownership manifest")
	}
	if mode == "cancel" || mode == "cancel_ignores_context" || (mode == "cancel_after_success" && n > 0) {
		b.cancel()
		if mode != "cancel_ignores_context" {
			call.err = ctx.Err()
		}
	}
	return call, mode
}

func (b *evidenceBackend) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	call, mode := b.response(ctx)
	call.kind, call.attribute = "attribute", a
	call.hits = evidenceHits(b.cfg, a)
	switch mode {
	case "ownership":
		call.hits = []Hit{}
	case "identity":
		if len(call.hits) > 0 {
			call.hits[0].Trace = "wrong-trace"
		} else {
			call.hits = append(call.hits, Hit{"wrong-trace", -1})
		}
	case "timestamp":
		if len(call.hits) > 0 {
			call.hits[0].Latest++
		}
	case "order":
		if len(call.hits) > 1 {
			call.hits[0], call.hits[1] = call.hits[1], call.hits[0]
		}
	}
	call.work.Hits = call.hits
	b.lastWork = call.work
	b.calls = append(b.calls, call)
	return call.work, call.err
}

func (b *evidenceBackend) Trace(ctx context.Context, trace string) ([]SpanHit, error) {
	if b.traceMode != "" {
		b.mode = b.traceMode
	}
	call, mode := b.response(ctx)
	call.kind, call.trace = "trace", trace
	call.spans = evidenceSpans(b.cfg, trace)
	switch mode {
	case "ownership":
		call.spans = []SpanHit{}
	case "identity":
		if len(call.spans) > 0 {
			call.spans[0].ID = "wrong-span"
		} else {
			call.spans = append(call.spans, SpanHit{ID: "wrong-span", Start: -1})
		}
	case "timestamp":
		if len(call.spans) > 0 {
			call.spans[0].Start++
		}
	case "order":
		if len(call.spans) > 1 {
			call.spans[0], call.spans[1] = call.spans[1], call.spans[0]
		}
	}
	b.lastWork = call.work
	b.calls = append(b.calls, call)
	return call.spans, call.err
}

func (b *evidenceBackend) Span(ctx context.Context, id string) ([]SpanMatch, error) {
	call, mode := b.response(ctx)
	call.kind, call.span = "span", id
	call.matches = bruteSpanOracle(b.cfg, id)
	if mode == "ownership" {
		call.matches = []SpanMatch{}
	} else if mode == "identity" {
		if len(call.matches) > 0 {
			call.matches[0].ID = "wrong-span"
		} else {
			call.matches = append(call.matches, SpanMatch{Trace: "wrong-trace", ID: "wrong-span", Start: -1})
		}
	} else if mode == "timestamp" && len(call.matches) > 0 {
		call.matches[0].Start++
	}
	b.lastWork = call.work
	b.calls = append(b.calls, call)
	return call.matches, call.err
}

func (b *evidenceBackend) Time(ctx context.Context, interval TimeRange) (QueryResult, error) {
	call, mode := b.response(ctx)
	call.kind, call.interval = "time", interval
	call.hits = bruteTimeOracle(b.cfg, interval)
	switch mode {
	case "ownership":
		call.hits = []Hit{}
	case "identity":
		if len(call.hits) > 0 {
			call.hits[0].Trace = "wrong-trace"
		} else {
			call.hits = append(call.hits, Hit{"wrong-trace", -1})
		}
	case "timestamp":
		if len(call.hits) > 0 {
			call.hits[0].Latest++
		}
	case "order":
		if len(call.hits) > 1 {
			call.hits[0], call.hits[1] = call.hits[1], call.hits[0]
		}
	}
	call.work.Hits = call.hits
	b.lastWork = call.work
	b.calls = append(b.calls, call)
	return call.work, call.err
}

func (b *evidenceBackend) LastQueryWork() QueryResult { return b.lastWork }

func evidenceDigest(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func evidenceClass(suite string, i int) string {
	span := []string{"span", "span_absent"}
	genai := []string{"genai_response", "genai_scope_resource", "genai_scope_scope", "genai_type", "genai_absent", "genai_operation", "genai_provider", "genai_model"}
	time := []string{"time_narrow", "time_empty", "time_absent", "time_broad"}
	switch suite {
	case "span":
		return span[i%len(span)]
	case "genai":
		return genai[i%len(genai)]
	case "time":
		return time[i%len(time)]
	case "all":
		classes := append([]string{"trace", "trace_absent"}, span...)
		classes = append(classes, genai...)
		classes = append(classes, time...)
		return classes[i%len(classes)]
	}
	if suite == "rare" || suite == "trace" {
		return suite
	}
	class := []string{"rare", "custom_varying", "lookalike_type", "absent", "id_varying"}[i%5]
	if suite == "mixed" && i%5 == 1 {
		return "resource_common"
	}
	return class
}

func TestMeasureQueriesReceipts(t *testing.T) {
	t.Parallel()
	for _, suite := range []string{"selective", "mixed", "rare", "trace", "span", "genai", "time", "all"} {
		for _, mode := range []string{"success", "identity", "timestamp", "order", "transport", "mixed"} {
			t.Run(suite+"/"+mode, func(t *testing.T) {
				t.Parallel()
				c := evidenceConfig()
				ctx := context.Background()
				b := &evidenceBackend{cfg: c, mode: mode, ctx: ctx}
				e := NewEvidence(c, "fake", "query", suite, "revision")
				err := MeasureQueries(ctx, b, c, suite, &e)
				if len(b.calls) != c.Iterations || len(e.Samples) != c.Iterations {
					t.Fatalf("calls=%d samples=%d want %d", len(b.calls), len(e.Samples), c.Iterations)
				}
				failures := 0
				queryIDs := map[string]bool{}
				populations := map[string][]int64{}
				for i, call := range b.calls {
					actual := e.Samples[i]
					if actual.QueryID == "" || queryIDs[actual.QueryID] {
						t.Fatalf("sample %d missing or reused data-query identity: %+v", i, actual)
					}
					queryIDs[actual.QueryID] = true
					class := evidenceClass(suite, i)
					kind := "attribute"
					for _, candidate := range []string{"trace", "span", "time"} {
						if class == candidate || strings.HasPrefix(class, candidate+"_") {
							kind = candidate
						}
					}
					if call.kind != kind {
						t.Fatalf("class %s dispatched to %s instead of %s", class, call.kind, kind)
					}
					want := Sample{
						Iteration: i, Class: class, Nanoseconds: actual.Nanoseconds,
						Candidates: call.work.Candidates, LogicalBytes: call.work.Bytes,
						ServerReadRows: call.work.ServerReadRows, ServerReadBytes: call.work.ServerReadBytes,
						ServerElapsed: call.work.ServerElapsed, ResponseBytes: call.work.ResponseBytes,
						ServerSummary: call.work.ServerSummary,
						QueryID:       call.work.QueryID, QueryTransport: call.work.QueryTransport, ServerMetricsSource: call.work.ServerMetricsSource,
					}
					mismatch := false
					switch call.kind {
					case "trace":
						want.Results, want.Digest = len(call.spans), evidenceDigest(call.spans)
						mismatch = !reflect.DeepEqual(call.spans, evidenceSpans(c, call.trace))
					case "span":
						want.Results, want.Digest = len(call.matches), evidenceDigest(call.matches)
						mismatch = !reflect.DeepEqual(call.matches, bruteSpanOracle(c, call.span))
					case "time":
						want.Results, want.Digest = len(call.hits), evidenceDigest(call.hits)
						mismatch = !reflect.DeepEqual(call.hits, bruteTimeOracle(c, call.interval))
					default:
						want.Results, want.Digest = len(call.hits), evidenceDigest(call.hits)
						mismatch = !reflect.DeepEqual(call.hits, evidenceHits(c, call.attribute))
					}
					if call.err != nil {
						want.Error = call.err.Error()
					} else if mismatch {
						message := "equality identities/order differ from independent oracle"
						switch call.kind {
						case "trace":
							message = "trace result differs from independent oracle"
						case "span":
							message = "standalone span result differs from independent oracle"
						case "time":
							message = "time identities/order differ from independent oracle"
						}
						if !strings.Contains(actual.Error, message) {
							t.Fatalf("sample %d error=%q want oracle mismatch %q", i, actual.Error, message)
						}
						want.Error = actual.Error
					}
					if !reflect.DeepEqual(actual, want) || actual.Nanoseconds < 0 {
						t.Fatalf("sample %d\ngot  %+v\nwant %+v", i, actual, want)
					}
					if want.Error != "" {
						failures++
					} else {
						populations[class] = append(populations[class], actual.Nanoseconds)
					}
				}
				if e.Failures != failures || (err != nil) != (failures > 0) {
					t.Fatalf("failures=%d want=%d verdict=%v", e.Failures, failures, err)
				}
				if mode != "success" && !(suite == "span" && mode == "order") && failures == 0 {
					t.Fatal("negative test had no failures")
				}
				if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%d/%d queries failed", failures, c.Iterations)) {
					t.Fatalf("aggregate verdict: %v", err)
				}
				if len(e.Histograms) != len(populations) {
					t.Fatalf("histogram classes=%v want %v", e.Histograms, populations)
				}
				for class, values := range populations {
					sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
					n := len(values)
					want := Histogram{
						Population: n, Min: values[0], P50: values[(n-1)/2],
						P95: values[(95*n-1)/100], P99: values[(99*n-1)/100], Max: values[n-1],
					}
					if e.Histograms[class] != want {
						t.Fatalf("histogram %s=%+v want %+v", class, e.Histograms[class], want)
					}
				}
				if e.Elapsed < 0 || e.Backend != "fake" || e.Corpus != corpusIdentity(c) || e.Revision != "revision" {
					t.Fatalf("invalid evidence metadata: %+v", e)
				}
			})
		}
	}
}

func TestOwnershipFailureDoesNotRetainQueryProvenance(t *testing.T) {
	t.Parallel()
	for _, suite := range []string{"rare", "trace", "span", "time"} {
		t.Run(suite, func(t *testing.T) {
			t.Parallel()
			c := evidenceConfig()
			c.Iterations = 3
			ctx := t.Context()
			b := &evidenceBackend{cfg: c, mode: "ownership_after_success", ctx: ctx}
			var e Evidence
			err := MeasureQueries(ctx, b, c, suite, &e)
			if err == nil || e.Failures != 2 || len(e.Samples) != 3 || len(b.calls) != 3 {
				t.Fatalf("ownership-failure population: error=%v evidence=%+v calls=%d", err, e, len(b.calls))
			}
			first := e.Samples[0]
			if first.Error != "" || first.QueryID == "" || first.QueryTransport != "http" || first.ServerMetricsSource != "http_summary" || !first.ServerSummary || first.ServerReadRows == 0 || first.ServerReadBytes == 0 || first.ServerElapsed == 0 {
				t.Fatalf("missing successful provenance/metric control: %+v", first)
			}
			for i, sample := range e.Samples[1:] {
				if sample.QueryID != "" || sample.QueryTransport != "" || sample.ServerMetricsSource != "" || sample.ServerSummary || sample.Candidates != 0 || sample.LogicalBytes != 0 || sample.ServerReadRows != 0 || sample.ServerReadBytes != 0 || sample.ServerElapsed != 0 || sample.ResponseBytes != 0 || sample.Results != 0 || sample.Error == "" || sample.Digest != evidenceDigest([]Hit{}) {
					t.Fatalf("ownership failure sample %d retained prior data-query provenance/metrics: %+v", i+1, sample)
				}
			}
		})
	}
}

func TestEvidenceQueryProvenanceJSONAndBoundary(t *testing.T) {
	t.Parallel()
	for _, sample := range []Sample{
		{QueryID: "http-data-id", QueryTransport: "http", ServerMetricsSource: "http_summary", ServerSummary: true},
		{QueryID: "native-data-id", QueryTransport: "native", ServerMetricsSource: "unavailable"},
	} {
		wire, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["query_id"] != sample.QueryID || fields["query_transport"] != sample.QueryTransport || fields["server_metrics_source"] != sample.ServerMetricsSource || fields["server_summary_available"] != sample.ServerSummary {
			t.Fatalf("provenance receipt JSON: %s", wire)
		}
	}
	wire, err := json.Marshal(Sample{})
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]any
	if err := json.Unmarshal(wire, &empty); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"query_id", "query_transport", "server_metrics_source"} {
		if _, present := empty[name]; present {
			t.Fatalf("absent work side channel fabricated %s: %s", name, wire)
		}
	}
	e := NewEvidence(evidenceConfig(), "ch-map", "query", "all", "revision")
	for _, disclosure := range []string{"ownership checks and data queries use the selected transport", "HTTP JSON", "native typed transport", "DDL, ingest, storage audit and EXPLAIN remain HTTP", "not protocol-cost-equivalent"} {
		if !strings.Contains(e.Boundary, disclosure) {
			t.Fatalf("missing protocol measurement boundary %q: %s", disclosure, e.Boundary)
		}
	}
}

func TestQueryCasesShareCompactExpectedAnswers(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Count, c.Tenants, c.MatchEvery, c.Iterations = 1000, 1, 1, 100
	cases, err := queryCases(context.Background(), c, "rare")
	if err != nil {
		t.Fatal(err)
	}
	want := evidenceHits(c, Attribute{"span", QueryKey, "string", QueryValue})
	if len(cases) != c.Iterations || len(want) != c.K {
		t.Fatal("empty or wrong regression population")
	}
	for i, q := range cases {
		if !reflect.DeepEqual(q.expected, want) || cap(q.expected) != c.K || &q.expected[0] != &cases[0].expected[0] {
			t.Fatalf("case %d did not share one compact top-K answer: len=%d cap=%d", i, len(q.expected), cap(q.expected))
		}
	}
}

func TestHistogramBoundaries(t *testing.T) {
	t.Parallel()
	if got := histogram(nil); got != (Histogram{}) {
		t.Fatalf("empty histogram: %+v", got)
	}
	if got := histogram([]int64{7}); got != (Histogram{1, 7, 7, 7, 7, 7}) {
		t.Fatalf("singleton histogram: %+v", got)
	}
	if got := histogram([]int64{8, 2}); got != (Histogram{2, 2, 2, 8, 8, 8}) {
		t.Fatalf("two-sample histogram: %+v", got)
	}
	values := make([]int64, 100)
	for i := range values {
		values[i] = int64(100 - i)
	}
	if got := histogram(values); got != (Histogram{100, 1, 50, 95, 99, 100}) {
		t.Fatalf("percentiles: %+v", got)
	}
}

// Err-count cancellation makes preparation cancellation deterministic without a
// sleep, a large allocation, or racing the machine's corpus generation speed.
type evidenceCancelContext struct {
	context.Context
	remaining int
}

func (c *evidenceCancelContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestQueryPreparationCancellation(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	for _, checks := range []int{1, 2, 4} {
		ctx := &evidenceCancelContext{Context: context.Background(), remaining: checks}
		b := &evidenceBackend{cfg: c, mode: "success", ctx: ctx}
		var e Evidence
		err := MeasureQueries(ctx, b, c, "rare", &e)
		if !errors.Is(err, context.Canceled) || len(b.calls) != 0 || len(e.Samples) != 0 {
			t.Fatalf("cancel after %d checks: error=%v calls=%d samples=%d", checks, err, len(b.calls), len(e.Samples))
		}
	}
	var e Evidence
	b := &evidenceBackend{cfg: c, ctx: context.Background()}
	if err := MeasureQueries(b.ctx, b, c, "unknown", &e); err == nil || len(b.calls) != 0 {
		t.Fatalf("invalid suite: %v calls=%d", err, len(b.calls))
	}
}

func TestMeasurementCancellationStopsCalls(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ suite, mode string }{{"rare", "cancel"}, {"trace", "cancel"}, {"span", "cancel"}, {"time", "cancel"}, {"genai", "cancel"}, {"all", "cancel"}, {"rare", "cancel_ignores_context"}, {"trace", "cancel_ignores_context"}, {"span", "cancel_ignores_context"}, {"time", "cancel_ignores_context"}} {
		t.Run(test.suite+"/"+test.mode, func(t *testing.T) {
			t.Parallel()
			c := evidenceConfig()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &evidenceBackend{cfg: c, mode: test.mode, ctx: ctx, cancel: cancel}
			var e Evidence
			err := MeasureQueries(ctx, b, c, test.suite, &e)
			if !errors.Is(err, context.Canceled) || len(b.calls) != 1 || len(e.Samples) != 1 || e.Failures != 1 {
				t.Fatalf("error=%v calls=%d samples=%+v failures=%d", err, len(b.calls), e.Samples, e.Failures)
			}
			if e.Samples[0].Error != context.Canceled.Error() || e.Samples[0].Digest == "" || len(e.Histograms) != 0 {
				t.Fatalf("canceled receipt: %+v", e)
			}
			work := b.calls[0].work
			if e.Samples[0].QueryID != work.QueryID || e.Samples[0].QueryTransport != work.QueryTransport || e.Samples[0].ServerMetricsSource != work.ServerMetricsSource {
				t.Fatalf("canceled receipt lost current query provenance: sample=%+v work=%+v", e.Samples[0], work)
			}
		})
	}
}

func TestAuditQueriesControlledBackend(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "identity", "order", "transport", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c := evidenceConfig()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &evidenceBackend{cfg: c, mode: mode, ctx: ctx, cancel: cancel}
			err := AuditQueries(ctx, b, c)
			if mode == "success" {
				if err != nil || len(b.calls) != 6 || b.calls[5].trace == "" {
					t.Fatalf("audit error=%v calls=%+v", err, b.calls)
				}
			} else if err == nil || len(b.calls) != 1 {
				t.Fatalf("negative audit error=%v calls=%d", err, len(b.calls))
			}
			if mode == "transport" && !strings.Contains(err.Error(), "injected transport failure") {
				t.Fatalf("lost backend error: %v", err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
	c := evidenceConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &evidenceBackend{cfg: c, ctx: ctx}
	if err := AuditQueries(ctx, b, c); !errors.Is(err, context.Canceled) || len(b.calls) != 0 {
		t.Fatalf("pre-canceled audit error=%v calls=%d", err, len(b.calls))
	}
}

func TestQueryCasesSuites(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Iterations = 5
	for _, suite := range []string{"selective", "mixed", "rare", "trace"} {
		cases, err := queryCases(context.Background(), c, suite)
		if err != nil || len(cases) != 5 {
			t.Fatalf("%s: cases=%d error=%v", suite, len(cases), err)
		}
		for i, ordinal := range []int{0, 44, 40, 36, 32} {
			q := cases[i]
			if q.class != evidenceClass(suite, i) {
				t.Fatalf("%s case %d class=%s", suite, i, q.class)
			}
			if suite == "trace" {
				trace := Generate(c, ordinal).Trace
				if q.trace != trace || !reflect.DeepEqual(q.expectedTrace, evidenceSpans(c, trace)) {
					t.Fatalf("trace case %d: %+v", i, q)
				}
				continue
			}
			want := Attribute{"span", QueryKey, "string", QueryValue}
			if suite != "rare" {
				switch i {
				case 1:
					want = Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + Generate(c, 44).ID}
					if suite == "mixed" {
						want = Attribute{"resource", QueryKey, "string", QueryValue}
					}
				case 2:
					want = Attribute{"span", QueryKey + ".int", "string", QueryValue}
				case 3:
					want = Attribute{"span", QueryKey, "string", "absent_query_v1"}
				case 4:
					want = Attribute{"span", QueryKey, "string", "other_16"}
				}
			}
			if q.attribute != want || !reflect.DeepEqual(q.expected, evidenceHits(c, want)) {
				t.Fatalf("%s case %d: got %+v want attribute %+v", suite, i, q, want)
			}
		}
	}
}

func TestV2QueryCaseShapes(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Iterations = 32
	ordinals := []int{0, 44, 40, 36, 32, 28, 24, 20, 16, 12, 8, 4}
	for _, suite := range []string{"span", "genai", "time", "all"} {
		cases, err := queryCases(t.Context(), c, suite)
		if err != nil || len(cases) != c.Iterations {
			t.Fatalf("%s cases=%d error=%v", suite, len(cases), err)
		}
		for i, q := range cases {
			class := evidenceClass(suite, i)
			ordinal := ordinals[i%len(ordinals)]
			source := Generate(c, ordinal)
			if q.class != class || q.sourceOrdinal != ordinal {
				t.Fatalf("%s case %d: class=%s source=%d want class=%s source=%d", suite, i, q.class, q.sourceOrdinal, class, ordinal)
			}
			switch class {
			case "trace", "trace_absent":
				trace := source.Trace
				if class == "trace_absent" {
					trace = "00000000000000000000000000000000"
				}
				if q.kind != "trace" || q.trace != trace || !reflect.DeepEqual(q.expectedTrace, evidenceSpans(c, trace)) {
					t.Fatalf("%s case %d trace shape: %+v", suite, i, q)
				}
			case "span", "span_absent":
				span := source.ID
				if class == "span_absent" {
					span = "0000000000000000"
				}
				if q.kind != "span" || q.span != span || !reflect.DeepEqual(q.expectedSpans, bruteSpanOracle(c, span)) {
					t.Fatalf("%s case %d span shape: %+v", suite, i, q)
				}
			case "time_narrow", "time_empty", "time_absent", "time_broad":
				interval := TimeRange{Start: source.Start, End: source.Start + 1}
				switch class {
				case "time_empty":
					interval.End = interval.Start
				case "time_absent":
					interval = TimeRange{Start: 1699999999999999999, End: 1700000000000000000}
				case "time_broad":
					interval = TimeRange{Start: 1700000000000000000, End: 1700086400000000000}
				}
				if q.kind != "time" || q.interval != interval || !reflect.DeepEqual(q.expected, bruteTimeOracle(c, interval)) {
					t.Fatalf("%s case %d time shape: %+v", suite, i, q)
				}
			default:
				attribute := Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}
				switch class {
				case "genai_scope_resource":
					attribute.Scope = "resource"
				case "genai_scope_scope":
					attribute.Scope = "scope"
				case "genai_type":
					attribute.Type, attribute.Value = "int", "42"
				case "genai_absent":
					attribute.Value = "chatcmpl-fixture-v2-0000000000000000"
				case "genai_operation":
					attribute = Attribute{"span", "gen_ai.operation.name", "string", "chat"}
				case "genai_provider":
					attribute = Attribute{"span", "gen_ai.provider.name", "string", "openai"}
				case "genai_model":
					attribute = Attribute{"span", "gen_ai.request.model", "string", "gpt-4"}
				}
				if q.kind != "attribute" || q.attribute != attribute || !reflect.DeepEqual(q.expected, bruteAttributeOracle(c, attribute)) {
					t.Fatalf("%s case %d GenAI shape: %+v", suite, i, q)
				}
			}
		}
	}
}

func TestV2SuiteAdmission(t *testing.T) {
	t.Parallel()
	for _, suite := range []string{"genai", "all"} {
		for _, attributes := range []int{4, 7, 8, 128} {
			c := evidenceConfig()
			c.Attributes = attributes
			err := ValidateQuerySuite(c, suite)
			var config *ConfigError
			if attributes < 8 {
				if !errors.As(err, &config) {
					t.Fatalf("%s accepted %d attributes: %v", suite, attributes, err)
				}
			} else if err != nil {
				t.Fatalf("%s rejected %d attributes: %v", suite, attributes, err)
			}
		}
	}
	for _, suite := range []string{"span", "time", "all"} {
		c := evidenceConfig()
		ctx := t.Context()
		b := &evidenceBackend{cfg: c, mode: "success", ctx: ctx}
		var e Evidence
		var config *ConfigError
		err := MeasureQueries(ctx, &evidenceNoWorkBackend{b}, c, suite, &e)
		if !errors.As(err, &config) || len(b.calls) != 0 || len(e.Samples) != 0 {
			t.Fatalf("%s unsupported intrinsic backend: error=%v calls=%d samples=%+v", suite, err, len(b.calls), e.Samples)
		}
	}
}

type evidenceNoWorkBackend struct{ Backend }

func TestTraceReceiptWithoutWorkSideChannel(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	ctx := context.Background()
	b := &evidenceBackend{cfg: c, mode: "success", ctx: ctx}
	var e Evidence
	if err := MeasureQueries(ctx, &evidenceNoWorkBackend{b}, c, "trace", &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Samples) != c.Iterations {
		t.Fatalf("samples=%d", len(e.Samples))
	}
	for i, s := range e.Samples {
		if s.Candidates != 0 || s.LogicalBytes != 0 || s.ServerReadRows != 0 || s.ServerReadBytes != 0 || s.ServerElapsed != 0 || s.ResponseBytes != 0 || s.ServerSummary || s.QueryID != "" || s.QueryTransport != "" || s.ServerMetricsSource != "" || s.Digest != evidenceDigest(b.calls[i].spans) {
			t.Fatalf("sample %d fabricated work or lost results: %+v", i, s)
		}
	}
}

type evidenceIntrinsicWithoutWork struct {
	Backend
	IntrinsicBackend
}

func TestSpanReceiptWithoutWorkSideChannel(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	ctx := t.Context()
	b := &evidenceBackend{cfg: c, mode: "success", ctx: ctx}
	var e Evidence
	if err := MeasureQueries(ctx, &evidenceIntrinsicWithoutWork{Backend: b, IntrinsicBackend: b}, c, "span", &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Samples) != c.Iterations {
		t.Fatalf("samples=%d", len(e.Samples))
	}
	for i, sample := range e.Samples {
		if sample.Candidates != 0 || sample.LogicalBytes != 0 || sample.ServerReadRows != 0 || sample.ServerReadBytes != 0 || sample.ServerElapsed != 0 || sample.ResponseBytes != 0 || sample.ServerSummary || sample.QueryID != "" || sample.QueryTransport != "" || sample.ServerMetricsSource != "" || sample.Digest != evidenceDigest(b.calls[i].matches) {
			t.Fatalf("sample %d fabricated absent side-channel provenance or lost span results: %+v", i, sample)
		}
	}
}

func TestAuditTraceFailures(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"identity", "order", "transport", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c := evidenceConfig()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &evidenceBackend{cfg: c, mode: "success", traceMode: mode, ctx: ctx, cancel: cancel}
			err := AuditQueries(ctx, b, c)
			if err == nil || len(b.calls) != 6 || b.calls[5].trace == "" {
				t.Fatalf("error=%v calls=%+v", err, b.calls)
			}
			switch mode {
			case "identity", "order":
				if !strings.Contains(err.Error(), "audit trace/oracle mismatch") {
					t.Fatalf("wrong oracle error: %v", err)
				}
			case "transport":
				if !errors.Is(err, b.calls[5].err) {
					t.Fatalf("lost transport error: %v", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			}
		})
	}
}

func TestCancellationRetainsSuccessfulHistogram(t *testing.T) {
	t.Parallel()
	for _, suite := range []string{"rare", "trace", "span", "time"} {
		c := evidenceConfig()
		ctx, cancel := context.WithCancel(context.Background())
		b := &evidenceBackend{cfg: c, mode: "cancel_after_success", ctx: ctx, cancel: cancel}
		var e Evidence
		err := MeasureQueries(ctx, b, c, suite, &e)
		cancel()
		if !errors.Is(err, context.Canceled) || len(b.calls) != 2 || len(e.Samples) != 2 || e.Failures != 1 {
			t.Fatalf("%s error=%v calls=%d samples=%+v failures=%d", suite, err, len(b.calls), e.Samples, e.Failures)
		}
		ns := e.Samples[0].Nanoseconds
		if e.Samples[0].Error != "" || e.Samples[1].Error != context.Canceled.Error() || len(e.Histograms) != 1 || e.Histograms[evidenceClass(suite, 0)] != (Histogram{1, ns, ns, ns, ns, ns}) {
			t.Fatalf("%s: canceled receipt lost successful population: %+v", suite, e)
		}
		for i, sample := range e.Samples {
			work := b.calls[i].work
			if sample.QueryID != work.QueryID || sample.QueryTransport != work.QueryTransport || sample.ServerMetricsSource != work.ServerMetricsSource || sample.ServerSummary != work.ServerSummary || sample.ServerReadRows != work.ServerReadRows || sample.ServerReadBytes != work.ServerReadBytes || sample.ServerElapsed != work.ServerElapsed || sample.ResponseBytes != work.ResponseBytes {
				t.Fatalf("%s sample %d retained stale provenance/metrics on cancellation: sample=%+v work=%+v", suite, i, sample, work)
			}
		}
	}
}
