package otel

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"
)

// These small-fixture references deliberately retain all matching traces. The
// production oracle must produce the same answers without that population map.
func bruteOracleHits(c Config, matches func(Span) bool) []Hit {
	latest := map[string]int64{}
	for ordinal := 0; ordinal < c.Count; ordinal++ {
		s := Generate(c, ordinal)
		if s.Tenant != c.Tenant || !matches(s) {
			continue
		}
		if ns, ok := latest[s.Trace]; !ok || s.Start > ns {
			latest[s.Trace] = s.Start
		}
	}
	out := []Hit{}
	for trace, start := range latest {
		out = append(out, Hit{Trace: trace, Latest: start})
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

func bruteAttributeOracle(c Config, attribute Attribute) []Hit {
	return bruteOracleHits(c, func(s Span) bool {
		for _, a := range s.Attributes {
			if a == attribute {
				return true
			}
		}
		return false
	})
}

func bruteTimeOracle(c Config, interval TimeRange) []Hit {
	return bruteOracleHits(c, func(s Span) bool {
		return s.Start >= interval.Start && s.Start < interval.End
	})
}

func bruteSpanOracle(c Config, id string) []SpanMatch {
	out := []SpanMatch{}
	for ordinal := 0; ordinal < c.Count; ordinal++ {
		s := Generate(c, ordinal)
		if s.Tenant == c.Tenant && s.ID == id {
			out = append(out, SpanMatch{Trace: s.Trace, ID: s.ID, Start: s.Start})
		}
	}
	return out
}

func TestBoundedAttributeOracleMatchesBruteForce(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 2, 3, 47, 128} {
		for _, attributes := range []int{4, 7, 8, 12} {
			for _, k := range []int{1, 3, 100} {
				c := evidenceConfig()
				c.Count, c.Attributes, c.K = count, attributes, k
				c.Tenants = min(3, (count+1)/2)
				for tenant := 0; tenant < c.Tenants; tenant++ {
					c.Tenant = tenant
					predicates := []Attribute{
						{"span", QueryKey, "string", QueryValue},
						{"resource", QueryKey, "string", QueryValue},
						{"scope", QueryKey, "string", QueryValue},
						{"span", QueryKey, "int", QueryValue},
						{"span", QueryKey + ".int", "int", QueryValue},
						{"span", QueryKey + ".int", "string", QueryValue},
						{"span", QueryKey, "string", "absent_query_v1"},
						{"span", "gen_ai.operation.name", "string", "chat"},
						{"span", "gen_ai.provider.name", "string", "openai"},
						{"span", "gen_ai.request.model", "string", "gpt-4"},
						{"resource", "gen_ai.operation.name", "string", "chat"},
						{"span", "gen_ai.operation.name", "int", "chat"},
					}
					for _, ordinal := range []int{0, count - 1} {
						predicates = append(predicates, Generate(c, ordinal).Attributes...)
					}
					for _, a := range predicates {
						got, err := ExpectedContext(t.Context(), c, a.Scope, a.Key, a.Type, a.Value)
						want := bruteAttributeOracle(c, a)
						if err != nil || !reflect.DeepEqual(got, want) || cap(got) != len(got) {
							t.Fatalf("count=%d attributes=%d tenant=%d K=%d predicate=%+v: got=%+v cap=%d want=%+v error=%v", count, attributes, tenant, k, a, got, cap(got), want, err)
						}
					}
				}
			}
		}
	}
}

func TestBoundedTimeOracleMatchesBruteForce(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 2, 3, 47, 129} {
		for _, k := range []int{1, 3, 100} {
			c := evidenceConfig()
			c.Count, c.K = count, k
			c.Tenants = min(3, (count+1)/2)
			first, last := Generate(c, 0), Generate(c, count-1)
			intervals := []TimeRange{
				{Start: 0, End: math.MaxInt64},
				{Start: 0, End: 0},
				{Start: first.Start, End: first.Start + 1},
				{Start: first.Start - 1, End: first.Start},
				{Start: last.Start, End: last.Start + 1},
				{Start: last.Start, End: last.Start},
			}
			for tenant := 0; tenant < c.Tenants; tenant++ {
				c.Tenant = tenant
				for _, interval := range intervals {
					got, err := ExpectedTimeContext(t.Context(), c, interval)
					want := bruteTimeOracle(c, interval)
					if err != nil || !reflect.DeepEqual(got, want) || cap(got) != len(got) {
						t.Fatalf("count=%d tenant=%d K=%d interval=%+v: got=%+v cap=%d want=%+v error=%v", count, tenant, k, interval, got, cap(got), want, err)
					}
				}
			}
		}
	}
	var config *ConfigError
	if _, err := ExpectedTimeContext(t.Context(), evidenceConfig(), TimeRange{Start: 5, End: 4}); !errors.As(err, &config) {
		t.Fatalf("reversed time range accepted or untyped: %v", err)
	}
}

func TestPointOraclesOddCountsAndTenants(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 2, 3, 47} {
		c := evidenceConfig()
		c.Count, c.K = count, 1
		c.Tenants = min(3, (count+1)/2)
		for tenant := 0; tenant < c.Tenants; tenant++ {
			c.Tenant = tenant
			for ordinal := 0; ordinal < count; ordinal++ {
				s := Generate(c, ordinal)
				spans, err := ExpectedSpanContext(t.Context(), c, s.ID)
				if want := bruteSpanOracle(c, s.ID); err != nil || !reflect.DeepEqual(spans, want) {
					t.Fatalf("count=%d tenant=%d ordinal=%d span: got=%+v want=%+v error=%v", count, tenant, ordinal, spans, want, err)
				}
				trace, err := ExpectedTraceContext(t.Context(), c, s.Trace)
				if want := evidenceSpans(c, s.Trace); err != nil || !reflect.DeepEqual(trace, want) {
					t.Fatalf("count=%d tenant=%d ordinal=%d trace: got=%+v want=%+v error=%v", count, tenant, ordinal, trace, want, err)
				}
				if spans != nil && len(spans) > 1 || len(trace) > 2 {
					t.Fatal("point oracle violated unique span / two-span trace contract")
				}
			}
		}
		for _, missing := range []string{"0000000000000000", "ffffffffffffffff"} {
			got, err := ExpectedSpanContext(t.Context(), c, missing)
			if want := bruteSpanOracle(c, missing); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("missing span got=%+v want=%+v error=%v", got, want, err)
			}
		}
		got, err := ExpectedTraceContext(t.Context(), c, "00000000000000000000000000000000")
		if err != nil || len(got) != 0 {
			t.Fatalf("zero trace is not a miss: %+v %v", got, err)
		}
	}
}

func TestOracleCancellation(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	for _, checks := range []int{1, 2, 4} {
		for _, api := range []string{"attribute", "time", "trace", "span"} {
			ctx := &evidenceCancelContext{Context: context.Background(), remaining: checks}
			var err error
			switch api {
			case "attribute":
				_, err = ExpectedContext(ctx, c, "resource", QueryKey, "string", QueryValue)
			case "time":
				_, err = ExpectedTimeContext(ctx, c, TimeRange{Start: 0, End: math.MaxInt64})
			case "trace":
				_, err = ExpectedTraceContext(ctx, c, "00000000000000000000000000000000")
			case "span":
				_, err = ExpectedSpanContext(ctx, c, "0000000000000000")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s canceled at check %d: %v", api, checks, err)
			}
		}
	}
}

func TestPreparedOraclesBoundedAndShared(t *testing.T) {
	t.Parallel()
	for _, count := range []int{3, 257, 1001} {
		c := evidenceConfig()
		c.Count, c.Tenants, c.MatchEvery, c.K = count, 1, 1, 2
		common := Attribute{"resource", QueryKey, "string", QueryValue}
		rare := Attribute{"span", QueryKey, "string", QueryValue}
		interval := TimeRange{Start: 0, End: math.MaxInt64}
		cases := []queryCase{}
		for repeat := 0; repeat < 30; repeat++ {
			cases = append(cases,
				queryCase{kind: "attribute", attribute: common},
				queryCase{attribute: rare},
				queryCase{kind: "time", interval: interval},
			)
		}
		stats, err := prepareQueryOracles(t.Context(), c, cases)
		if err != nil || stats.Predicates != 3 || stats.ScannedSpans != count || stats.PeakRetainedHits > 3*c.K || stats.PeakRetainedHits == 0 {
			t.Fatalf("count=%d: stats=%+v error=%v", count, stats, err)
		}
		want := [][]Hit{bruteAttributeOracle(c, common), bruteAttributeOracle(c, rare), bruteTimeOracle(c, interval)}
		for i, q := range cases {
			if !reflect.DeepEqual(q.expected, want[i%3]) || cap(q.expected) != len(q.expected) {
				t.Fatalf("count=%d case=%d: got=%+v cap=%d want=%+v", count, i, q.expected, cap(q.expected), want[i%3])
			}
			if len(q.expected) > 0 && &q.expected[0] != &cases[i%3].expected[0] {
				t.Fatalf("count=%d case=%d: duplicate predicate did not share immutable answer", count, i)
			}
		}
	}
}

func TestPreparedPointOraclesDoNotScan(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Count = 100000000
	for _, ordinal := range []int{0, 2, c.Count - 2} {
		s := Generate(c, ordinal)
		c.Tenant = s.Tenant
		cases := []queryCase{
			{kind: "trace", trace: s.Trace, sourceOrdinal: ordinal},
			{kind: "span", span: s.ID, sourceOrdinal: ordinal},
			{kind: "trace", trace: "00000000000000000000000000000000", sourceOrdinal: ordinal},
			{kind: "span", span: "0000000000000000", sourceOrdinal: ordinal},
		}
		stats, err := prepareQueryOracles(t.Context(), c, cases)
		if err != nil || stats.Predicates != 0 || stats.ScannedSpans != 0 || stats.PeakRetainedHits != 0 {
			t.Fatalf("point preparation scanned corpus: stats=%+v error=%v", stats, err)
		}
		wantTrace := []SpanHit{{ID: s.ID, Start: s.Start}}
		paired := Generate(c, ordinal+1)
		wantTrace = append(wantTrace, SpanHit{ID: paired.ID, Start: paired.Start})
		sort.Slice(wantTrace, func(i, j int) bool {
			if wantTrace[i].Start == wantTrace[j].Start {
				return wantTrace[i].ID < wantTrace[j].ID
			}
			return wantTrace[i].Start < wantTrace[j].Start
		})
		wantSpan := []SpanMatch{{Trace: s.Trace, ID: s.ID, Start: s.Start}}
		if !reflect.DeepEqual(cases[0].expectedTrace, wantTrace) || !reflect.DeepEqual(cases[1].expectedSpans, wantSpan) || len(cases[2].expectedTrace) != 0 || len(cases[3].expectedSpans) != 0 {
			t.Fatalf("point answers wrong at ordinal=%d: %+v", ordinal, cases)
		}
	}
}

func TestPreparedOracleEmptyAbsentAndOffTenantPopulations(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Count, c.Tenants, c.Tenant, c.K = 47, 3, 2, 2
	cases := []queryCase{
		{kind: "attribute", attribute: Attribute{"resource", QueryKey, "string", QueryValue}},
		{kind: "attribute", attribute: Attribute{"span", QueryKey, "string", "absent_query_v1"}},
		{kind: "time", interval: TimeRange{Start: 0, End: 0}},
	}
	stats, err := prepareQueryOracles(t.Context(), c, cases)
	if err != nil || stats.Predicates != 3 || stats.ScannedSpans != c.Count || stats.PeakRetainedHits != c.K {
		t.Fatalf("off-tenant/absent census: stats=%+v error=%v", stats, err)
	}
	if !reflect.DeepEqual(cases[0].expected, bruteAttributeOracle(c, cases[0].attribute)) || len(cases[0].expected) != c.K || !reflect.DeepEqual(cases[1].expected, []Hit{}) || !reflect.DeepEqual(cases[2].expected, []Hit{}) {
		t.Fatalf("off-tenant/absent answers: %+v", cases)
	}
	c.Count = 100000000
	stats, err = prepareQueryOracles(t.Context(), c, nil)
	if err != nil || stats != (oracleStats{}) {
		t.Fatalf("empty request scanned population: stats=%+v error=%v", stats, err)
	}
}

func TestPreparedPointOraclesOddSingleton(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 3, 47} {
		c := evidenceConfig()
		c.Count = count
		c.Tenants = min(2, (count+1)/2)
		last := Generate(c, count-1)
		c.Tenant = last.Tenant
		cases := []queryCase{
			{kind: "trace", trace: last.Trace, sourceOrdinal: last.Ordinal},
			{kind: "span", span: last.ID, sourceOrdinal: last.Ordinal},
		}
		stats, err := prepareQueryOracles(t.Context(), c, cases)
		if err != nil || stats != (oracleStats{}) || !reflect.DeepEqual(cases[0].expectedTrace, []SpanHit{{ID: last.ID, Start: last.Start}}) || !reflect.DeepEqual(cases[1].expectedSpans, []SpanMatch{{Trace: last.Trace, ID: last.ID, Start: last.Start}}) {
			t.Fatalf("odd count=%d: stats=%+v cases=%+v error=%v", count, stats, cases, err)
		}
	}
}

func TestOracleHeapLiteralTiesAndReplacement(t *testing.T) {
	t.Parallel()
	h := oracleHeap{}
	for i, hit := range []Hit{{Trace: "b", Latest: 8}, {Trace: "a", Latest: 8}, {Trace: "c", Latest: 8}, {Trace: "z", Latest: 4}, {Trace: "d", Latest: 9}} {
		grew := retainOracleHit(&h, hit, 2)
		if grew != (i < 2) || h.Len() != min(i+1, 2) {
			t.Fatalf("candidate %d: grew=%t retained=%d", i, grew, h.Len())
		}
	}
	got, err := finishOracleHeap(t.Context(), &h)
	want := []Hit{{Trace: "d", Latest: 9}, {Trace: "c", Latest: 8}}
	if err != nil || !reflect.DeepEqual(got, want) || cap(got) != 2 || h.Len() != 0 {
		t.Fatalf("literal tie/replacement result: got=%+v cap=%d remaining=%d want=%+v error=%v", got, cap(got), h.Len(), want, err)
	}
}

func TestTimeRangeValidation(t *testing.T) {
	t.Parallel()
	for _, interval := range []TimeRange{{Start: 0, End: 0}, {Start: 1, End: 1}, {Start: -1, End: 0}, {Start: math.MinInt64, End: math.MaxInt64}} {
		if err := interval.Validate(); err != nil {
			t.Fatalf("valid interval %+v: %v", interval, err)
		}
	}
	for _, interval := range []TimeRange{{Start: 1, End: 0}, {Start: math.MaxInt64, End: math.MinInt64}} {
		var config *ConfigError
		if err := interval.Validate(); !errors.As(err, &config) {
			t.Fatalf("reversed interval %+v: expected ConfigError, got %v", interval, err)
		}
	}
}

func TestPreparedOracleCancellation(t *testing.T) {
	t.Parallel()
	for _, checks := range []int{1, 2, 4} {
		c := evidenceConfig()
		cases := []queryCase{{kind: "attribute", attribute: Attribute{"resource", QueryKey, "string", QueryValue}}}
		ctx := &evidenceCancelContext{Context: context.Background(), remaining: checks}
		_, err := prepareQueryOracles(ctx, c, cases)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preparation canceled at check %d: %v", checks, err)
		}
	}
}
