package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The merge-slot census and the leg-read classifier are both DECISIONS, and a
// whole-suite run walks only the arms the corpus happens to reach. That is not a
// substitute for driving them: the census's Untyped arm is asserted at zero, so
// a healthy corpus reaches it exactly never, and its first real firing would be
// read as a finding rather than as an untested branch.
//
// These pins existed before, against the census this one was extracted from,
// and were deleted with that file. They are restored here against the extracted
// instrument, which takes explicit state and so can be driven directly.

func TestClassifyMergeSlotPartitionsTheFourOutcomes(t *testing.T) {
	t.Parallel()

	recordType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}}

	for _, tc := range []struct {
		name      string
		slotType  values.Type
		scavenged bool
		want      mergeSlotClass
		why       string
	}{
		{
			name:     "a record type from the quantifier is TYPED",
			slotType: recordType, want: mergeSlotClassTyped,
			why: "the quantifier is the authority for its own row, and it stated one",
		},
		{
			name:     "SCAVENGED wins over the type it recovered",
			slotType: recordType, scavenged: true, want: mergeSlotClassScavenged,
			why: "the recovered slot is a record too, so classifying on the type alone would " +
				"hide the fallback entirely and report the scavenge rate as zero",
		},
		{
			name:     "a non-record type is SCALAR, not a defect",
			slotType: values.NotNullLong, want: mergeSlotClassScalar,
			why: "an unnest ELEMENT flows one array element, a scalar, and is correct — " +
				"inheriting the record test would report every unnest as a lost leg row",
		},
		{
			name:     "a nil type states nothing and is UNTYPED",
			slotType: nil, want: mergeSlotClassUntyped,
			why: "this is the class asserted at zero: the reference degrades to source-relative " +
				"and the join returns zero rows reporting success",
		},
		{
			name:     "UNKNOWN states nothing either and is UNTYPED",
			slotType: values.UnknownType, want: mergeSlotClassUntyped,
			why: "UnknownType is a non-nil *PrimitiveType, so a bare nil check would file it " +
				"under SCALAR and the hard zero would stop seeing the very residual it guards",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyMergeSlot(tc.slotType, tc.scavenged); got != tc.want {
				t.Fatalf("classifyMergeSlot(%v, scavenged=%t) = %v, want %v — %s",
					tc.slotType, tc.scavenged, got, tc.want, tc.why)
			}
		})
	}
}

func TestAssertMergeSlotTypingCensusFiresOnEachArm(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		counts     MergeSlotTypingCounts
		floor      int
		wantFailed bool
		wantIn     string
		why        string
	}{
		{
			name:   "a clean census with a holding partition does NOT fail",
			counts: MergeSlotTypingCounts{Slots: 100, Typed: 90, Scalar: 10},
			floor:  10, wantFailed: false,
			why: "the polarity is FAILED, not OK — returning true here makes every clean run " +
				"report as a gate failure, which is how the inversion was caught",
		},
		{
			name:   "the four classes must PARTITION the denominator",
			counts: MergeSlotTypingCounts{Slots: 100, Typed: 50, Scalar: 10},
			floor:  10, wantFailed: true, wantIn: "must PARTITION the denominator",
			why: "without the identity every share is a number about nothing, including the " +
				"Untyped share the gate exists for",
		},
		{
			name:   "a COLLAPSED denominator fails even though Untyped is 0",
			counts: MergeSlotTypingCounts{Slots: 3, Typed: 3},
			floor:  10, wantFailed: true, wantIn: "went QUIET",
			why: "this is the vacuity arm: Untyped == 0 is true because nothing was measured, " +
				"and it prints identically to a clean measurement",
		},
		{
			name:   "a NON-ZERO Untyped fails",
			counts: MergeSlotTypingCounts{Slots: 100, Typed: 90, Untyped: 10},
			floor:  10, wantFailed: true, wantIn: "Untyped = 10, want 0",
			why: "a slot stating no type is the silent zero-rows defect the census exists for",
		},
		{
			name:   "every arm can fire at once and each is reported",
			counts: MergeSlotTypingCounts{Slots: 3, Typed: 1, Untyped: 5},
			floor:  10, wantFailed: true, wantIn: "went QUIET",
			why: "the arms must not short-circuit one another — a report naming one cause when " +
				"three hold sends the reader down one branch",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var w strings.Builder
			failed := AssertMergeSlotTypingCensus(&w, tc.counts, tc.floor)
			if failed != tc.wantFailed {
				t.Fatalf("AssertMergeSlotTypingCensus returned failed=%t, want %t — %s.\nreport: %s",
					failed, tc.wantFailed, tc.why, w.String())
			}
			if tc.wantIn != "" && !strings.Contains(w.String(), tc.wantIn) {
				t.Fatalf("report does not name the cause %q — a gate that fails without saying "+
					"which arm fired sends the reader to the wrong subsystem.\nreport: %s",
					tc.wantIn, w.String())
			}
		})
	}

	// The all-arms case above must report ALL of them, not just the first.
	var w strings.Builder
	AssertMergeSlotTypingCensus(&w, MergeSlotTypingCounts{Slots: 3, Typed: 1, Untyped: 5}, 10)
	for _, want := range []string{"must PARTITION the denominator", "went QUIET", "want 0"} {
		if !strings.Contains(w.String(), want) {
			t.Fatalf("a census violating all three conditions omitted %q from its report.\nreport: %s",
				want, w.String())
		}
	}
}

// classifyLegReadIdentity is load-bearing CONTROL FLOW, not census: the
// InLegDomain answer is what rebaseOuterLegValue's arms branch on. Its only test
// lived in the census file that RFC-235 deleted, so it survived the deletion
// unpinned.
func TestClassifyLegReadIdentityPartitionsTheThreeOutcomes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                     string
		hasResolved, inLegDomain bool
		want                     legReadIdentity
		why                      string
	}{
		{
			name:        "an identity IN the leg's domain wins regardless of hasResolved",
			hasResolved: false, inLegDomain: true, want: legReadIdentityInLegDomain,
			why: "this is the arm the rebase branches on; the leg's own layout answered",
		},
		{
			name:        "in-leg-domain still wins when a resolved path also exists",
			hasResolved: true, inLegDomain: true, want: legReadIdentityInLegDomain,
			why: "ordering matters — testing hasResolved first would misroute every read " +
				"that has both",
		},
		{
			name:        "a resolved path the leg cannot read is OTHER DOMAIN",
			hasResolved: true, inLegDomain: false, want: legReadIdentityOtherDomain,
			why: "it states an identity, just not one this leg's layout can serve",
		},
		{
			name:        "no resolved path at all is LAZY NAME ONLY",
			hasResolved: false, inLegDomain: false, want: legReadIdentityLazyNameOnly,
			why: "the display name is all there is, which is the arm that must DECLINE rather " +
				"than mint a name-keyed read",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyLegReadIdentity(tc.hasResolved, tc.inLegDomain)
			if got != tc.want {
				t.Fatalf("classifyLegReadIdentity(hasResolved=%t, inLegDomain=%t) = %v, want %v — %s",
					tc.hasResolved, tc.inLegDomain, got, tc.want, tc.why)
			}
		})
	}
}
