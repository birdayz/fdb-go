package values

import (
	"strings"
	"testing"
)

// TestDottedLegQualifier_TableNameRouteIsNotAContradiction pins the distinction
// that cost this census its first measurement.
//
// A leg registered under its scan TABLE name (`FROM PA AS "s"` answering
// `PA."ID"`) matches a qualifier its own alias does not equal — indistinguishable
// at the reader from a leg carrying two spellings that disagree, and the opposite
// thing. Folded together, the gate's MATCH-ALIAS-DIFFERS zero is unsatisfiable on
// a corpus that has the feature, and the first run reported exactly that: one
// "contradiction" that was the documented second addressing route.
func TestDottedLegQualifier_TableNameRouteIsNotAContradiction(t *testing.T) {
	t.Parallel()
	s := NamedCorrelationIdentifier("S")
	if got := classifyDottedLegQualifier("PA", s, "S", DottedLegLookupHit); got != DottedLegMatchViaTableName {
		t.Fatalf("a table-name qualifier over an aliased leg is its own class, got %s", got)
	}
	// The contradiction the zero is actually about: the qualifier IS the leg's
	// binding, and the leg states a different identity.
	if got := classifyDottedLegQualifier("S", NamedCorrelationIdentifier("q$3"), "S", DottedLegLookupHit); got != DottedLegMatchAliasDiffers {
		t.Fatalf("want MATCH-ALIAS-DIFFERS, got %s", got)
	}
}

// TestDottedLegQualifier_ExactAliasTest pins the case-disjointness. A leg matched
// on its own binding by folded text while stating the lowercase machine spelling
// is a leg with two spellings, not a match.
func TestDottedLegQualifier_ExactAliasTest(t *testing.T) {
	t.Parallel()
	if got := classifyDottedLegQualifier("Q$5", NamedCorrelationIdentifier("q$5"), "Q$5", DottedLegLookupHit); got != DottedLegMatchAliasDiffers {
		t.Fatalf("a case-variant alias is not a match, got %s", got)
	}
	if got := classifyDottedLegQualifier("Q$5", NamedCorrelationIdentifier("Q$5"), "Q$5", DottedLegLookupHit); got != DottedLegMatchAliasIsQualifier {
		t.Fatalf("an exactly-equal alias is a match, got %s", got)
	}
	if got := classifyDottedLegQualifier("A", CorrelationIdentifier{}, "A", DottedLegLookupHit); got != DottedLegMatchNoAlias {
		t.Fatalf("want MATCH-NO-ALIAS, got %s", got)
	}
	if got := classifyDottedLegQualifier("A", NamedCorrelationIdentifier("A"), "", DottedLegLookupMiss); got != DottedLegNoMatch {
		t.Fatalf("want noMatch, got %s", got)
	}
}

// TestDottedLegQualifier_GateFailsOnDisagreeingSpellings pins that the
// MATCH-ALIAS-DIFFERS zero is wired and names its site.
//
// It no longer pairs with "and a table-name route does NOT trip it". Every class
// trips the gate now: the channel is retired and ANY call is the alarm, so the
// per-class distinction survives in the classifier (pinned above) and in the
// failure text, not in which counts are tolerated. The classifier pins are what
// keep that distinction from rotting while nothing drives it.
func TestDottedLegQualifier_GateFailsOnDisagreeingSpellings(t *testing.T) {
	t.Parallel()
	var counts [dottedLegSiteCount][dottedLegClassCount]int
	counts[DottedLegSiteLegQOVBake][DottedLegMatchAliasDiffers] = 1
	var b strings.Builder
	if !assertDottedLegQualifierCounts(&b, counts, nil) {
		t.Fatal("a leg matched on its own binding while stating another identity must fail the gate")
	}
	if !strings.Contains(b.String(), "legQOVBake") {
		t.Fatalf("the failure must name the site; got %q", b.String())
	}
	if !strings.Contains(b.String(), "MATCH-ALIAS-DIFFERS") {
		t.Fatalf("the failure must name the CLASS — the retirement alarm fires on any "+
			"call, so the class is the only thing telling the two apart; got %q", b.String())
	}
}

// TestDottedLegQualifier_RetirementIsWatched replaces the floors that used to
// catch an unreached site.
//
// Both sites were arms of the name-model bake, and the ordinal model deleted
// them: RecordDottedLegQualifier has no caller, so the population is
// structurally zero and a floor on it is unsatisfiable. The direction inverts —
// an unreached site is the steady state and a REACHED one is the alarm.
func TestDottedLegQualifier_RetirementIsWatched(t *testing.T) {
	t.Parallel()
	var empty [dottedLegSiteCount][dottedLegClassCount]int
	var b strings.Builder
	if assertDottedLegQualifierCounts(&b, empty, &DottedLegQualifierFloors{}) {
		t.Fatalf("the RETIRED steady state (nothing reaches either site) failed the gate; got %q",
			b.String())
	}
	// Every site, and a class that used to be tolerated: the alarm has to cover
	// the whole channel or an arm can revive under a class nobody watched.
	for site := DottedLegSite(0); site < dottedLegSiteCount; site++ {
		var counts [dottedLegSiteCount][dottedLegClassCount]int
		counts[site][DottedLegMatchViaTableName] = 1
		var b2 strings.Builder
		if !assertDottedLegQualifierCounts(&b2, counts, &DottedLegQualifierFloors{}) {
			t.Fatalf("%s reached once and the gate PASSED. The dotted-qualifier match "+
				"channel is retired; a call means a name-splitting resolution path is back.",
				site)
		}
		if !strings.Contains(b2.String(), "revival") {
			t.Fatalf("%s: the failure does not state which DIRECTION is the alarm; got %q",
				site, b2.String())
		}
	}
}

// TestDottedLegQualifier_AmbiguousIsNotNoMatch pins the fifth class.
//
// The map-based reader records its call BEFORE bailing on a nil layout, and a
// qualifier with no entry and a qualifier whose entry was POISONED for ambiguity
// (`layouts[key] = nil`, two legs claiming one alias) are the same nil at that
// point. Reported as noMatch, a qualifier carried by TWO legs reads as one
// carried by none — the opposite fact about the leg table. The classes are only
// a partition of every call once the reader's own map read is threaded through
// instead of being re-derived from the leg it did not find.
func TestDottedLegQualifier_AmbiguousIsNotNoMatch(t *testing.T) {
	t.Parallel()
	if got := classifyDottedLegQualifier("A", CorrelationIdentifier{}, "", DottedLegLookupAmbiguous); got != DottedLegAmbiguousQualifier {
		t.Fatalf("a POISONED qualifier is its own class, got %s — folded into noMatch it "+
			"reports a qualifier carried by two legs as one carried by none", got)
	}
	if got := classifyDottedLegQualifier("A", CorrelationIdentifier{}, "", DottedLegLookupMiss); got != DottedLegNoMatch {
		t.Fatalf("an ABSENT qualifier is still noMatch, got %s", got)
	}
	// Ambiguity outranks every question about the leg, because there is no leg:
	// a poisoned entry carries neither a binding nor an identity to ask about.
	if got := classifyDottedLegQualifier("A", NamedCorrelationIdentifier("A"), "A", DottedLegLookupAmbiguous); got != DottedLegAmbiguousQualifier {
		t.Fatalf("ambiguity must decide before the alias tests, got %s", got)
	}
}

// TestDottedLegQualifier_NoAliasTripsTheGate pins the SECOND hard zero.
//
// MATCH-NO-ALIAS was documented as blocking from the day it was written and was
// never gated: its zero was a sentence in a comment, and a population arriving
// there would have printed on the report and failed nothing. It is the reader
// side of the collision the seed-window authority declines on — a leg stating no
// identity is a leg the identity-keyed conversion cannot carry, and two of them
// occupy one key.
func TestDottedLegQualifier_NoAliasTripsTheGate(t *testing.T) {
	t.Parallel()
	var counts [dottedLegSiteCount][dottedLegClassCount]int
	counts[DottedLegSiteLegQOVBake][DottedLegMatchNoAlias] = 3
	var b strings.Builder
	if !assertDottedLegQualifierCounts(&b, counts, nil) {
		t.Fatal("a leg matched while stating NO identity must fail the gate; it was " +
			"documented as blocking and checked by nothing")
	}
	if !strings.Contains(b.String(), "MATCH-NO-ALIAS") || !strings.Contains(b.String(), "legQOVBake") {
		t.Fatalf("the failure must name the class and the site; got %q", b.String())
	}
	// The ambiguous class used to be tolerated by the gate — it is a fact about
	// the corpus, and poisoning is the reader behaving correctly. It is no longer
	// tolerated, because nothing may reach this channel at all; what survives is
	// that it is still its OWN class, which the classifier pin above holds.
	var ambiguous [dottedLegSiteCount][dottedLegClassCount]int
	ambiguous[DottedLegSiteLegQOVBake][DottedLegAmbiguousQualifier] = 9
	var b2 strings.Builder
	if !assertDottedLegQualifierCounts(&b2, ambiguous, nil) {
		t.Fatalf("an ambiguous qualifier still reaches the retired channel and must "+
			"trip its revival alarm; got %q", b2.String())
	}
}
