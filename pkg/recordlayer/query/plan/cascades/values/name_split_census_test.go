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

		var floors NameSplitFloors
		floors.Calls[NameSplitSiteLegQOVSegmentsOf] = 1
		floors.Calls[NameSplitSiteFlatColumnBake] = 1

		var sb strings.Builder
		if failed := assertNameSplitCounts(&sb, counts, []string{site.String() + ` "A.B"`}, &floors); !failed {
			t.Fatalf("%s: one manufactured qualifier did not fail the census — the "+
				"SPLIT-QUALIFIED zero is unenforced and the re-split population is unguarded", site)
		}
		out := sb.String()
		if !strings.Contains(out, "SPLIT-QUALIFIED") || !strings.Contains(out, site.String()) {
			t.Fatalf("%s: failure text names neither the class nor the site: %s", site, out)
		}
		// The message must carry the star-body BOUNDARY decision, because that is
		// the reading under which a maintainer would otherwise "fix" this by
		// widening the zero — settling by accident the exact question the arm was
		// left standing to keep open.
		if !strings.Contains(out, "star-body") || !strings.Contains(out, "SemanticAnalyzer.java:459-461") {
			t.Fatalf("%s: failure text does not hand over the open boundary decision "+
				"(star-body normalization / Java's unnamed-output rule): %s", site, out)
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

		var floors NameSplitFloors
		floors.Calls[NameSplitSiteLegQOVSegmentsOf] = 1
		floors.Calls[NameSplitSiteFlatColumnBake] = 1

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

// TestNameSplitCensus_MeasuredShapeIsGreen pins the CURRENT reading, so the two
// reds above cannot be satisfied by an assertion that simply always fails.
func TestNameSplitCensus_MeasuredShapeIsGreen(t *testing.T) {
	t.Parallel()

	var counts [nameSplitSiteCount][nameSplitClassCount]int
	counts[NameSplitSiteLegQOVSegmentsOf][NameSplitSegmented] = 9
	counts[NameSplitSiteFlatColumnBake][NameSplitBare] = 2

	var floors NameSplitFloors
	floors.Calls[NameSplitSiteLegQOVSegmentsOf] = 1
	floors.Calls[NameSplitSiteFlatColumnBake] = 1

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
