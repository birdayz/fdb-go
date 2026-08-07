package query

import (
	"strings"
	"testing"
)

// The ARM PARTITION gate — the invariant the arm matrix's doc claimed before it
// had one.
//
// The arm matrix exists to tell a name-arm ZERO that means "never reached" apart
// from one that means "reached, converted on the other arm". That reading is only
// sound while the arms account for every arrival at the branch point. When the
// branch total was computed AS the arm sum, an arm added without a recorder did
// not surface as a gap — it silently shrank the printed `reached N`, which is the
// summed denominator this census's own text calls "true by construction".
//
// These drive both directions of the check, because a partition assertion that
// cannot fail is exactly the defect it is meant to catch.

func TestUnnestLegMintPartition_ArmsMatchingReachPass(t *testing.T) {
	t.Parallel()
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	var reached [unnestLegMintSiteCount]int
	arms[UnnestLegMintSiteBuriedNotWindowed][UnnestLegMintArmOrdinalTwin] = 9
	arms[UnnestLegMintSiteBuriedNotWindowed][UnnestLegMintArmPlanTimeBake] = 1
	reached[UnnestLegMintSiteBuriedNotWindowed] = 10

	var b strings.Builder
	if assertUnnestLegMintArmPartition(&b, arms, reached) {
		t.Fatalf("arms that account for every arrival failed the partition:\n%s", b.String())
	}
}

// The PHANTOM ARM: a branch arrival that no arm recorded. This is what an arm
// added without a `RecordUnnestLegMintArm` call looks like from the census's
// side, and it is the case the old summed total could not express at all.
func TestUnnestLegMintPartition_PhantomArmFails(t *testing.T) {
	t.Parallel()
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	var reached [unnestLegMintSiteCount]int
	arms[UnnestLegMintSiteJoinPredNotWindowed][UnnestLegMintArmPlanTimeBake] = 92
	arms[UnnestLegMintSiteJoinPredNotWindowed][UnnestLegMintArmLegRelative] = 73
	// One more arrival than the arms account for — an arm with no recorder.
	reached[UnnestLegMintSiteJoinPredNotWindowed] = 166

	var b strings.Builder
	if !assertUnnestLegMintArmPartition(&b, arms, reached) {
		t.Fatal("a branch arrival that NO arm recorded passed the partition.\n" +
			"  The arm matrix's entire reading rests on the arms accounting for every\n" +
			"  arrival; with this undetectable, an arm added without a recorder silently\n" +
			"  moves traffic out of the arms it is read against.")
	}
	msg := b.String()
	if !strings.Contains(msg, "reached 166") || !strings.Contains(msg, "sum to 165") {
		t.Fatalf("the failure does not state both sides of the gap: %q", msg)
	}
	if !strings.Contains(msg, "RE-ARMS") {
		t.Fatalf("the failure does not say what a gap re-arms: %q", msg)
	}
	if !strings.Contains(msg, "jpCensus") {
		t.Fatalf("the failure does not name the sharp case — the joinPredicate branch "+
			"whose census guard IS its third arm, where an inserted `else if` steals "+
			"from leg-relative: %q", msg)
	}
}

// The reverse gap: more arms than arrivals. It means a recorder fires on a path
// that does not pass the branch point, which corrupts the matrix in the opposite
// direction and is just as invisible under a summed total.
func TestUnnestLegMintPartition_ArmWithoutArrivalFails(t *testing.T) {
	t.Parallel()
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	var reached [unnestLegMintSiteCount]int
	arms[UnnestLegMintSiteChainedNameModel][UnnestLegMintArmOrdinalTwin] = 34
	reached[UnnestLegMintSiteChainedNameModel] = 33

	var b strings.Builder
	if !assertUnnestLegMintArmPartition(&b, arms, reached) {
		t.Fatal("an arm recorded on a path that never reached the branch point passed")
	}
}

// A branchless site must record NEITHER. If one grows a branch, the report would
// otherwise keep printing its structural NO SEED BRANCH line over a real reading.
func TestUnnestLegMintPartition_BranchlessSiteMustRecordNothing(t *testing.T) {
	t.Parallel()
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	var reached [unnestLegMintSiteCount]int
	reached[UnnestLegMintSiteNonChainedMerge] = 1

	var b strings.Builder
	if !assertUnnestLegMintArmPartition(&b, arms, reached) {
		t.Fatal("an UNCONDITIONAL call site recorded a branch arrival and passed.\n" +
			"  Its report line is STRUCTURAL (NO SEED BRANCH EXISTS), so a real reading " +
			"arriving there would be printed as a tautology.")
	}
	if !strings.Contains(b.String(), "hasSeedBranch") {
		t.Fatalf("the failure does not name the predicate that has to be updated: %q", b.String())
	}
}

// The RENDERER must also show the gap, not just the assertion — the report is
// what a reader quotes, and a quoted `reached N` that silently equals the arm sum
// is how this defect shipped.
func TestUnnestLegMintPartition_RendererShowsTheGap(t *testing.T) {
	t.Parallel()
	var calls, mints [unnestLegMintSiteCount]int
	var arms [unnestLegMintSiteCount][unnestLegMintArmCount]int
	var reached [unnestLegMintSiteCount]int
	arms[UnnestLegMintSiteBuriedNotWindowed][UnnestLegMintArmOrdinalTwin] = 9
	reached[UnnestLegMintSiteBuriedNotWindowed] = 10

	out := formatUnnestLegMintCounters(calls, mints, arms, reached, 9, nil)
	if !strings.Contains(out, "reached 10") {
		t.Fatalf("the branch total is not the INDEPENDENT counter:\n%s", out)
	}
	if !strings.Contains(out, "ARM GAP") {
		t.Fatalf("the renderer printed a clean row over a gap. A reader quoting "+
			"`reached 10` would be quoting a number the arms do not account for.\n%s", out)
	}
}
