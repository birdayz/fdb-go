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
	if got := classifyDottedLegQualifier("PA", s, "S", true); got != DottedLegMatchViaTableName {
		t.Fatalf("a table-name qualifier over an aliased leg is its own class, got %s", got)
	}
	// The contradiction the zero is actually about: the qualifier IS the leg's
	// binding, and the leg states a different identity.
	if got := classifyDottedLegQualifier("S", NamedCorrelationIdentifier("q$3"), "S", true); got != DottedLegMatchAliasDiffers {
		t.Fatalf("want MATCH-ALIAS-DIFFERS, got %s", got)
	}
}

// TestDottedLegQualifier_ExactAliasTest pins the case-disjointness. A leg matched
// on its own binding by folded text while stating the lowercase machine spelling
// is a leg with two spellings, not a match.
func TestDottedLegQualifier_ExactAliasTest(t *testing.T) {
	t.Parallel()
	if got := classifyDottedLegQualifier("Q$5", NamedCorrelationIdentifier("q$5"), "Q$5", true); got != DottedLegMatchAliasDiffers {
		t.Fatalf("a case-variant alias is not a match, got %s", got)
	}
	if got := classifyDottedLegQualifier("Q$5", NamedCorrelationIdentifier("Q$5"), "Q$5", true); got != DottedLegMatchAliasIsQualifier {
		t.Fatalf("an exactly-equal alias is a match, got %s", got)
	}
	if got := classifyDottedLegQualifier("A", CorrelationIdentifier{}, "A", true); got != DottedLegMatchNoAlias {
		t.Fatalf("want MATCH-NO-ALIAS, got %s", got)
	}
	if got := classifyDottedLegQualifier("A", NamedCorrelationIdentifier("A"), "", false); got != DottedLegNoMatch {
		t.Fatalf("want noMatch, got %s", got)
	}
}

// TestDottedLegQualifier_GateFailsOnDisagreeingSpellings pins that the hard zero
// is wired, and that a table-name route does NOT trip it.
func TestDottedLegQualifier_GateFailsOnDisagreeingSpellings(t *testing.T) {
	t.Parallel()
	var counts [dottedLegSiteCount][dottedLegClassCount]int
	counts[DottedLegSiteLegQOVBake][DottedLegMatchViaTableName] = 7
	var b strings.Builder
	if assertDottedLegQualifierCounts(&b, counts, nil) {
		t.Fatalf("the table-name route must not fail the gate; got %q", b.String())
	}
	counts[DottedLegSiteLegQOVBake][DottedLegMatchAliasDiffers] = 1
	var b2 strings.Builder
	if !assertDottedLegQualifierCounts(&b2, counts, nil) {
		t.Fatal("a leg matched on its own binding while stating another identity must fail the gate")
	}
	if !strings.Contains(b2.String(), "legQOVBake") {
		t.Fatalf("the failure must name the site; got %q", b2.String())
	}
}

// TestDottedLegQualifier_FloorsCatchAnUnreachedSite: the MATCH-ALIAS-DIFFERS zero
// is the whole finding, and over an empty population it costs nothing.
func TestDottedLegQualifier_FloorsCatchAnUnreachedSite(t *testing.T) {
	t.Parallel()
	var counts [dottedLegSiteCount][dottedLegClassCount]int
	floors := &DottedLegQualifierFloors{}
	floors.Calls[DottedLegSiteFlatColumnBake] = 10
	var b strings.Builder
	if !assertDottedLegQualifierCounts(&b, counts, floors) {
		t.Fatal("an unreached floored site must fail the gate")
	}
	if !strings.Contains(b.String(), "flatColumnBake") {
		t.Fatalf("the failure must name the dark site; got %q", b.String())
	}
}
