package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The bakeability census is a GATE: the sqldriver suite runs it over the whole
// corpus and a red gate stops the build. Everything it protects therefore rests
// on one claim — that these counter states actually FAIL — and until this test
// existed nothing checked that claim. The gate ran only over a healthy corpus,
// where every assertion holds, so an assertion that had been dropped, inverted or
// never wired would have looked exactly the same: green.
//
// That is not hypothetical. DisagreeingLegs sat in the partition unasserted while
// its three siblings were asserted at zero, and the shape of the miss is
// instructive: a leg whose GetFlowedObjectType ERRORS gets no layout, exactly
// like an underivable one, so legs could migrate out of FlowedLegs into it with
// the partition still adding up and the only visible effect being FlowedLegs
// drifting down against a floor set an order of magnitude below it.
//
// Each case below violates exactly ONE invariant, so a dropped check is a red
// here rather than a silence in the field.
func TestLegLocalBakeGateFailsOnEachViolation(t *testing.T) {
	t.Parallel()

	// healthy is the corpus's measured shape: the state the gate must PASS, or
	// every red below is red for the uninteresting reason.
	healthy := legLocalBakeCounters{
		Total: 126, Baked: 0, Minted: 126,
		UntypedLeg: 0, ColumnAbsent: 0, LayoutAvailable: 126,
		FlowedLegs: 848, DisagreeingLegs: 0, UnderivableLegs: 0,
		LegDerivations:  848,
		MergeSlots:      18246,
		MergeSlotsTyped: 17492, MergeSlotsScavenged: 4, MergeSlotsScalar: 0,
		MergeSlotsUntyped: 750,
	}
	floors := &LegLocalBakeFloors{Total: 12, LegDerivations: 80, MergeSlots: 1800}

	var healthyOut strings.Builder
	if assertLegLocalBakeCounters(&healthyOut, healthy, floors) {
		t.Fatalf("the gate FAILS the corpus's own measured counter state:\n%s\n"+
			"  Every case below expects a red, so a baseline that is already red proves "+
			"nothing about any of them.", healthyOut.String())
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*legLocalBakeCounters)
		wantMsg string
	}{
		{
			// A leg whose reference members flow different rows. Java Verify-fails;
			// Go declines the leg and the read falls back to the qualified name, so
			// the cost is an underivable leg's for a strictly worse reason.
			name:    "a member-disagreement leg",
			mutate:  func(c *legLocalBakeCounters) { c.FlowedLegs--; c.DisagreeingLegs++ },
			wantMsg: "DisagreeingLegs = 1, want 0",
		},
		{
			name:    "a leg whose layout cannot be derived",
			mutate:  func(c *legLocalBakeCounters) { c.FlowedLegs--; c.UnderivableLegs++ },
			wantMsg: "UnderivableLegs = 1, want 0",
		},
		{
			name: "a read that minted because its leg stated no type",
			mutate: func(c *legLocalBakeCounters) {
				c.LayoutAvailable--
				c.UntypedLeg++
			},
			wantMsg: "UntypedLeg = 1, want 0",
		},
		{
			// The three per-leg outcomes must partition LegDerivations, or
			// UnderivableLegs counts the instrumented paths rather than the legs.
			name:    "a leg that leaves the derivation by an uncounted path",
			mutate:  func(c *legLocalBakeCounters) { c.LegDerivations++ },
			wantMsg: "but LegDerivations = 849",
		},
		{
			name:    "a firing counted into Total and classified into neither bucket",
			mutate:  func(c *legLocalBakeCounters) { c.Total++ },
			wantMsg: "but Total = 127",
		},
		{
			name:    "a minted read with no reason",
			mutate:  func(c *legLocalBakeCounters) { c.Minted++; c.Baked-- },
			wantMsg: "but Minted = 127",
		},
		{
			// The floors are what keep every zero above from holding vacuously.
			name:    "the leg-derivation population collapses",
			mutate:  func(c *legLocalBakeCounters) { *c = legLocalBakeCounters{} },
			wantMsg: "LegDerivations = 0, want >= 80",
		},
		{
			// The merge-slot partition. MergeSlotsUntyped is an upper bound on the
			// one remaining path by which a leg's row is silently lost here, and a
			// bound is only a bound while its siblings account for the rest.
			name:    "a merge slot counted into no bucket",
			mutate:  func(c *legLocalBakeCounters) { c.MergeSlots++ },
			wantMsg: "but MergeSlots = 18247",
		},
		{
			name: "the merge-slot population collapses",
			mutate: func(c *legLocalBakeCounters) {
				c.MergeSlots, c.MergeSlotsTyped, c.MergeSlotsScavenged, c.MergeSlotsUntyped = 0, 0, 0, 0
			},
			wantMsg: "MergeSlots = 0, want >= 1800",
		},
		{
			name: "the read population collapses",
			mutate: func(c *legLocalBakeCounters) {
				c.Total, c.Baked, c.Minted, c.LayoutAvailable = 0, 0, 0, 0
			},
			wantMsg: "Total = 0, want >= 12",
		},
	} {
		got := healthy
		tc.mutate(&got)
		var out strings.Builder
		if !assertLegLocalBakeCounters(&out, got, floors) {
			t.Errorf("%s: the gate PASSED %+v.\n"+
				"  This counter state is one the census exists to catch; a gate that only\n"+
				"  ever runs over a healthy corpus cannot tell a live assertion from a\n"+
				"  dropped one.", tc.name, got)
			continue
		}
		if !strings.Contains(out.String(), tc.wantMsg) {
			t.Errorf("%s: the gate failed, but not for the stated reason — want a message "+
				"containing %q, got:\n%s\n"+
				"  A red for the wrong invariant is how a check stays green in the one "+
				"direction it was written for.", tc.name, tc.wantMsg, out.String())
		}
	}
}

// The floors are dropped on a narrowed run, and the zeros are not. That split is
// the whole reason the census can be quoted from a filtered invocation at all, so
// it gets its own direction: with floors nil, an empty population must PASS (the
// zeros hold vacuously — which is what the floors exist to prevent, on the runs
// that have them) while a real violation must still fail.
func TestLegLocalBakeGateWithoutFloors(t *testing.T) {
	t.Parallel()

	var empty strings.Builder
	if assertLegLocalBakeCounters(&empty, legLocalBakeCounters{}, nil) {
		t.Errorf("with floors dropped, an EMPTY census failed:\n%s\n"+
			"  A narrowed -test.run reaches no leg derivations at all, and the partitions "+
			"hold there as 0 == 0. Failing would make every focused run red.", empty.String())
	}

	var violated strings.Builder
	if !assertLegLocalBakeCounters(&violated,
		legLocalBakeCounters{LegDerivations: 1, DisagreeingLegs: 1}, nil) {
		t.Error("with floors dropped, a member-disagreement leg PASSED. The floors " +
			"describe a whole-suite population; the zeros describe a defect, and dropping " +
			"the first must not drop the second.")
	}
}

// TestClassifyMergeSlotPartitionsTheFourOutcomes pins the merge-slot classifier's
// arms, which the corpus cannot pin for itself.
//
// The gate above checks that the four counters ADD UP to MergeSlots. That is a
// partition check, and a partition survives any permutation of its parts: swap
// which arm increments Scalar and which increments Untyped and the sum is
// unchanged, so the gate stays green while the census reports the opposite of
// what happened. The corpus cannot separate them either — it drives Scalar at
// zero, so an untyped slot misfiled as scalar shows up as the counter that was
// already zero staying zero, and the residue the acceptance number reads quietly
// loses the slot it exists to surface.
//
// Each case therefore names an input whose bucket differs from at least one
// neighbour's, and the scavenged case carries a type that would otherwise be
// Typed so that domination is asserted rather than assumed.
func TestClassifyMergeSlotPartitionsTheFourOutcomes(t *testing.T) {
	t.Parallel()

	row := nljTestLayout("SLOT", "ID", "CATEGORY")

	for _, tc := range []struct {
		name      string
		slotType  values.Type
		scavenged bool
		want      mergeSlotClass
		because   string
	}{
		{
			name: "record type", slotType: values.Type(row), want: mergeSlotClassTyped,
			because: "a slot stating a ROW is the intended path for a leg — the whole " +
				"point of the quantifier stating what it flows",
		},
		{
			name: "stated non-record type", slotType: values.NotNullLong, want: mergeSlotClassScalar,
			because: "a STATED non-row type is the mixed-seed case the gate deliberately " +
				"admits, not a residue: something said what this slot holds and it is not " +
				"a row. Filing it as Untyped inflates the residue with slots that are fine",
		},
		{
			name: "unknown type", slotType: values.UnknownType, want: mergeSlotClassUntyped,
			because: "nothing stated a type at all. This is the residue — either a leg " +
				"whose row is lost or a struct-array whole-object element — and filing it " +
				"as Scalar hides a lost leg row inside a bucket nobody reads",
		},
		{
			name: "nil type", slotType: nil, want: mergeSlotClassUntyped,
			because: "an absent type is the same finding as an unstated one, and it must " +
				"not reach the default arm: a nil Type would panic on Code() if the untyped " +
				"arm stopped guarding for it",
		},
		{
			name: "scavenged, over a record type", slotType: values.Type(row), scavenged: true,
			want: mergeSlotClassScavenged,
			because: "scavenged DOMINATES. The slot holds a row, but the quantifier did " +
				"not state it — a baked reference in the select did — and reporting it as " +
				"Typed credits the quantifier with a row it never stated, which is exactly " +
				"the drift the census exists to catch",
		},
	} {
		if got := classifyMergeSlot(tc.slotType, tc.scavenged); got != tc.want {
			t.Errorf("classifyMergeSlot(%v, scavenged=%v) = %v, want %v — %s",
				tc.slotType, tc.scavenged, got, tc.want, tc.because)
		}
	}
}
