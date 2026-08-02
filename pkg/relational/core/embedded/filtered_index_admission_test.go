package embedded

import (
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestMetadataPlanContextExcludesFilteredIndexesBeforeEveryCandidateFactory(t *testing.T) {
	t.Parallel()
	builder := metadata.NewSchemaTemplateBuilder().SetName("filtered_candidates")
	builder.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("STATUS", api.NewStringType(false), 2),
		metadata.NewColumnSpec("AMOUNT", api.NewLongType(false), 3),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 4),
	}, []string{"ID"})
	builder.AddIndex("T", "VALUE_FILTERED", []string{"STATUS"}, true)
	builder.AddIndex("T", "VALUE_PLAIN", []string{"AMOUNT"}, false)
	builder.AddAggregateIndex("T", "AGG_FILTERED", []string{"STATUS"}, "SUM", "AMOUNT")
	builder.AddAggregateIndex("T", "AGG_PLAIN", []string{"AMOUNT"}, "COUNT", "")
	builder.AddVectorIndexUsing("HNSW", "T", "VECTOR_FILTERED", "EMBEDDING", nil,
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	builder.AddVectorIndexUsing("HNSW", "T", "VECTOR_PLAIN", "EMBEDDING", []string{"STATUS"},
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := builder.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	for _, name := range []string{"VALUE_FILTERED", "AGG_FILTERED", "VECTOR_FILTERED"} {
		idx := md.GetIndex(name)
		if idx == nil {
			t.Fatalf("index %s missing", name)
		}
		if err := idx.SetPredicateProto(&gen.Predicate{
			ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_TRUE.Enum(),
			},
		}); err != nil {
			t.Fatalf("SetPredicateProto(%s): %v", name, err)
		}
		if !idx.HasPredicate() {
			t.Fatalf("index %s did not retain predicate", name)
		}
	}

	ctx := buildCascadesPlanContext(md, cascades.DefaultPlannerConfiguration())
	got := make(map[string]bool)
	for _, candidate := range ctx.GetMatchCandidates() {
		name := candidate.CandidateName()
		if strings.HasPrefix(name, "primary(") {
			continue
		}
		if strings.HasSuffix(name, "_FILTERED") {
			t.Fatalf("filtered %T candidate %q was admitted", candidate, name)
		}
		got[name] = true
	}
	for _, want := range []string{"VALUE_PLAIN", "AGG_PLAIN", "VECTOR_PLAIN"} {
		if !got[want] {
			t.Errorf("unfiltered %s candidate was not admitted; got %v", want, got)
		}
	}
}
