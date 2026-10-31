package embedded

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// customCandidacyIndexType is a custom maintainer's type (the OTel posting
// index's): its entries are hashed postings, not (key, pk) tuples.
const customCandidacyIndexType = "otel_typed_scoped_eq_v1"

// customCandidacyMetaData is a table with a fan-out index T_TAGS, a scalar
// index T_B and a VALUE index T_A. T_TAGS and T_B get indexType; with
// recordlayer.IndexTypeValue it is the control twin.
func customCandidacyMetaData(t *testing.T, indexType string) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("custom_candidacy")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
		metadata.NewColumnSpec("A", api.NewLongType(true), 3),
		metadata.NewColumnSpec("B", api.NewLongType(true), 4),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	b.AddIndex("T", "T_A", []string{"A"}, false)
	b.AddIndex("T", "T_B", []string{"B"}, false)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	for _, name := range []string{"T_TAGS", "T_B"} {
		idx := md.GetIndex(name)
		if idx == nil || idx.Type != recordlayer.IndexTypeValue {
			t.Fatalf("schema lacks VALUE index %s: %v", name, idx)
		}
		idx.Type = indexType
	}
	return md
}

func planUsedIndexes(t *testing.T, query string, md *recordlayer.RecordMetaData) ([]string, string) {
	t.Helper()
	plan, err := PlanRecordQueryWithMetadata(query, md, properties.FixedStatistics{Cardinality: 1_000_000})
	if err != nil {
		t.Fatalf("%s: planning failed: %v", query, err)
	}
	used := map[string]struct{}{}
	collectDependedOnIndexNames(plan, used)
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, plan.Explain()
}

// A custom index type must not become a VALUE-scan candidate: a VALUE scan
// would decode its postings as (key, pk) tuples and return wrong rows. With
// the type custom, each query still plans, over the primary scan or the VALUE
// index T_A.
func TestCustomIndexTypeIsNotAValueScanCandidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		// control is the index set planned when T_TAGS and T_B are VALUE
		// indexes; custom the set when they have the custom type.
		control, custom []string
		// customExplain must appear in the custom plan.
		customExplain string
	}{
		{"SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)", []string{"T_TAGS"}, nil, "Explode"},
		{"SELECT R.ID FROM T AS R WHERE R.B = 3", []string{"T_B"}, nil, ""},
		{"SELECT R.ID, R.B FROM T AS R ORDER BY R.B", []string{"T_B"}, nil, "InMemorySort"},
		{"SELECT R.ID FROM T AS R WHERE R.A = 3", []string{"T_A"}, []string{"T_A"}, ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			got, plan := planUsedIndexes(t, tc.query, customCandidacyMetaData(t, recordlayer.IndexTypeValue))
			if !equalStrings(got, tc.control) {
				t.Fatalf("control: the VALUE twin uses %v, want %v, so the custom case observes nothing\n  plan: %s", got, tc.control, plan)
			}
			got, plan = planUsedIndexes(t, tc.query, customCandidacyMetaData(t, customCandidacyIndexType))
			for _, name := range got {
				if name != "T_A" {
					t.Fatalf("with T_TAGS and T_B of type %s the plan scans %s as a VALUE index\n  plan: %s", customCandidacyIndexType, name, plan)
				}
			}
			if tc.custom != nil && !equalStrings(got, tc.custom) {
				t.Fatalf("custom: the plan uses %v, want %v\n  plan: %s", got, tc.custom, plan)
			}
			if !strings.Contains(plan, tc.customExplain) {
				t.Fatalf("custom: the plan lacks %q\n  plan: %s", tc.customExplain, plan)
			}
		})
	}
}

// The candidate set itself: the custom-typed indexes get none, whatever their
// root, while the VALUE index keeps its own.
func TestCustomIndexTypeBuildsNoMatchCandidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		indexType  string
		wantCustom bool
	}{
		{recordlayer.IndexTypeValue, true},
		{customCandidacyIndexType, false},
	} {
		pc := &metadataPlanContext{md: customCandidacyMetaData(t, tc.indexType)}
		names := map[string]bool{}
		for _, candidate := range pc.buildMatchCandidates() {
			names[candidate.CandidateName()] = true
		}
		if !names["T_A"] {
			t.Fatalf("%s: VALUE index T_A lost its candidate: %v", tc.indexType, names)
		}
		for _, name := range []string{"T_TAGS", "T_B"} {
			if names[name] != tc.wantCustom {
				t.Fatalf("%s: %s candidate = %v, want %v (candidates %v)", tc.indexType, name, names[name], tc.wantCustom, names)
			}
		}
	}
}

// Only the types whose Java factories expand a value-index candidate opt in.
func TestValueScanCandidateIndexTypes(t *testing.T) {
	t.Parallel()
	for indexType, want := range map[string]bool{
		recordlayer.IndexTypeValue:                 true,
		recordlayer.IndexTypeVersion:               true,
		recordlayer.IndexTypeRank:                  true,
		recordlayer.IndexTypePermutedMin:           true,
		recordlayer.IndexTypePermutedMax:           true,
		recordlayer.IndexTypeText:                  false,
		recordlayer.IndexTypeTimeWindowLeaderboard: false,
		recordlayer.IndexTypeMultidimensional:      false,
		recordlayer.IndexTypeVector:                false,
		recordlayer.IndexTypeVectorSPFresh:         false,
		recordlayer.IndexTypeBitmapValue:           false,
		recordlayer.IndexTypeCount:                 false,
		customCandidacyIndexType:                   false,
		"":                                         false,
	} {
		if got := valueScanCandidateIndexType(indexType); got != want {
			t.Errorf("valueScanCandidateIndexType(%q) = %v, want %v", indexType, got, want)
		}
	}
}
