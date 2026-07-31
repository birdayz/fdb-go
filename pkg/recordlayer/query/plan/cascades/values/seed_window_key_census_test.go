package values

import (
	"strings"
	"testing"
)

// The seed-window key census's whole product is a per-site verdict of the form
// "the blocking classes are empty", and every one of those verdicts is only as
// good as the classifier that files them. These tests exercise the classifier
// directly, over hand-built window maps, because the corpus cannot produce the
// states that would make the verdict wrong — that is precisely why the verdict
// is worth having, and precisely why nothing in the corpus can check it.

func win(offset int, alias CorrelationIdentifier, cols ...string) OrdinalSeedLegWindow {
	fields := make([]Field, len(cols))
	for i, c := range cols {
		fields[i] = Field{Name: c, Ordinal: i}
	}
	return OrdinalSeedLegWindow{Offset: offset, Typ: &RecordType{Fields: fields}, Alias: alias}
}

// TestSeedWindowKey_IdentityAndTextAgree is the ordinary corpus shape: the key
// is the upper fold of the correlation and the window carries that same
// correlation.
func TestSeedWindowKey_IdentityAndTextAgree(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	windows := map[string]OrdinalSeedLegWindow{
		"A": win(0, a, "ID", "V"),
		"B": win(2, NamedCorrelationIdentifier("B"), "K"),
	}
	if got, _ := classifySeedWindowKey(windows, "A", a); got != SeedWindowIdentityAgreesHit {
		t.Fatalf("want identityAgreesHit, got %s", got)
	}
	c := NamedCorrelationIdentifier("C")
	if got, _ := classifySeedWindowKey(windows, "C", c); got != SeedWindowIdentityAgreesMiss {
		t.Fatalf("want identityAgreesMiss, got %s", got)
	}
}

// TestSeedWindowKey_TextOnlyHitIsTheBlocker pins the class that BLOCKS re-keying:
// the text finds a window and no window states the reference's identity, so an
// identity-keyed reader would stop resolving a leg this one resolves.
//
// This is the state a corpus measurement of zero is a claim ABOUT, and the
// corpus cannot produce it — a leg filed under a key that no leg's identity
// matches is a producer bug, not a query shape. Without this test the zero is
// unfalsifiable.
func TestSeedWindowKey_TextOnlyHitIsTheBlocker(t *testing.T) {
	t.Parallel()
	// The window is filed under "A" but states a DIFFERENT identity, so the
	// reference correlated to A finds the window by text and nothing by identity.
	windows := map[string]OrdinalSeedLegWindow{
		"A": win(0, NamedCorrelationIdentifier("q$7"), "ID"),
	}
	got, detail := classifySeedWindowKey(windows, "A", NamedCorrelationIdentifier("A"))
	if got != SeedWindowTextOnlyHit {
		t.Fatalf("want TEXT-ONLY-HIT, got %s (%s)", got, detail)
	}
}

// TestSeedWindowKey_IdentityOnlyHit is the other direction: the window states
// the reference's identity but is filed under a key the reference's fold does
// not produce. Re-keying would START resolving something the text reader passes
// through.
func TestSeedWindowKey_IdentityOnlyHit(t *testing.T) {
	t.Parallel()
	q := NamedCorrelationIdentifier("q$7")
	windows := map[string]OrdinalSeedLegWindow{"Q$7": win(0, q, "ID")}
	// The reader folds the correlation to "Q$7"... but suppose it did not, and
	// keyed with the raw lowercase name: text misses, identity hits.
	got, detail := classifySeedWindowKey(windows, "q$7", q)
	if got != SeedWindowIdentityOnlyHit {
		t.Fatalf("want IDENTITY-ONLY-HIT, got %s (%s)", got, detail)
	}
}

// TestSeedWindowKey_DivergedIsTheHardZero pins the contradiction the gate
// refuses: both keys hit and they select different slots.
func TestSeedWindowKey_DivergedIsTheHardZero(t *testing.T) {
	t.Parallel()
	a := NamedCorrelationIdentifier("A")
	windows := map[string]OrdinalSeedLegWindow{
		// Filed under "A" but stating B's identity...
		"A": win(0, NamedCorrelationIdentifier("B"), "ID"),
		// ...while A's identity sits at a different offset under another key.
		"BURIED": win(5, a, "ID"),
	}
	got, detail := classifySeedWindowKey(windows, "A", a)
	if got != SeedWindowKeyDiverged {
		t.Fatalf("want DIVERGED, got %s (%s)", got, detail)
	}
	var counts [seedWindowSiteCount][seedWindowKeyClassCount]int
	counts[SeedWindowSiteExistentialRebase][SeedWindowKeyDiverged] = 1
	var b strings.Builder
	if !assertSeedWindowKeyCounts(&b, counts, nil) {
		t.Fatal("a DIVERGED lookup must fail the gate")
	}
	if !strings.Contains(b.String(), "DIVERGED") {
		t.Fatalf("the failure must name what diverged; got %q", b.String())
	}
}

// TestSeedWindowKey_TwoKeysOneWindowIsNotDivergence pins the distinction the
// gate would otherwise manufacture a contradiction out of: finalizeSeedWindows
// files a box run and its rightmost leaf under two keys, and where those two
// entries carry the same offset and columns, a reader keyed either way reads the
// same slots. That is agreement.
func TestSeedWindowKey_TwoKeysOneWindowIsNotDivergence(t *testing.T) {
	t.Parallel()
	c := NamedCorrelationIdentifier("C")
	windows := map[string]OrdinalSeedLegWindow{
		"C$BOX": win(0, c, "ID", "V"),
		"C":     win(0, c, "ID", "V"),
	}
	if got, detail := classifySeedWindowKey(windows, "C$BOX", c); got != SeedWindowIdentityAgreesHit {
		t.Fatalf("two keys over one window must agree, got %s (%s)", got, detail)
	}
}

// TestSeedWindowKey_TextSiteMintIsExactNotFolding is the case-disjointness the
// text-only sites turn on. A key `Q$5` over a window whose alias is `q$5` would
// pass a folding test and still mint an identifier SameLeg refuses to match, so
// the classifier must call it a forgery rather than a match.
func TestSeedWindowKey_TextSiteMintIsExactNotFolding(t *testing.T) {
	t.Parallel()
	windows := map[string]OrdinalSeedLegWindow{"Q$5": win(0, NamedCorrelationIdentifier("q$5"), "ID")}
	got, detail := classifySeedWindowKey(windows, "Q$5", CorrelationIdentifier{})
	if got != SeedWindowTextSiteHitAliasDiffers {
		t.Fatalf("a case-variant alias is a forgery, not a round trip: got %s (%s)", got, detail)
	}
	windows["Q$5"] = win(0, NamedCorrelationIdentifier("Q$5"), "ID")
	if got, _ := classifySeedWindowKey(windows, "Q$5", CorrelationIdentifier{}); got != SeedWindowTextSiteHitAliasIsKey {
		t.Fatalf("an exactly-equal alias is a round trip: got %s", got)
	}
}

// TestSeedWindowKey_IdentityScanIsDeterministic pins the sorted scan. Two
// windows can carry one identity, and picking between them by map order would
// make the census report a different verdict on each run.
func TestSeedWindowKey_IdentityScanIsDeterministic(t *testing.T) {
	t.Parallel()
	c := NamedCorrelationIdentifier("C")
	windows := map[string]OrdinalSeedLegWindow{
		"ZZ": win(9, c, "ID"),
		"AA": win(0, c, "ID"),
	}
	for i := 0; i < 64; i++ {
		k, _, ok := seedWindowByIdentity(windows, c)
		if !ok || k != "AA" {
			t.Fatalf("iteration %d: want the sorted-first key AA, got %q (ok=%v)", i, k, ok)
		}
	}
}

// TestSeedWindowKey_FloorsCatchAnUnreachedSite pins the reason the floors exist:
// a site nothing drove reports every blocking class at zero, which is the exact
// shape of a site measured clean.
func TestSeedWindowKey_FloorsCatchAnUnreachedSite(t *testing.T) {
	t.Parallel()
	var counts [seedWindowSiteCount][seedWindowKeyClassCount]int
	floors := &SeedWindowKeyFloors{}
	floors.Calls[SeedWindowSiteExistentialRebase] = 90
	var b strings.Builder
	if !assertSeedWindowKeyCounts(&b, counts, floors) {
		t.Fatal("an unreached floored site must fail the gate")
	}
	if !strings.Contains(b.String(), "existentialRebase") {
		t.Fatalf("the failure must name the dark site; got %q", b.String())
	}
	// The same counts pass with no floors, which is what makes the floors the
	// thing doing the work rather than the partition.
	var b2 strings.Builder
	if assertSeedWindowKeyCounts(&b2, counts, nil) {
		t.Fatalf("an empty census with no floors must pass; got %q", b2.String())
	}
}
