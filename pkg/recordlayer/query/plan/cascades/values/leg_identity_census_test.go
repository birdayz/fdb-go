package values

import "testing"

// The census is the instrument the leg-identity retyping rests on, so its own
// arithmetic is pinned here. An instrument that miscounts the FoldOnlyEqual
// population would report a vacuous zero and license the conversion on no
// evidence — which is the exact failure mode the census exists to prevent.
//
// This test does NOT call t.Parallel(): the counters are package-scoped and it
// asserts absolute totals, so it must own them for its duration. Every other test
// in this package leaves the census disabled (its default), so no sibling
// contributes; the FDB corpus pass that DOES enable it lives in another package
// and asserts only a zero, which extra traffic cannot falsely satisfy.
func TestLegIdentityCensus_ClassifiesExactFoldOnlyAndNeither(t *testing.T) {
	ResetLegIdentityCensus()
	SetLegIdentityCensusEnabled(true)
	t.Cleanup(func() {
		SetLegIdentityCensusEnabled(false)
		ResetLegIdentityCensus()
	})

	site := LegSiteRowLegsBinder
	// Exact.
	RecordLegIdentityComparison(site, "A", "A")
	RecordLegIdentityComparison(site, "q$5", "q$5")
	// Fold-only: the conversion delta. Both directions of the case disjointness
	// the planner deliberately maintains — a user alias upper-folded onto the
	// machine namespace, and the reverse.
	RecordLegIdentityComparison(site, "Q$5", "q$5")
	RecordLegIdentityComparison(site, "a", "A")
	// Neither.
	RecordLegIdentityComparison(site, "A", "B")

	c := LegIdentityCensusOf(site)
	if c.Total != 5 {
		t.Fatalf("Total = %d, want 5", c.Total)
	}
	if c.ExactEqual != 2 {
		t.Errorf("ExactEqual = %d, want 2", c.ExactEqual)
	}
	if c.FoldOnlyEqual != 2 {
		t.Errorf("FoldOnlyEqual = %d, want 2 — the population whose zero licenses the "+
			"folding-to-exact conversion must be counted, or its zero is vacuous", c.FoldOnlyEqual)
	}
	if c.Neither != 1 {
		t.Errorf("Neither = %d, want 1", c.Neither)
	}
	if len(c.FoldOnlySamples) != 2 {
		t.Fatalf("FoldOnlySamples = %v, want 2 witnesses — a nonzero count must NAME the "+
			"offending pair so the producer can be found", c.FoldOnlySamples)
	}
	want := map[string]bool{"Q$5 vs q$5": true, "a vs A": true}
	for _, s := range c.FoldOnlySamples {
		if !want[s] {
			t.Errorf("unexpected witness %q, want one of %v", s, want)
		}
	}

	// Sites are INDEPENDENT populations: a fold-only pair at one site must not
	// contaminate another's zero, or a per-site disposition could not be made.
	if other := LegIdentityCensusOf(LegSiteBuriedLegWindow); other.Total != 0 {
		t.Errorf("sibling site total = %d, want 0 — site populations must not merge", other.Total)
	}
}

// The disabled census must record NOTHING. The gate is what keeps an atomic
// increment out of the per-row executor path, so a leak here is both a
// measurement bug and a hot-path regression.
func TestLegIdentityCensus_DisabledRecordsNothing(t *testing.T) {
	ResetLegIdentityCensus()
	t.Cleanup(ResetLegIdentityCensus)

	if LegIdentityCensusEnabled() {
		t.Fatal("census must default to DISABLED — an always-on counter puts a " +
			"contended atomic in the row loop")
	}
	// Callers guard on LegIdentityCensusEnabled(); this asserts the guard's
	// premise, i.e. that the default really is off for every production path.
	for _, site := range LegIdentitySites() {
		if c := LegIdentityCensusOf(site); c.Total != 0 {
			t.Errorf("site %s recorded %d comparisons while disabled", site, c.Total)
		}
	}
}
