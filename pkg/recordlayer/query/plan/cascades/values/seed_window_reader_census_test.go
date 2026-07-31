package values

import (
	"strings"
	"testing"
)

// TestSeedWindowReader_HardZerosAreWired pins both decline classes.
//
// Each is a claim about COVERAGE — "the gather/wrap is not silently turning
// itself off for this shape" — and a claim of that form is only worth what the
// check behind it is worth. Both were prose in a comment before this census
// existed.
func TestSeedWindowReader_HardZerosAreWired(t *testing.T) {
	t.Parallel()
	var qual [seedWindowSiteCount][seedWindowReadClassCount]int
	qual[SeedWindowSiteGatheredGroupSlot][SeedWindowQualifiedNoIdentity] = 3
	var b strings.Builder
	if !assertSeedWindowReaderCounts(&b, qual, nil) {
		t.Fatal("a QUALIFIED-NO-IDENTITY read must fail the gate")
	}
	if !strings.Contains(b.String(), "QUALIFIED-NO-IDENTITY") ||
		!strings.Contains(b.String(), "gatheredGroupSlot") ||
		!strings.Contains(b.String(), "NAME\n  MODEL") {
		t.Fatalf("the failure must name the class, the site, and what a non-zero re-arms; got %q", b.String())
	}

	var childless [seedWindowSiteCount][seedWindowReadClassCount]int
	childless[SeedWindowSiteBoxLegRef][SeedWindowChildlessBaked] = 1
	var b2 strings.Builder
	if !assertSeedWindowReaderCounts(&b2, childless, nil) {
		t.Fatal("a CHILDLESS-BAKED read must fail the gate")
	}
	if !strings.Contains(b2.String(), "CHILDLESS-BAKED") || !strings.Contains(b2.String(), "boxLegRef") {
		t.Fatalf("the failure must name the class and the site; got %q", b2.String())
	}

	// Ordinary traffic passes: hits and misses are both normal at every site.
	var clean [seedWindowSiteCount][seedWindowReadClassCount]int
	clean[SeedWindowSiteExistentialRebase][SeedWindowHit] = 461
	clean[SeedWindowSiteExistentialRebase][SeedWindowMiss] = 501
	clean[SeedWindowSiteBoxSurvivorQOV][SeedWindowMiss] = 184
	var b3 strings.Builder
	if assertSeedWindowReaderCounts(&b3, clean, nil) {
		t.Fatalf("hits and misses are ordinary traffic, not failures; got %q", b3.String())
	}
}

// TestSeedWindowReader_FloorCatchesASilencedReader is the instrument's reason
// for existing, stated as a test.
//
// Its predecessor was deleted along with the question it answered, and that took
// away the only thing distinguishing an exercised reader from a dark one: every
// claim on this path has the shape "this class is EMPTY", which an unreached
// site prints identically to a site measured clean. A reader that stopped
// running would read GREEN.
//
// Floored per SITE, because the sites do not substitute for one another — the
// existential rebase carries more traffic than the other four together, so a
// total floor stays satisfied with all four dark.
func TestSeedWindowReader_FloorCatchesASilencedReader(t *testing.T) {
	t.Parallel()
	floors := &SeedWindowReaderFloors{}
	floors.Reads[SeedWindowSiteExistentialRebase] = 90
	floors.Reads[SeedWindowSiteGatheredGroupSlot] = 16

	// The existential rebase alone, at full volume: the loud site cannot cover
	// for the quiet one.
	var lopsided [seedWindowSiteCount][seedWindowReadClassCount]int
	lopsided[SeedWindowSiteExistentialRebase][SeedWindowHit] = 962
	var b strings.Builder
	if !assertSeedWindowReaderCounts(&b, lopsided, floors) {
		t.Fatal("a silenced gatheredGroupSlot must fail the gate even while the " +
			"existential rebase runs at full volume — that is what per-site floors are for")
	}
	if !strings.Contains(b.String(), "gatheredGroupSlot") {
		t.Fatalf("the failure must name the DARK site; got %q", b.String())
	}
	if strings.Contains(b.String(), "existentialRebase reached") {
		t.Fatalf("the loud site must not be reported dark; got %q", b.String())
	}

	var both [seedWindowSiteCount][seedWindowReadClassCount]int
	both[SeedWindowSiteExistentialRebase][SeedWindowHit] = 962
	both[SeedWindowSiteGatheredGroupSlot][SeedWindowHit] = 160
	var b2 strings.Builder
	if assertSeedWindowReaderCounts(&b2, both, floors) {
		t.Fatalf("both floors met must pass; got %q", b2.String())
	}
}

// TestSeedWindowReader_LookupHelperMapsBothWays keeps the recording helper from
// drifting: the counters are only evidence if the reader's outcome is what lands
// in them. It exercises the DECISION rather than the mutation, so it can run in
// parallel — the counters are process-global and a test that resets them cannot.
func TestSeedWindowReader_LookupHelperMapsBothWays(t *testing.T) {
	t.Parallel()
	if got := seedWindowLookupClass(true); got != SeedWindowHit {
		t.Fatalf("a found window must count as a hit, got %s", got)
	}
	if got := seedWindowLookupClass(false); got != SeedWindowMiss {
		t.Fatalf("an absent window must count as a miss, got %s", got)
	}
	// Every site must appear in the report, including the ones the corpus may
	// leave at zero — a site missing from the table is a site nobody notices is
	// dark.
	report := FormatSeedWindowReaderCensus()
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		if !strings.Contains(report, s.String()) {
			t.Fatalf("the report omits %s; got %q", s, report)
		}
	}
}

// RFC-200's ACTIVATION TRIPWIRE fires, and it is OFF by default.
//
// NESTED-HIT is unlike every other assertion in this census: a non-zero is GOOD
// NEWS. RFC-200 merged with the nested reader arm correct, pinned on both
// entries and both arms, and UNREACHED — no corpus query selects a nested
// window, so gate (a)'s four mutation directions are not writable. The tripwire
// is what turns activation day into a red test carrying the hand-over, rather
// than a printed number nobody diffs and a gate left unwritten by default.
func TestSeedWindowReader_NestedHitTripwireHandsOverGateA(t *testing.T) {
	t.Parallel()

	var live [seedWindowSiteCount][seedWindowReadClassCount]int
	live[SeedWindowSiteExistentialRebase][SeedWindowHit] = 100
	live[SeedWindowSiteExistentialRebase][SeedWindowNestedHit] = 1

	// OFF by default: the flag is what arms it, so an unrelated caller cannot be
	// surprised by a red on news it did not ask about.
	var quiet strings.Builder
	if assertSeedWindowReaderCounts(&quiet, live, &SeedWindowReaderFloors{}) {
		t.Fatalf("a NESTED-HIT red fired with the tripwire OFF:\n%s", quiet.String())
	}

	var out strings.Builder
	if !assertSeedWindowReaderCounts(&out, live, &SeedWindowReaderFloors{NestedHitMustBeZero: true}) {
		t.Fatal("a NESTED-HIT read left the census GREEN with the tripwire ARMED.\n" +
			"  Without this red, the day RFC-200's nested reader arm goes live nothing is " +
			"visible: the suite passes, the number is printed, and gate (a) stays " +
			"unwritten BY DEFAULT rather than by decision. That is the whole reason the " +
			"count is asserted instead of reported.")
	}
	// The message has to HAND OVER, not merely fail. Whoever sees it is someone
	// who landed an unrelated change and needs the whole task in front of them.
	for _, want := range []string{
		"nested reader arm is now LIVE",
		"WRITE THEM",
		"THIS IS NOT A DEFECT",
		"nested_merge_leg_wrong_rows_fdb_test.go",
		"SILENTLY WRONG ROWS",
		"NestedHitMustBeZero",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the activation message is missing %q — it is a HAND-OVER, and a "+
				"hand-over that omits the fixture, the four directions, the refutation "+
				"clause or the way to clear it sends the reader back to the RFC to "+
				"reconstruct what this branch already knew:\n%s", want, out.String())
		}
	}

	// And a clean census with the tripwire armed stays green, so arming it is not
	// a permanent red.
	var clean strings.Builder
	var none [seedWindowSiteCount][seedWindowReadClassCount]int
	none[SeedWindowSiteExistentialRebase][SeedWindowHit] = 100
	if assertSeedWindowReaderCounts(&clean, none, &SeedWindowReaderFloors{NestedHitMustBeZero: true}) {
		t.Fatalf("the armed tripwire reds on a census with NO nested hits:\n%s", clean.String())
	}
}
