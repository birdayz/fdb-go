package cascades

import (
	"strings"
	"testing"
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
		LegDerivations: 848,
	}
	floors := &LegLocalBakeFloors{Total: 12, LegDerivations: 80}

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
