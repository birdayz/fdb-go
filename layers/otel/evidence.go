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
	Iteration       int    `json:"iteration"`
	Class           string `json:"class"`
	Nanoseconds     int64  `json:"ns"`
	Results         int    `json:"results"`
	Candidates      int    `json:"consumed_candidates,omitempty"`
	LogicalBytes    int64  `json:"consumed_logical_bytes,omitempty"`
	ServerReadRows  int64  `json:"server_read_rows,omitempty"`
	ServerReadBytes int64  `json:"server_read_bytes,omitempty"`
	ServerElapsed   int64  `json:"server_elapsed_ns,omitempty"`
	ResponseBytes   int64  `json:"http_response_bytes,omitempty"`
	ServerSummary   bool   `json:"server_summary_available"`
	Digest          string `json:"result_digest,omitempty"`
	Error           string `json:"error,omitempty"`
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
	return Evidence{Tool: ToolVersion, Backend: backend, Mode: mode, Suite: suite, Revision: revision, Go: runtime.Version(), Config: c, Corpus: corpusIdentity(c), Start: time.Now().UTC().Format(time.RFC3339Nano), Boundary: "immutable unique synthetic spans; local closed-loop diagnostic, not SLO/HA/production evidence; covering string/int equality and full trace lookup only"}
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
	class         string
	attribute     Attribute
	trace         string
	expected      []Hit
	expectedTrace []SpanHit
}

func queryCases(ctx context.Context, c Config, suite string) ([]queryCase, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "prepare queries", err)
	}
	if suite != "selective" && suite != "mixed" && suite != "rare" && suite != "trace" {
		return nil, fail("config", "prepare queries", "suite must be selective, mixed, rare or trace")
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
		q := queryCase{class: "rare", attribute: Attribute{"span", QueryKey, "string", QueryValue}}
		if suite == "trace" {
			q.class = "trace"
			q.trace = source.Trace
		} else if suite != "rare" {
			switch i % 5 {
			case 1:
				q.class = "custom_varying"
				j := 4 + i%max(1, c.Attributes-4)
				if j >= len(source.Attributes) {
					j = 3
				}
				q.attribute = source.Attributes[j]
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
			if suite == "mixed" && i%5 == 1 {
				q.class = "resource_common"
				q.attribute = Attribute{"resource", QueryKey, "string", QueryValue}
			}
		}
		cases[i] = q
	}
	// Compute expected answers for all unique predicates in one source traversal.
	// These maps are oracle state only; backend query results are never cached.
	lookup := map[Attribute]map[string]int64{}
	traces := map[string][]SpanHit{}
	for _, q := range cases {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "prepare queries", err)
		}
		if q.class != "trace" {
			if _, ok := lookup[q.attribute]; !ok {
				lookup[q.attribute] = map[string]int64{}
			}
		} else if _, ok := traces[q.trace]; !ok {
			traces[q.trace] = []SpanHit{}
		}
	}
	for i := 0; i < c.Count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "prepare queries", err)
		}
		s := Generate(c, i)
		if s.Tenant != c.Tenant {
			continue
		}
		if _, ok := traces[s.Trace]; ok {
			traces[s.Trace] = append(traces[s.Trace], SpanHit{s.ID, s.Start})
		}
		for _, a := range s.Attributes {
			m, ok := lookup[a]
			if !ok {
				continue
			}
			if t, ok := m[s.Trace]; !ok || s.Start > t {
				m[s.Trace] = s.Start
			}
		}
	}
	expected := make(map[Attribute][]Hit, len(lookup))
	for attribute, latest := range lookup {
		out, err := oracleTopKContext(ctx, latest, c.K)
		if err != nil {
			return nil, err
		}
		// Retain only K hits, once per predicate; cases share immutable answers.
		compact := make([]Hit, len(out))
		copy(compact, out)
		expected[attribute] = compact
	}
	for trace := range traces {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "prepare queries", err)
		}
		oracleTraceSort(traces[trace])
	}
	for i := range cases {
		if err := ctx.Err(); err != nil {
			return nil, wrap("canceled", "prepare queries", err)
		}
		if cases[i].class != "trace" {
			cases[i].expected = expected[cases[i].attribute]
		} else {
			cases[i].expectedTrace = traces[cases[i].trace]
		}
	}
	return cases, nil
}

// MeasureQueries excludes corpus/oracle preparation; includes manifest guards,
// backend RPCs, decoding and exact covering verification. No warmup is hidden.
func MeasureQueries(ctx context.Context, b Backend, c Config, suite string, e *Evidence) error {
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
		if q.class == "trace" {
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
			}
			sample.Digest = resultDigest(got)
			if err != nil {
				sample.Error = err.Error()
			}
		} else {
			got, err := b.Query(ctx, q.attribute)
			sample.Nanoseconds = time.Since(before).Nanoseconds()
			if err == nil && !sameJSON(got.Hits, q.expected) {
				err = fail("oracle", "measure equality", "equality identities/order differ from independent oracle")
			}
			sample.Results = len(got.Hits)
			sample.Candidates = got.Candidates
			sample.LogicalBytes = got.Bytes
			sample.ServerReadRows = got.ServerReadRows
			sample.ServerReadBytes = got.ServerReadBytes
			sample.ServerElapsed = got.ServerElapsed
			sample.ResponseBytes = got.ResponseBytes
			sample.ServerSummary = got.ServerSummary
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
	for _, a := range []Attribute{{"span", QueryKey, "string", QueryValue}, {"resource", QueryKey + ".int", "string", QueryValue}, {"scope", QueryKey + ".int", "string", QueryValue}, {"span", QueryKey + ".int", "string", QueryValue}, {"span", QueryKey, "string", "absent_query_v1"}} {
		expected, err := ExpectedContext(ctx, c, a.Scope, a.Key, a.Type, a.Value)
		if err != nil {
			return err
		}
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
	expected, err := ExpectedTraceContext(ctx, c, s.Trace)
	if err != nil {
		return err
	}
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
