package otel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"time"
)

type Sample struct {
	Iteration           int    `json:"iteration"`
	Class               string `json:"class"`
	Nanoseconds         int64  `json:"ns"`
	Results             int    `json:"results"`
	Candidates          int    `json:"consumed_candidates,omitempty"`
	LogicalBytes        int64  `json:"consumed_logical_bytes,omitempty"`
	ServerReadRows      int64  `json:"server_read_rows,omitempty"`
	ServerReadBytes     int64  `json:"server_read_bytes,omitempty"`
	ServerElapsed       int64  `json:"server_elapsed_ns,omitempty"`
	ResponseBytes       int64  `json:"http_response_bytes,omitempty"`
	ServerSummary       bool   `json:"server_summary_available"`
	QueryID             string `json:"query_id,omitempty"`
	QueryTransport      string `json:"query_transport,omitempty"`
	ServerMetricsSource string `json:"server_metrics_source,omitempty"`
	Digest              string `json:"result_digest,omitempty"`
	Error               string `json:"error,omitempty"`
}
type Histogram struct {
	Population int   `json:"population"`
	Min        int64 `json:"min_ns"`
	P50        int64 `json:"p50_ns"`
	P95        int64 `json:"p95_ns"`
	P99        int64 `json:"p99_ns"`
	Max        int64 `json:"max_ns"`
}
type Evidence struct {
	Tool       string               `json:"tool"`
	Backend    string               `json:"backend"`
	Mode       string               `json:"mode"`
	Suite      string               `json:"suite,omitempty"`
	Revision   string               `json:"revision"`
	Go         string               `json:"go"`
	Config     Config               `json:"config"`
	Corpus     string               `json:"corpus_fingerprint"`
	Start      string               `json:"start_utc"`
	Elapsed    int64                `json:"wall_ns"`
	Stats      Statistics           `json:"stats"`
	Samples    []Sample             `json:"samples,omitempty"`
	Histograms map[string]Histogram `json:"histograms,omitempty"`
	Failures   int                  `json:"failures"`
	Error      string               `json:"error,omitempty"`
	Boundary   string               `json:"boundary"`
}

func NewEvidence(c Config, backend, mode, suite, revision string) Evidence {
	return Evidence{Tool: ToolVersion, Backend: backend, Mode: mode, Suite: suite, Revision: revision, Go: runtime.Version(), Config: c, Corpus: corpusIdentity(c), Start: time.Now().UTC().Format(time.RFC3339Nano), Boundary: "fixture V2 immutable synthetic spans; local closed-loop diagnostic, not SLO/HA/production evidence; typed scoped equality, complete trace, standalone span ID, intrinsic half-open time; synthetic ID/response uniqueness is not a provider or semconv guarantee; ClickHouse ownership checks and data queries use the selected transport, HTTP JSON or native typed transport; DDL, ingest, storage audit and EXPLAIN remain HTTP; transport arms are not protocol-cost-equivalent"}
}

func resultDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func histogram(samples []int64) Histogram {
	if len(samples) == 0 {
		return Histogram{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	q := func(p int) int64 {
		i := (len(samples)*p+99)/100 - 1
		if i < 0 {
			i = 0
		}
		return samples[i]
	}
	return Histogram{len(samples), samples[0], q(50), q(95), q(99), samples[len(samples)-1]}
}

type queryCase struct {
	kind          string
	class         string
	attribute     Attribute
	trace         string
	span          string
	interval      TimeRange
	sourceOrdinal int
	expected      []Hit
	expectedTrace []SpanHit
	expectedSpans []SpanMatch
}

func ValidateQuerySuite(c Config, suite string) error {
	switch suite {
	case "selective", "mixed", "rare", "trace", "span", "time":
		return nil
	case "genai", "all":
		if c.Attributes < 8 {
			return fail("config", "prepare queries", "genai and all suites require at least 8 attributes")
		}
		return nil
	default:
		return fail("config", "prepare queries", "suite must be selective, mixed, rare, trace, span, genai, time or all")
	}
}

func intrinsicQueryCase(c Config, source Span, kind string, i int) queryCase {
	q := queryCase{kind: kind, sourceOrdinal: source.Ordinal}
	switch kind {
	case "trace":
		q.class, q.trace = "trace", source.Trace
		if i%2 != 0 {
			q.class, q.trace = "trace_absent", "00000000000000000000000000000000"
		}
	case "span":
		q.class, q.span = "span", source.ID
		if i%2 != 0 {
			q.class, q.span = "span_absent", "0000000000000000"
		}
	case "time":
		q.class, q.interval = "time_narrow", TimeRange{Start: source.Start, End: source.Start + 1}
		switch i % 4 {
		case 1:
			q.class, q.interval = "time_empty", TimeRange{Start: source.Start, End: source.Start}
		case 2:
			q.class, q.interval = "time_absent", TimeRange{Start: 1699999999999999999, End: 1700000000000000000}
		case 3:
			q.class, q.interval = "time_broad", TimeRange{Start: 1700000000000000000, End: 1700086400000000000}
		}
	case "genai":
		q.kind, q.class = "attribute", "genai_response"
		q.attribute = Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}
		switch i % 8 {
		case 1:
			q.class, q.attribute.Scope = "genai_scope_resource", "resource"
		case 2:
			q.class, q.attribute.Scope = "genai_scope_scope", "scope"
		case 3:
			q.class, q.attribute.Type, q.attribute.Value = "genai_type", "int", "42"
		case 4:
			q.class, q.attribute.Value = "genai_absent", "chatcmpl-fixture-v2-0000000000000000"
		case 5:
			q.class, q.attribute = "genai_operation", source.Attributes[4]
		case 6:
			q.class, q.attribute = "genai_provider", source.Attributes[5]
		case 7:
			q.class, q.attribute = "genai_model", source.Attributes[6]
		}
	}
	return q
}

func queryCases(ctx context.Context, c Config, suite string) ([]queryCase, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "prepare queries", err)
	}
	if err := ValidateQuerySuite(c, suite); err != nil {
		return nil, err
	}
	cases := make([]queryCase, c.Iterations)
	for i := range cases {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "prepare queries", err)
		}
		ordinal := 2 * (c.Tenant + ((i*7919)%max(1, (c.Count/2+c.Tenants-1)/c.Tenants))*c.Tenants)
		if ordinal >= c.Count {
			ordinal = 2 * c.Tenant
		}
		if ordinal >= c.Count {
			ordinal = c.Tenant
		}
		source := Generate(c, ordinal)
		q := queryCase{kind: "attribute", sourceOrdinal: ordinal, class: "rare", attribute: Attribute{"span", QueryKey, "string", QueryValue}}
		if suite == "trace" {
			q.kind, q.class, q.trace = "trace", "trace", source.Trace
		} else if suite == "span" || suite == "genai" || suite == "time" {
			q = intrinsicQueryCase(c, source, suite, i)
		} else if suite == "all" {
			switch j := i % 16; {
			case j < 2:
				q = intrinsicQueryCase(c, source, "trace", j)
			case j < 4:
				q = intrinsicQueryCase(c, source, "span", j-2)
			case j < 12:
				q = intrinsicQueryCase(c, source, "genai", j-4)
			default:
				q = intrinsicQueryCase(c, source, "time", j-12)
			}
		} else if suite != "rare" {
			switch i % 5 {
			case 1:
				q.class = "custom_varying"
				if suite == "mixed" {
					q.class = "resource_common"
					q.attribute = Attribute{"resource", QueryKey, "string", QueryValue}
				} else if c.Attributes >= 8 {
					q.attribute = source.Attributes[7]
				} else if c.Attributes > 4 {
					q.attribute = source.Attributes[4+i%(c.Attributes-4)]
				} else {
					q.kind, q.class, q.trace = "trace", "trace_varying", source.Trace
					q.attribute = Attribute{}
				}
			case 2:
				q.class = "lookalike_type"
				q.attribute = Attribute{"span", QueryKey + ".int", "string", QueryValue}
			case 3:
				q.class = "absent"
				q.attribute = Attribute{"span", QueryKey, "string", "absent_query_v1"}
			case 4:
				q.class = "id_varying"
				q.attribute = source.Attributes[0]
			}
		}
		cases[i] = q
	}
	if _, err := prepareQueryOracles(ctx, c, cases); err != nil {
		return nil, err
	}
	return cases, nil
}

// MeasureQueries excludes corpus/oracle preparation; includes manifest guards,
// backend RPCs, decoding and exact covering verification. No warmup is hidden.
func MeasureQueries(ctx context.Context, b Backend, c Config, suite string, e *Evidence) error {
	if err := ctx.Err(); err != nil {
		return wrap("canceled", "measure queries", err)
	}
	intrinsic, capable := b.(IntrinsicBackend)
	if (suite == "span" || suite == "time" || suite == "all") && !capable {
		return fail("config", "measure queries", "backend does not support intrinsic span/time queries")
	}
	cases, err := queryCases(ctx, c, suite)
	if err != nil {
		return err
	}
	durations := map[string][]int64{}
	start := time.Now()
	defer func() {
		e.Elapsed = time.Since(start).Nanoseconds()
		e.Histograms = map[string]Histogram{}
		for class, pop := range durations {
			e.Histograms[class] = histogram(pop)
		}
	}()
	for i, q := range cases {
		if err := ctx.Err(); err != nil {
			return wrap("canceled", "measure queries", err)
		}
		sample := Sample{Iteration: i, Class: q.class}
		before := time.Now()
		if q.kind == "trace" {
			got, err := b.Trace(ctx, q.trace)
			sample.Nanoseconds = time.Since(before).Nanoseconds()
			if err == nil && !sameJSON(got, q.expectedTrace) {
				err = fail("oracle", "measure trace", "trace result differs from independent oracle")
			}
			sample.Results = len(got)
			if w, ok := b.(interface{ LastQueryWork() QueryResult }); ok {
				work := w.LastQueryWork()
				sample.Candidates = work.Candidates
				sample.LogicalBytes = work.Bytes
				sample.ServerReadRows = work.ServerReadRows
				sample.ServerReadBytes = work.ServerReadBytes
				sample.ServerElapsed = work.ServerElapsed
				sample.ResponseBytes = work.ResponseBytes
				sample.ServerSummary = work.ServerSummary
				sample.QueryID, sample.QueryTransport, sample.ServerMetricsSource = work.QueryID, work.QueryTransport, work.ServerMetricsSource
			}
			sample.Digest = resultDigest(got)
			if err != nil {
				sample.Error = err.Error()
			}
		} else if q.kind == "span" {
			got, err := intrinsic.Span(ctx, q.span)
			sample.Nanoseconds = time.Since(before).Nanoseconds()
			if err == nil && !sameJSON(got, q.expectedSpans) {
				err = fail("oracle", "measure span", "standalone span result differs from independent oracle")
			}
			sample.Results, sample.Digest = len(got), resultDigest(got)
			if w, ok := b.(interface{ LastQueryWork() QueryResult }); ok {
				work := w.LastQueryWork()
				sample.Candidates, sample.LogicalBytes = work.Candidates, work.Bytes
				sample.ServerReadRows, sample.ServerReadBytes = work.ServerReadRows, work.ServerReadBytes
				sample.ServerElapsed, sample.ResponseBytes, sample.ServerSummary = work.ServerElapsed, work.ResponseBytes, work.ServerSummary
				sample.QueryID, sample.QueryTransport, sample.ServerMetricsSource = work.QueryID, work.QueryTransport, work.ServerMetricsSource
			}
			if err != nil {
				sample.Error = err.Error()
			}
		} else {
			var got QueryResult
			var err error
			if q.kind == "time" {
				got, err = intrinsic.Time(ctx, q.interval)
			} else {
				got, err = b.Query(ctx, q.attribute)
			}
			sample.Nanoseconds = time.Since(before).Nanoseconds()
			if err == nil && !sameJSON(got.Hits, q.expected) {
				if q.kind == "time" {
					err = fail("oracle", "measure time", "time identities/order differ from independent oracle")
				} else {
					err = fail("oracle", "measure equality", "equality identities/order differ from independent oracle")
				}
			}
			sample.Results = len(got.Hits)
			sample.Candidates = got.Candidates
			sample.LogicalBytes = got.Bytes
			sample.ServerReadRows = got.ServerReadRows
			sample.ServerReadBytes = got.ServerReadBytes
			sample.ServerElapsed = got.ServerElapsed
			sample.ResponseBytes = got.ResponseBytes
			sample.ServerSummary = got.ServerSummary
			sample.QueryID, sample.QueryTransport, sample.ServerMetricsSource = got.QueryID, got.QueryTransport, got.ServerMetricsSource
			sample.Digest = resultDigest(got.Hits)
			if err != nil {
				sample.Error = err.Error()
			}
		}
		if err := ctx.Err(); err != nil && sample.Error == "" {
			sample.Error = err.Error()
		}
		if sample.Error != "" {
			e.Failures++
		} else {
			durations[q.class] = append(durations[q.class], sample.Nanoseconds)
		}
		e.Samples = append(e.Samples, sample)
		if err := ctx.Err(); err != nil {
			return wrap("canceled", "measure queries", err)
		}
	}
	if e.Failures > 0 {
		return fail("oracle", "measure queries", fmt.Sprintf("%d/%d queries failed; raw samples include failures; histograms are successful-only", e.Failures, len(cases)))
	}
	return nil
}

// AuditQueries checks positive, scoped/type-lookalike and negative predicates
// plus trace lookup against source truth; it does not substitute for source audit.
func AuditQueries(ctx context.Context, b Backend, c Config) error {
	attributes := []Attribute{{"span", QueryKey, "string", QueryValue}, {"resource", QueryKey + ".int", "string", QueryValue}, {"scope", QueryKey + ".int", "string", QueryValue}, {"span", QueryKey + ".int", "string", QueryValue}, {"span", QueryKey, "string", "absent_query_v1"}}
	expectedAttributes, _, _, err := scanOraclePredicates(ctx, c, attributes, nil)
	if err != nil {
		return err
	}
	for _, a := range attributes {
		expected := expectedAttributes[a]
		if err := ctx.Err(); err != nil {
			return wrap("canceled", "audit queries", err)
		}
		got, err := b.Query(ctx, a)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return wrap("canceled", "audit queries", err)
		}
		if !sameJSON(got.Hits, expected) {
			return fail("oracle", "audit equality", "audit query/oracle mismatch")
		}
	}
	s := Generate(c, min(2*c.Tenant, c.Count-1))
	traceCases := []queryCase{{kind: "trace", trace: s.Trace, sourceOrdinal: s.Ordinal}}
	if _, err := prepareQueryOracles(ctx, c, traceCases); err != nil {
		return err
	}
	expected := traceCases[0].expectedTrace
	if err := ctx.Err(); err != nil {
		return wrap("canceled", "audit queries", err)
	}
	got, err := b.Trace(ctx, s.Trace)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return wrap("canceled", "audit queries", err)
	}
	if !sameJSON(got, expected) {
		return fail("oracle", "audit trace", "audit trace/oracle mismatch")
	}
	return nil
}
