package otel

import (
	"reflect"
	"strings"
	"testing"
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
