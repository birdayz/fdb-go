package values

import (
	"strings"
	"testing"
)

// The name-split census exists to make two different failures loud, and a census
// whose own assertion cannot fail is the fake-green shape it was written to end.
// These tests drive the decision function directly over constructed counts, so
// each direction is exercised without a corpus run and without touching the
// process-global counters.

// TestNameSplitCensus_ForgedQualifierIsRed pins the HARD ZERO.
//
// The direction: a rendered name arrives at a splitting arm carrying a dot, the
// arm slices it, and a qualifier that no parse tree ever stated comes into
// existence. Over the real-FDB corpus this population is 0 at both arms; if this
// assertion cannot go red on a forged one, that 0 means nothing.
func TestNameSplitCensus_ForgedQualifierIsRed(t *testing.T) {
	t.Parallel()

	for _, site := range []NameSplitSite{NameSplitSiteLegQOVSegmentsOf, NameSplitSiteFlatColumnBake} {
		var counts [nameSplitSiteCount][nameSplitClassCount]int
		// A healthy population at both arms, so the failure below is the forged
		// qualifier and not an incidental floor breach.
		counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
		counts[NameSplitSiteFlatColumnBake][NameSplitBare] = 2
		counts[site][NameSplitQualified] = 1

		// NO floors at all, so nothing but the hard zero can produce the red.
		// The forged qualifier lands in a SPLIT bucket, so under the measured
		// floors it would ALSO trip legQOVSegmentsOf's zero-declaration check —
		// and a test that passes for two reasons pins neither.
		var sb strings.Builder
		if failed := assertNameSplitCounts(&sb, counts, []string{site.String() + ` "A.B"`}, nil); !failed {
			t.Fatalf("%s: one manufactured qualifier did not fail the census — the "+
				"SPLIT-QUALIFIED zero is unenforced and the re-split population is unguarded", site)
		}
		out := sb.String()
		if !strings.Contains(out, "SPLIT-QUALIFIED") || !strings.Contains(out, site.String()) {
			t.Fatalf("%s: failure text names neither the class nor the site: %s", site, out)
		}
		// The message must carry the star-body READING and Java's ruling on it,
		// because that is the reading under which a maintainer would otherwise
		// "fix" this by widening the zero. The message hands over the ANSWER —
		// Java resolves a nameless projection output by no spelling, so the arm
		// declines — rather than telling the tripper to go and ask.
		if !strings.Contains(out, "star-body") || !strings.Contains(out, "SemanticAnalyzer.java:459-461") {
			t.Fatalf("%s: failure text does not carry the star-body reading and Java's "+
				"unnamed-output ruling: %s", site, out)
		}
		if !strings.Contains(out, "DO NOT WIDEN THIS ZERO") || !strings.Contains(out, "DECLINE") {
			t.Fatalf("%s: failure text does not tell the tripper what to DO. A gate that "+
				"describes a settled question without stating its answer gets closed by "+
				"widening the zero, which is the one fix Java rules out: %s", site, out)
		}
	}
}

// TestNameSplitCensus_DarkArmIsRed pins the FLOORS.
//
// The direction that matters more than the zero: both arms are nearly dark
// already (9 and 2 calls over the whole corpus), so SPLIT-QUALIFIED == 0 is
// nearly vacuous on its own. If the shapes driving these arms stop being
// planned, or a recorder is dropped from an arm, the zero stays 0 and the gate
// stays green while measuring nothing at all.
func TestNameSplitCensus_DarkArmIsRed(t *testing.T) {
	t.Parallel()

	for _, dark := range []NameSplitSite{NameSplitSiteLegQOVSegmentsOf, NameSplitSiteFlatColumnBake} {
		var counts [nameSplitSiteCount][nameSplitClassCount]int
		counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
		counts[NameSplitSiteFlatColumnBake][NameSplitBare] = 2
		// The arm goes silent. No qualifier is forged — the hard zero still holds.
		counts[dark] = [nameSplitClassCount]int{}

		floors := measuredFloors()

		var sb strings.Builder
		if failed := assertNameSplitCounts(&sb, counts, nil, &floors); !failed {
			t.Fatalf("%s went dark and the census stayed green — a SPLIT-QUALIFIED zero "+
				"over an empty population reads exactly like a channel measured clean", dark)
		}
		if out := sb.String(); !strings.Contains(out, "DARK") {
			t.Fatalf("%s: failure text does not name the disappearance: %s", dark, out)
		}
	}
}

// measuredFloors is the shape the sqldriver TestMain declares, kept in one place
// so every direction below is exercised against the floors production actually
// runs — including the ZERO declaration, which is the part most easily made
// vacuous by a test that invents its own healthier numbers.
func measuredFloors() NameSplitFloors {
	var f NameSplitFloors
	f.Calls[NameSplitSiteLegQOVSegmentsOf] = 1 // measured 9
	f.Calls[NameSplitSiteFlatColumnBake] = 1   // measured 2
	f.Split[NameSplitSiteFlatColumnBake] = 1   // measured 2 (splitBare)
	f.Split[NameSplitSiteLegQOVSegmentsOf] = 0 // measured 0 — WATCHED, NOT PROVEN
	return f
}

// TestNameSplitCensus_SplitArmGoingDarkIsRed pins the per-CLASS floor, which is
// the one the total-calls floor cannot stand in for.
//
// flatColumnBake's 2 calls are BOTH splits, so its Calls floor and its Split
// floor happen to move together. legQOVSegmentsOf's do not: 9 calls, 0 splits.
// The direction here is the one that separates them — an arm whose SEGMENTED
// traffic is healthy while its splitting arm stops being reached or loses its
// recorder. Under a Calls floor alone that reads green.
func TestNameSplitCensus_SplitArmGoingDarkIsRed(t *testing.T) {
	t.Parallel()

	var counts [nameSplitSiteCount][nameSplitClassCount]int
	counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
	// flatColumnBake keeps a healthy CALL total, entirely on segmented traffic:
	// its splitting arm has gone dark. bakeSegmentedColumnRef is a different
	// function today so this site reports segmented 0 by construction — the
	// point of the shape is that the Calls floor cannot tell the difference.
	counts[NameSplitSiteFlatColumnBake][NameSplitSegmented] = 2

	floors := measuredFloors()

	var sb strings.Builder
	if failed := assertNameSplitCounts(&sb, counts, nil, &floors); !failed {
		t.Fatal("flatColumnBake's splitting arm went dark behind a healthy call total and " +
			"the census stayed green — the Calls floor is measuring the segmented channel, " +
			"and this census's hard zero is a zero over the SPLIT population")
	}
	if out := sb.String(); !strings.Contains(out, "SPLITTING arm") {
		t.Fatalf("failure text does not name the split population: %s", out)
	}
}

// TestNameSplitCensus_StaleZeroDeclarationIsRed pins the OTHER direction of the
// zero declaration, and it is the one that keeps the label honest.
//
// legQOVSegmentsOf's split floor is 0. That is not "no floor" — it is a claim:
// this arm's splitting paths are measured EMPTY over the corpus, so the corpus
// cannot guard their recorder wiring and a unit pin does it instead. The arm is
// live (a panic there is reached, with a dotted name), so the day the corpus
// starts driving it, that claim is stale and the unit pin is no longer the only
// coverage available. A declaration nobody re-reads is how a placeholder becomes
// a permanent exemption.
func TestNameSplitCensus_StaleZeroDeclarationIsRed(t *testing.T) {
	t.Parallel()

	var counts [nameSplitSiteCount][nameSplitClassCount]int
	counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
	// The corpus now drives the arm — bare, so no debt is forged and the hard
	// zero still holds. The ONLY thing that may go red here is the declaration.
	counts[NameSplitSiteLegQOVSegmentsOf][NameSplitBare] = 1
	counts[NameSplitSiteFlatColumnBake][NameSplitBare] = 2

	floors := measuredFloors()

	var sb strings.Builder
	if failed := assertNameSplitCounts(&sb, counts, nil, &floors); !failed {
		t.Fatal("legQOVSegmentsOf's splitting arm acquired a population and its " +
			"floor-of-0 declaration stayed green — 'watched, not proven' would then " +
			"outlive the measurement that made it honest, and the site would keep a " +
			"permanent exemption from the floor its sibling carries")
	}
	if out := sb.String(); !strings.Contains(out, "WATCHED, NOT PROVEN") {
		t.Fatalf("failure text does not name the declaration going stale: %s", out)
	}
}

// TestNameSplitCensus_MeasuredShapeIsGreen pins the CURRENT reading, so the two
// reds above cannot be satisfied by an assertion that simply always fails.
func TestNameSplitCensus_MeasuredShapeIsGreen(t *testing.T) {
	t.Parallel()

	var counts [nameSplitSiteCount][nameSplitClassCount]int
	counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
	counts[NameSplitSiteFlatColumnBake][NameSplitBare] = 2

	floors := measuredFloors()

	var sb strings.Builder
	if failed := assertNameSplitCounts(&sb, counts, nil, &floors); failed {
		t.Fatalf("the measured corpus shape must pass: %s", sb.String())
	}
}

// TestNameSplitCensus_RecorderTracksTheGate pins that the recorder is gated by
// LegIdentityCensusEnabled — off, it must cost the production path nothing; on,
// it must actually count, because a recorder that no-ops under its own gate is
// how an instrument reports a clean zero from a site it never watched.
//
// Asserted in whichever direction this binary is built for rather than skipped,
// so the test is a real check under both.
func TestNameSplitCensus_RecorderTracksTheGate(t *testing.T) {
	t.Parallel()

	before, _ := NameSplitCensus()
	RecordNameSplit(NameSplitSiteFlatColumnBake, NameSplitQualified, "A.B")
	after, _ := NameSplitCensus()

	moved := before != after
	if enabled := LegIdentityCensusEnabled(); moved != enabled {
		t.Fatalf("census gate is %t but the recorder %s — the recorder must track its gate "+
			"in both directions, or a zero from this site says nothing about the traffic",
			enabled, map[bool]string{true: "counted", false: "did not count"}[moved])
	}
}
