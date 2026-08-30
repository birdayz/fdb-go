package factory

import (
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// The eligibility census, pinned.
//
// factory.Candidates drops every query the TLP oracle cannot partition, and
// that filter — not the generator, not the quota — is what decides which query
// shapes the committed corpus can EVER contain. Nothing measured it, so the
// corpus grew for a month with zero GROUP BY, zero UNION, zero derived table,
// zero DISTINCT and zero LIMIT scenarios while every batch reported a clean
// funnel.
//
// The numbers below are a measurement over a FIXED seed window, not a target.
// rowdiff.Generate is seeded and deterministic, so they are reproducible
// exactly; they change when the generator changes, and that is the point —
// this test is what makes a change to the generator's shape mix visible
// instead of silent.
const (
	censusSeeds       = 4000
	censusTotalQuerie = 23891
	censusEligible    = 11403
)

// wantIneligible is the multi-label class census: a query can be refused for
// several reasons at once, so these SUM TO MORE than the ineligible count.
// Written multi-label deliberately — the single-label form (a switch returning
// the first match) reports `union 0` and `derived-table 0` over this same
// window, because `no-WHERE` is tested first and masks them. That reading is
// what makes an absent class look like an absent feature.
var wantIneligible = map[string]int{
	"no-WHERE":      5315,
	"aggregate":     4106,
	"limit":         3689,
	"union":         1967,
	"distinct":      1685,
	"derived-table": 1678,
	"offset":        353,
}

// allIneligible reports EVERY reason a query is TLP-ineligible.
//
// IT IS A DESCRIPTION, NOT THE DECISION. tlpEligible (candidate.go) is the
// production filter and the only thing that decides what the corpus can hold;
// this function exists solely to break its verdict into named classes, because
// tlpEligible returns one bool and "52.3% refused" is not actionable without
// knowing which 52.3%.
//
// The two are tied together by an assertion in the census loop below, and that
// tie is the whole point. Without it this is a hand-copied replica: deleting
// the `q.Union != nil` arm from the PRODUCTION filter left the census GREEN,
// so the "mutation-verified" claim made for this test had verified the test's
// own copy of the rule. A census that pins a replica pins nothing.
func allIneligible(q rowdiff.Query) []string {
	var out []string
	if q.Where == nil {
		out = append(out, "no-WHERE")
	}
	if q.Agg != nil {
		out = append(out, "aggregate")
	}
	if q.Union != nil {
		out = append(out, "union")
	}
	if q.Derived != nil {
		out = append(out, "derived-table")
	}
	if q.Limit > 0 {
		out = append(out, "limit")
	}
	if q.Offset > 0 {
		out = append(out, "offset")
	}
	if q.Distinct {
		out = append(out, "distinct")
	}
	return out
}

func TestEligibilityCensus(t *testing.T) {
	t.Parallel()
	reasons := map[string]int{}
	total, eligible := 0, 0
	for seed := uint64(1); seed <= censusSeeds; seed++ {
		c := rowdiff.Generate(seed)
		for _, q := range c.Queries {
			total++
			// THE PRODUCTION FILTER decides. tlpEligible is what
			// factory.Candidates actually calls, so it is what this census
			// must count — anything else measures a copy.
			elig := tlpEligible(q)
			if elig {
				eligible++
			}
			cls := allIneligible(q)
			// And the class breakdown must AGREE with it. This is what makes
			// the breakdown a description of the real filter rather than a
			// second implementation of it: mutate tlpEligible and this fires
			// on the first query where the two disagree.
			if elig != (len(cls) == 0) {
				t.Fatalf("query %d: tlpEligible=%v but the class breakdown says %v — the census's "+
					"named classes have drifted from the production filter, so every count below "+
					"describes a replica", total, elig, cls)
			}
			// WHAT THIS TIE CANNOT CATCH, stated because discovering it later
			// would look like the tie failing. It detects a mutation only where
			// the mutated arm is the SOLE reason some query is ineligible.
			// Removing `q.Union != nil` from tlpEligible leaves this test green:
			// over seeds 1..4000 every union query the generator emits is
			// already ineligible for another reason (it carries no WHERE, or an
			// aggregate), so the arm is never the deciding one and deleting it
			// changes no verdict. Removing `q.Distinct` DOES fire, immediately.
			//
			// That is a fact about the generator's shape mix, not a hole in the
			// tie — but it means "mutation-verified" for this test is a claim
			// about the arms that are reachable, and `union` is not one of them.
			for _, r := range cls {
				reasons[r]++
			}
		}
	}

	var keys []string
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("ELIGIBILITY %-16s %6d (%.1f%%)\n", k, reasons[k], 100*float64(reasons[k])/float64(total))
	}
	fmt.Printf("ELIGIBILITY total=%d eligible=%d (%.1f%%) excluded=%d (%.1f%%)\n",
		total, eligible, 100*float64(eligible)/float64(total),
		total-eligible, 100*float64(total-eligible)/float64(total))

	if total != censusTotalQuerie {
		t.Errorf("generated queries over seeds 1..%d = %d, want %d — the GENERATOR changed; "+
			"re-measure and update the census rather than relaxing it", censusSeeds, total, censusTotalQuerie)
	}
	if eligible != censusEligible {
		t.Errorf("TLP-eligible = %d, want %d — the ELIGIBILITY FILTER changed. If a shape class was "+
			"admitted, that is the intended direction and the census moves with it; if one was newly "+
			"excluded, the corpus just lost a query family", eligible, censusEligible)
	}
	for cls, want := range wantIneligible {
		if reasons[cls] != want {
			t.Errorf("ineligible[%s] = %d, want %d", cls, reasons[cls], want)
		}
	}
	// Guard the instrument itself: a class silently dropping to zero is how a
	// whole family of excluded shapes would stop being counted, and a zero here
	// reads exactly like "the generator never emits this".
	for cls := range wantIneligible {
		if reasons[cls] == 0 {
			t.Errorf("ineligible[%s] = 0 — either the generator stopped emitting the shape or the "+
				"classifier stopped recognising it; both are silent coverage losses", cls)
		}
	}
	// And guard the direction that matters most: the excluded share is the
	// headline number this whole census exists to keep honest.
	if excluded := total - eligible; excluded*100/total < 40 {
		t.Errorf("excluded share fell to %d%% — if the factory learned to bless more shapes this is "+
			"good news and the pins move; it must not move silently", excluded*100/total)
	}
}
