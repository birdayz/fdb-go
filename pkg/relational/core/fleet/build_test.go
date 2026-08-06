package fleet

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// TestSelectIndexesNarrowsToTheRequestedRollout pins the two properties an
// index roll-out depends on. An empty request must select EVERYTHING pending —
// otherwise "build whatever this tenant owes" silently becomes "build
// nothing". A non-empty request must select only what was asked, so a tenant
// carrying an unrelated half-built index is not dragged into someone else's
// roll-out.
func TestSelectIndexesNarrowsToTheRequestedRollout(t *testing.T) {
	t.Parallel()
	pending := []*recordlayer.Index{
		{Name: "T_BY_C"},
		{Name: "T_BY_V"},
		{Name: "UNRELATED_HALF_BUILT"},
	}

	if got := selectIndexes(pending, nil); len(got) != 3 {
		t.Fatalf("empty request selected %d indexes, want all 3 — an unfiltered fleet build "+
			"must build everything pending", len(got))
	}

	got := selectIndexes(pending, []string{"T_BY_C"})
	if len(got) != 1 || got[0].Name != "T_BY_C" {
		t.Fatalf("selectIndexes([T_BY_C]) = %v, want exactly [T_BY_C] — a targeted roll-out "+
			"must not touch a tenant's unrelated pending indexes", indexNames(got))
	}

	// Index identifiers are case-insensitive throughout the relational layer;
	// a roll-out typed in lower case must still match the stored name.
	if got := selectIndexes(pending, []string{"t_by_v"}); len(got) != 1 || got[0].Name != "T_BY_V" {
		t.Fatalf("selectIndexes([t_by_v]) = %v, want [T_BY_V]", indexNames(got))
	}

	// A name no tenant carries selects nothing, and that is NOT an error:
	// tenants sit at different template versions mid-migration.
	if got := selectIndexes(pending, []string{"NOT_ON_THIS_TENANT"}); len(got) != 0 {
		t.Fatalf("selectIndexes of an absent index = %v, want empty", indexNames(got))
	}
}

func indexNames(idx []*recordlayer.Index) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, i.Name)
	}
	return out
}
