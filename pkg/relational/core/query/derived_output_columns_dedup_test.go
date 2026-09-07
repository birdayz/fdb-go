package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A derived leg's ordinal layout describes the row its projection EMITS, and
// that row names a repeated output by the name-addressability suffix
// (values.DedupFieldNames): `SELECT g, SUM(v) AS g` flows [G G_2]. The layout
// stated the names verbatim, [G G], which is a different ordinal domain from
// the runtime row; the seed's baked read of slot 1 then declined the ordinal
// and fell back to a by-name read that answered the FIRST G — `SELECT *` over
// that leg beside another table returned the grouping key in both G columns.
// Pinned against the rule itself, so the two cannot drift apart again.
func TestDerivedOutputColumnsNameARepeatedOutputAsTheRuntimeRowDoes(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{}
	for _, tc := range []struct {
		name        string
		projections []string
		aliases     []string
		want        []string
	}{
		{"repeated alias beside the bare key", []string{"G", "SUM(V)"}, []string{"", "G"}, []string{"G", "G_2"}},
		{"three of a kind", []string{"G", "G", "G"}, nil, []string{"G", "G_2", "G_3"}},
		{"distinct names are untouched", []string{"G", "SUM(V)"}, []string{"", "S"}, []string{"G", "S"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tr.derivedOutputColumns(&logical.LogicalProject{
				Projections: tc.projections,
				Aliases:     tc.aliases,
			})
			if len(got) != len(tc.want) {
				t.Fatalf("derivedOutputColumns returned %d columns, want %d", len(got), len(tc.want))
			}
			names := make([]string, len(got))
			for i, f := range got {
				names[i] = f.Name
			}
			want := values.DedupFieldNames(tc.want)
			for i := range names {
				if names[i] != tc.want[i] || names[i] != want[i] {
					t.Fatalf("layout names = %v, want %v (values.DedupFieldNames agrees: %v)", names, tc.want, want)
				}
			}
		})
	}
}
