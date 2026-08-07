package query

import (
	"strings"
	"testing"
)

// The unnest leg-mint census's own gate, exercised without a corpus run.
//
// The assertion it carries is a DISJOINTNESS claim across two populations
// measured in different packages, and a claim of that shape fails silently in
// exactly one way: by never being able to fail. These tests drive both
// directions so the green reading over the real corpus means "the sets are
// disjoint" rather than "the check cannot fire".

func TestUnnestLegMintCensus_DisjointNamesPass(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if failed := assertUnnestLegMintNames(&b, []string{"T1.ID", "T1.V"}, []string{"C.CV", "I.QTY"}); failed {
		t.Fatalf("disjoint name sets reported a failure:\n%s", b.String())
	}
}

func TestUnnestLegMintCensus_OverlapFailsAndSaysWhatItReArms(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if failed := assertUnnestLegMintNames(&b, []string{"T1.ID", "C.CV"}, []string{"C.CV", "I.QTY"}); !failed {
		t.Fatal("an OVERLAPPING name set passed. The disjointness claim this census " +
			"exists to keep honest can then never fail, and every reading of it over " +
			"the corpus is vacuous.")
	}
	msg := b.String()
	if !strings.Contains(msg, "C.CV") {
		t.Fatalf("the failure names no overlapping witness: %q", msg)
	}
	if !strings.Contains(msg, "RE-ARMS") {
		t.Fatalf("the failure does not say what an overlap re-arms, which is the only "+
			"reason this negative result is worth pinning: %q", msg)
	}
}

// Case folding is deliberate: the mint upper-folds its leg and its column, while
// the executor's reader compares case-insensitively. A disjointness check that
// was case-SENSITIVE would report two sets as disjoint whenever one side folded
// differently — a false negative on exactly the claim being made.
func TestUnnestLegMintCensus_OverlapIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if failed := assertUnnestLegMintNames(&b, []string{"c.cv"}, []string{"C.CV"}); !failed {
		t.Fatal("a case-differing overlap passed. The two populations fold their names " +
			"differently, so a case-sensitive comparison reports disjoint sets that are " +
			"the same set.")
	}
}

// An EMPTY population on either side must not read as disjoint news. It reads as
// no news, and the site report is what tells them apart.
func TestUnnestLegMintCensus_EmptyPopulationsDoNotFail(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if assertUnnestLegMintNames(&b, nil, []string{"C.CV"}) {
		t.Fatal("an empty mint population failed the disjointness check")
	}
	if !strings.Contains(b.String(), "VACUOUS") {
		t.Fatalf("an empty mint population passed SILENTLY. A disjointness check with "+
			"an empty side prints exactly like two populations measured apart, which is "+
			"the one reading this census must never produce.\n  got: %q", b.String())
	}
	var b2 strings.Builder
	if assertUnnestLegMintNames(&b2, []string{"T1.ID"}, nil) {
		t.Fatal("an empty reader population failed the disjointness check")
	}
}

// The ARM report exists to disambiguate a zero on a `!seedWindowed` else-branch.
// Its two readings must be visibly different in the rendered text, because the
// text is what a reader quotes.
func TestUnnestLegMintCensus_ArmsRenderApart(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for a := UnnestLegMintArm(0); a < unnestLegMintArmCount; a++ {
		name := a.String()
		if name == "unknown" {
			t.Fatalf("arm %d renders as %q — an arm the census cannot name is an arm "+
				"whose reading is unquotable", int(a), name)
		}
		if _, dup := seen[name]; dup {
			t.Fatalf("two arms render as %q; the two readings a zero has to be told "+
				"apart by would print the same", name)
		}
		seen[name] = struct{}{}
	}
}

// A branch point nobody reached must SAY SO, and must not print as a row of
// zeros beside branch points that were reached and took another arm. Those are
// the two readings F2 exists to separate, and a reader gets them from this text.
func TestUnnestLegMintCensus_UnreachedBranchSaysSo(t *testing.T) {
	t.Parallel()
	// Driven through the pure renderer, not the process globals: this package's
	// TestMain runs the whole suite under an ENABLED census, so parallel
	// translator tests record into those globals concurrently.
	var calls, mints [unnestLegMintSiteCount]int
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	arms[UnnestLegMintSiteBuriedNotWindowed][UnnestLegMintArmOrdinalTwin] = 1
	var reached [unnestLegMintSiteCount]int
	reached[UnnestLegMintSiteBuriedNotWindowed] = 1

	out := formatUnnestLegMintCounters(calls, mints, arms, reached, 1, nil)
	if !strings.Contains(out, "BRANCH POINT NEVER REACHED") {
		t.Fatalf("a branch point with no recorded arm did not announce itself; an "+
			"unreached branch would then print as a row of zeros, which is exactly the "+
			"reading it must be told apart from.\n%s", out)
	}
	if !strings.Contains(out, "ordinal-twin 1") {
		t.Fatalf("the ordinal-twin arm did not render its count; without it a name-arm "+
			"zero cannot be read as CONVERTED HERE.\n%s", out)
	}
	if !strings.Contains(out, "rebaseUnnestOuterLegPredicateOrdinal calls 1") {
		t.Fatalf("the twin's INDEPENDENT total did not render. It is the denominator the "+
			"arm counts are read against, and summing the arms instead would be true by "+
			"construction and blind to the twin's uninstrumented callers.\n%s", out)
	}
}

func TestUnnestLegMintCensus_SitesRenderApart(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for s := UnnestLegMintSite(0); s < unnestLegMintSiteCount; s++ {
		name := s.String()
		if name == "unknown" {
			t.Fatalf("site %d renders as %q — a site the census cannot name is a site "+
				"whose population is unreadable in the report", int(s), name)
		}
		if _, dup := seen[name]; dup {
			t.Fatalf("two sites render as %q; the report would merge their populations", name)
		}
		seen[name] = struct{}{}
	}
}
