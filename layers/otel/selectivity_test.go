package otel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func selectivitySourcePopulation(c Config, attributes []Attribute) map[Attribute]int {
	counts := make(map[Attribute]int, len(attributes))
	for _, attribute := range attributes {
		counts[attribute] = 0
	}
	for ordinal := 0; ordinal < c.Count; ordinal++ {
		source := Generate(c, ordinal)
		if source.Tenant != c.Tenant {
			continue
		}
		for _, attribute := range source.Attributes {
			if _, requested := counts[attribute]; requested {
				counts[attribute]++
			}
		}
	}
	return counts
}

func TestSelectiveV2SourcePopulationAndDefaultBudgets(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Run, c.Count, c.Tenants, c.Iterations = "selectivity_fixture_v2", 100000, 1, 2
	selective, err := queryCases(t.Context(), c, "selective")
	if err != nil || len(selective) != 2 {
		t.Fatalf("selective preparation: cases=%d error=%v", len(selective), err)
	}
	mixed, err := queryCases(t.Context(), c, "mixed")
	if err != nil || len(mixed) != 2 {
		t.Fatalf("mixed preparation: cases=%d error=%v", len(mixed), err)
	}
	genaiConfig := c
	genaiConfig.Iterations = 7
	genai, err := queryCases(t.Context(), genaiConfig, "genai")
	if err != nil || len(genai) != 7 {
		t.Fatalf("GenAI preparation: cases=%d error=%v", len(genai), err)
	}
	varying, resource, provider := selective[1], mixed[1], genai[6]
	source := Generate(c, varying.sourceOrdinal)
	want := Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}
	if varying.class != "custom_varying" || varying.kind != "attribute" || varying.attribute != want || !reflect.DeepEqual(varying.expected, []Hit{{Trace: source.Trace, Latest: source.Start}}) {
		t.Fatalf("selective response needle did not preserve its source witness: %+v", varying)
	}
	if resource.class != "resource_common" || resource.kind != "attribute" || resource.attribute != (Attribute{"resource", QueryKey, "string", QueryValue}) || provider.class != "genai_provider" || provider.kind != "attribute" || provider.attribute != (Attribute{"span", "gen_ai.provider.name", "string", "openai"}) {
		t.Fatalf("explicit common workload classes changed: mixed=%+v provider=%+v", resource, provider)
	}
	population := selectivitySourcePopulation(c, []Attribute{varying.attribute, resource.attribute, provider.attribute})
	if population[varying.attribute] != 1 || population[resource.attribute] != c.Count || population[provider.attribute] != c.Count || population[resource.attribute] <= c.MaxCandidates || population[provider.attribute] <= c.MaxCandidates {
		t.Fatalf("source-span populations must distinguish selective/default-budget work from common terms: %+v default candidates=%d", population, c.MaxCandidates)
	}
	message, err := toRecord(source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := postings(subspace.Sub("selectivity_fixture_v2"), c.HashBytes, c.Order, &recordlayer.FDBStoredRecord[proto.Message]{Record: message})
	if err != nil {
		t.Fatal(err)
	}
	var candidates int
	var logicalBytes int64
	for _, entry := range entries {
		var value coveringValue
		if err := json.Unmarshal(entry.Value, &value); err != nil {
			t.Fatal(err)
		}
		if value.Attribute == varying.attribute {
			candidates++
			logicalBytes += int64(len(entry.Key) + len(entry.Value))
		}
	}
	if candidates != 1 || candidates > c.MaxCandidates || logicalBytes <= 0 || logicalBytes > c.MaxBytes {
		t.Fatalf("unique selective posting does not fit default work budgets: candidates=%d bytes=%d limits=%d/%d", candidates, logicalBytes, c.MaxCandidates, c.MaxBytes)
	}
}

func TestSelectiveAttributeBoundsUseHonestWorkloadClasses(t *testing.T) {
	t.Parallel()
	for _, attributes := range []int{4, 5, 6, 7, 8, 12, 128} {
		c := evidenceConfig()
		c.Count, c.Tenants, c.MatchEvery, c.Iterations, c.Attributes = 64, 1, 1, 12, attributes
		cases, err := queryCases(t.Context(), c, "selective")
		if err != nil || len(cases) != c.Iterations {
			t.Fatalf("attributes=%d preparation: cases=%d error=%v", attributes, len(cases), err)
		}
		for _, i := range []int{1, 6, 11} {
			q := cases[i]
			source := Generate(c, q.sourceOrdinal)
			if attributes == 4 {
				if q.class != "trace_varying" || q.kind != "trace" || q.trace != source.Trace || q.attribute != (Attribute{}) || !reflect.DeepEqual(q.expectedTrace, evidenceSpans(c, source.Trace)) || len(q.expectedTrace) != 2 {
					t.Fatalf("four-attribute fixture mislabeled common equality as varying: %+v", q)
				}
				continue
			}
			if q.class != "custom_varying" || q.kind != "attribute" {
				t.Fatalf("attributes=%d varying class/kind: %+v", attributes, q)
			}
			if attributes < 8 {
				if !strings.HasPrefix(q.attribute.Key, "custom.literal.") {
					t.Fatalf("attributes=%d did not retain an available varying filler: %+v", attributes, q)
				}
			} else if q.attribute != (Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}) {
				t.Fatalf("attributes=%d selected a common GenAI slot: %+v", attributes, q)
			}
			if population := selectivitySourcePopulation(c, []Attribute{q.attribute}); population[q.attribute] != 1 {
				t.Fatalf("attributes=%d source-span population=%d want 1 for bounded fixture", attributes, population[q.attribute])
			}
		}
		mixed, err := queryCases(t.Context(), c, "mixed")
		if err != nil || mixed[1].class != "resource_common" || mixed[1].kind != "attribute" || mixed[1].attribute != (Attribute{"resource", QueryKey, "string", QueryValue}) {
			t.Fatalf("attributes=%d common mixed class changed: %+v %v", attributes, mixed, err)
		}
	}
}

func TestSelectiveFourAttributesDispatchesTraceFallback(t *testing.T) {
	t.Parallel()
	c := evidenceConfig()
	c.Attributes, c.MatchEvery, c.Iterations = 4, 1, 5
	ctx := t.Context()
	b := &evidenceBackend{cfg: c, mode: "success", ctx: ctx}
	var e Evidence
	if err := MeasureQueries(ctx, b, c, "selective", &e); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 5 || len(e.Samples) != 5 || b.calls[1].kind != "trace" || e.Samples[1].Class != "trace_varying" || e.Samples[1].Results != 2 || e.Failures != 0 {
		t.Fatalf("four-attribute fallback dispatch/evidence: calls=%+v evidence=%+v", b.calls, e)
	}
	if _, fabricated := e.Histograms["custom_varying"]; fabricated || e.Histograms["trace_varying"].Population != 1 {
		t.Fatalf("fallback retained a false custom/common histogram label: %+v", e.Histograms)
	}
}

func testOtelSelectiveV2(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.MatchEvery = 64, 1, 8, 1
	c.MaxCandidates, c.K, c.Iterations, c.Order = 2, 1, 2, order
	b := openOtelFixture(t, ctx, cluster, c, true)
	c = b.cfg
	selective, err := queryCases(ctx, c, "selective")
	if err != nil {
		t.Fatal(err)
	}
	q := selective[1]
	source := Generate(c, q.sourceOrdinal)
	if q.kind != "attribute" || q.class != "custom_varying" || q.attribute != (Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}) {
		t.Fatalf("real selective predicate is not the unique source response ID: %+v", q)
	}
	got, err := b.Query(ctx, q.attribute)
	want := []Hit{{Trace: source.Trace, Latest: source.Start}}
	if err != nil || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > c.MaxBytes {
		t.Fatalf("%s unique response query did not fit bounded work: got=%+v want=%+v error=%v", order, got, want, err)
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
		got, err := b.Query(ctx, common.attribute)
		if order == "trace" {
			var budget *QueryBudgetError
			if !errors.As(err, &budget) || got.Candidates != 3 || got.Bytes <= 0 {
				t.Fatalf("trace-order %s must exhaust the 2-candidate budget: got=%+v error=%v", common.class, got, err)
			}
		} else {
			want := bruteAttributeOracle(c, common.attribute)
			if err != nil || len(want) != 1 || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > c.MaxBytes {
				t.Fatalf("time-order %s must return the sound K=1 frontier: got=%+v want=%+v error=%v", common.class, got, want, err)
			}
		}
	}
}
