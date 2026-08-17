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

// TestNameSplitCensus_RetirementIsWatched replaces the three floor directions
// this file used to pin (a dark arm, a dark splitting arm, and a stale
// floor-of-zero declaration).
//
// All three watched the population for COLLAPSE, because both arms were nearly
// dark already — nine calls and two over the whole corpus — so SPLIT-QUALIFIED
// == 0 was nearly vacuous on its own, and an arm that stopped being driven read
// exactly like an arm measured clean.
//
// The arms are gone. Both were part of the NAME-model bake, which the ordinal
// model replaced with resolution by baked slot, so RecordNameSplit has no caller
// and the population is structurally zero. A floor on it is unsatisfiable; the
// direction inverts and a CALL is the alarm.
func TestNameSplitCensus_RetirementIsWatched(t *testing.T) {
	t.Parallel()

	var empty [nameSplitSiteCount][nameSplitClassCount]int
	var sb strings.Builder
	if assertNameSplitCounts(&sb, empty, nil, &NameSplitFloors{}) {
		t.Fatalf("the RETIRED steady state (neither arm reached) failed the census: %s", sb.String())
	}

	// Every site, and on SEGMENTED traffic — the class that used to be the
	// healthy one. The alarm has to cover the whole channel or an arm revives
	// under a class nobody watched.
	for site := NameSplitSite(0); site < nameSplitSiteCount; site++ {
		var counts [nameSplitSiteCount][nameSplitClassCount]int
		counts[site][NameSplitSegmented] = 1
		var b strings.Builder
		if !assertNameSplitCounts(&b, counts, nil, &NameSplitFloors{}) {
			t.Fatalf("%s was reached once and the census stayed green. The name-split "+
				"channel is retired; a call means a resolution path that decides from a "+
				"rendered NAME is back.", site)
		}
		if out := b.String(); !strings.Contains(out, "revival") {
			t.Fatalf("%s: the failure does not state which DIRECTION is the alarm: %s", site, out)
		}
	}

	// The retirement alarm must survive a narrowed run (nil floors): a guard that
	// only watches a retired population on full runs stops watching it exactly
	// when someone is iterating on the code that would revive it.
	var revived [nameSplitSiteCount][nameSplitClassCount]int
	revived[NameSplitSiteFlatColumnBake][NameSplitBare] = 1
	var b2 strings.Builder
	if !assertNameSplitCounts(&b2, revived, nil, nil) {
		t.Fatalf("with floors dropped, a revived arm passed: %s", b2.String())
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
