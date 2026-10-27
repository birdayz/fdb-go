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
	attribute Attribute
	trace     string
	work      QueryResult
	hits      []Hit
	spans     []SpanHit
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
		ServerSummary: n%2 == 0,
	}}
	if mode == "transport" {
		call.err = errors.New("injected transport failure")
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
	call.attribute = a
	call.hits = evidenceHits(b.cfg, a)
	switch mode {
	case "identity":
		call.hits = append(call.hits, Hit{"wrong-trace", -1})
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
	call.trace = trace
	call.spans = evidenceSpans(b.cfg, trace)
	switch mode {
	case "identity":
		call.spans[0].ID = "wrong-span"
	case "order":
		call.spans[0], call.spans[1] = call.spans[1], call.spans[0]
	}
	b.lastWork = call.work
	b.calls = append(b.calls, call)
	return call.spans, call.err
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
	for _, suite := range []string{"selective", "mixed", "rare", "trace"} {
		for _, mode := range []string{"success", "identity", "order", "transport", "mixed"} {
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
				populations := map[string][]int64{}
				for i, call := range b.calls {
					actual := e.Samples[i]
					class := evidenceClass(suite, i)
					want := Sample{
						Iteration: i, Class: class, Nanoseconds: actual.Nanoseconds,
						Candidates: call.work.Candidates, LogicalBytes: call.work.Bytes,
						ServerReadRows: call.work.ServerReadRows, ServerReadBytes: call.work.ServerReadBytes,
						ServerElapsed: call.work.ServerElapsed, ResponseBytes: call.work.ResponseBytes,
						ServerSummary: call.work.ServerSummary,
					}
					mismatch := false
					if suite == "trace" {
						want.Results, want.Digest = len(call.spans), evidenceDigest(call.spans)
						mismatch = !reflect.DeepEqual(call.spans, evidenceSpans(c, call.trace))
					} else {
						want.Results, want.Digest = len(call.hits), evidenceDigest(call.hits)
						mismatch = !reflect.DeepEqual(call.hits, evidenceHits(c, call.attribute))
					}
					if call.err != nil {
						want.Error = call.err.Error()
					} else if mismatch {
						message := "equality identities/order differ from independent oracle"
						if suite == "trace" {
							message = "trace result differs from independent oracle"
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
				if mode != "success" && failures == 0 {
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
	for _, checks := range []int{1, 2, c.Iterations + 2, 2*c.Iterations + 2, 2*c.Iterations + c.Count + 2} {
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
	for _, test := range []struct{ suite, mode string }{{"rare", "cancel"}, {"trace", "cancel"}, {"rare", "cancel_ignores_context"}, {"trace", "cancel_ignores_context"}} {
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
					want = Attribute{"span", "custom.literal.5", "string", "v_5_44"}
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
		if s.Candidates != 0 || s.LogicalBytes != 0 || s.ServerReadRows != 0 || s.ServerReadBytes != 0 || s.ServerElapsed != 0 || s.ResponseBytes != 0 || s.ServerSummary || s.Digest != evidenceDigest(b.calls[i].spans) {
			t.Fatalf("sample %d fabricated work or lost results: %+v", i, s)
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
	for _, suite := range []string{"rare", "trace"} {
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
		if e.Samples[0].Error != "" || e.Samples[1].Error != context.Canceled.Error() || len(e.Histograms) != 1 || e.Histograms[suite] != (Histogram{1, ns, ns, ns, ns, ns}) {
			t.Fatalf("%s: canceled receipt lost successful population: %+v", suite, e)
		}
	}
}
