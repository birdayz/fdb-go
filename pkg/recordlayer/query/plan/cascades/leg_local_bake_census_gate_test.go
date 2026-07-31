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
// Each case below names the MESSAGE it expects, not merely a failure. Several
// of the checks overlap by construction — the identity partition is implied by
// the two cross-cuts and the outcome partition — so a case can red more than one
// check, and "the gate failed" alone would stop distinguishing a live assertion
// from a dropped one the moment two checks covered the same input.
func TestLegLocalBakeGateFailsOnEachViolation(t *testing.T) {
	t.Parallel()

	// healthy is a SYNTHETIC gate input: the state the gate must PASS, or every
	// red below is red for the uninteresting reason. It is shaped after a real
	// corpus run so that the reds are reds on a realistic population, but the gate
	// does not require it to EQUAL the corpus and this test does not check that it
	// does — nothing here reads the live census.
	//
	// It deliberately no longer claims to be the corpus's measured shape. That
	// claim was written three times and was stale within the same change each
	// time: the corpus population moves whenever a query is added anywhere in the
	// sqldriver suite, including by seeded controls added alongside this fixture.
	// A number that drifts under unrelated work is a number a comment cannot
	// carry. Whoever wants the live one runs:
	//
	//	go test ./pkg/relational/sqldriver/ -count=1 -v | grep "leg-local bakeability"
	//
	// What the shape has to keep is its STRUCTURE, and that is what the reds below
	// exercise: every read baked, none re-anchored, none declined, every one
	// stating an identity in its own leg's domain. That is a different structure
	// from the one this fixture originally carried (all reads re-anchored, none
	// stating an identity), and the difference is the whole of what phase 2 did.
	healthy := legLocalBakeCounters{
		Total: 174, Baked: 174, MergedReAnchor: 0, Declined: 0,
		// The CALL-SITE cut: which lowering reached the arm. Measured 174 / 0 —
		// every firing is the EXISTS site, none is the RFC-153 buried site. Two
		// documents assert that zero in prose (DIVERGENCES.md's sibling-alias entry
		// and TODO.md's CQ-53 phase-3 refutation 2), which is exactly why the gate
		// has to hold it rather than the prose.
		SiteExists: 174, SiteBuried: 0,
		// Vacuous at HEAD: these partition MergedReAnchor, which is 0. The gate
		// says so out loud rather than reporting a green check over nobody — the
		// VACUOUS direction below is what pins that.
		UntypedLeg: 0, ColumnAbsent: 0, LayoutAvailable: 0,
		// EVERY firing cut by what each read states about its OWN identity. This
		// is the cut the qualified-name channel's retirement rests on: every
		// leg-correlated read reaching the arm states its own slot, with no name
		// consulted anywhere. It partitions Total — which is FLOORED — so unlike
		// the layout cut above it cannot go vacuous without the floor firing
		// first, and it is cross-checked against the OUTCOME cut so that filing it
		// wholesale wrong is not a state both partitions accept.
		IdentityInLegDomain: 174, IdentityOtherDomain: 0, LazyNameOnly: 0,
		FlowedLegs: 960, DisagreeingLegs: 0, UnderivableLegs: 0,
		LegDerivations:  960,
		MergeSlots:      18358,
		MergeSlotsTyped: 17604, MergeSlotsScavenged: 4, MergeSlotsScalar: 0,
		MergeSlotsUntyped: 750,
	}
	floors := &LegLocalBakeFloors{Total: 12, SiteExists: 12, LegDerivations: 80, MergeSlots: 1800}

	var healthyOut strings.Builder
	if assertLegLocalBakeCounters(&healthyOut, healthy, floors) {
		t.Fatalf("the gate FAILS the corpus's own measured counter state:\n%s\n"+
			"  Every case below expects a red, so a baseline that is already red proves "+
			"nothing about any of them.", healthyOut.String())
	}

	for _, tc := range []struct {
		name   string
		mutate func(*legLocalBakeCounters)
		// wantMsgs is a LIST because the checks overlap: the cross-cuts below are
		// two readings of one dispatch, so the input that violates one violates the
		// other, and a case naming only one of them would go on passing after the
		// other was dropped.
		wantMsgs []string
	}{
		{
			// A leg whose reference members flow different rows. Java Verify-fails;
			// Go declines the leg and the read falls back to the qualified name, so
			// the cost is an underivable leg's for a strictly worse reason.
			name:     "a member-disagreement leg",
			mutate:   func(c *legLocalBakeCounters) { c.FlowedLegs--; c.DisagreeingLegs++ },
			wantMsgs: []string{"DisagreeingLegs = 1, want 0"},
		},
		{
			name:     "a leg whose layout cannot be derived",
			mutate:   func(c *legLocalBakeCounters) { c.FlowedLegs--; c.UnderivableLegs++ },
			wantMsgs: []string{"UnderivableLegs = 1, want 0"},
		},
		{
			// A read the arm re-anchored because its leg stated no type. The read
			// MOVES from Baked into MergedReAnchor rather than being conjured, so
			// Total and both other partitions stay exact and this case violates
			// only the zero it is written for.
			name: "a re-anchored read whose leg stated no type",
			mutate: func(c *legLocalBakeCounters) {
				c.Baked--
				c.MergedReAnchor++
				c.UntypedLeg++
			},
			wantMsgs: []string{"UntypedLeg = 1, want 0"},
		},
		{
			// A read that reached the arm stating NO identity at all. Asserted at
			// zero — the defect is at the PRODUCER that built the reference
			// unresolved, and no rewrite at this arm can supply what the producer
			// did not — and until this case existed nothing checked that the gate
			// would say so. Every other zero the census asserts had a case; this
			// one had none, which is the same shape of miss as the DisagreeingLegs
			// gap that motivated this whole test.
			name: "a read that states no identity at all",
			mutate: func(c *legLocalBakeCounters) {
				c.Baked--
				c.Declined++
				c.IdentityInLegDomain--
				c.LazyNameOnly++
			},
			wantMsgs: []string{"Declined = 1, want 0"},
		},
		{
			// The three per-leg outcomes must partition LegDerivations, or
			// UnderivableLegs counts the instrumented paths rather than the legs.
			name:     "a leg that leaves the derivation by an uncounted path",
			mutate:   func(c *legLocalBakeCounters) { c.LegDerivations++ },
			wantMsgs: []string{"but LegDerivations = 961"},
		},
		{
			// THE CALL-SITE ZERO. A leg-correlated read reaching the arm from the
			// RFC-153 buried lowering. The read MOVES between sites so Total and
			// every other partition stay exact and only the zero this case is
			// written for breaks — otherwise a passing case could be passing on the
			// partition check instead.
			//
			// This is the phase-3 refutation being held. The buried site measured
			// ZERO while DOING state a layout, which is why the entry's claim that
			// the arm is reached "without a stated merged layout on the
			// EXISTS-over-join and RFC-153 buried-leg paths" was wrong about half
			// its subject. Nothing checked that until this dimension existed.
			name: "a read reaching the arm from the RFC-153 buried lowering",
			mutate: func(c *legLocalBakeCounters) {
				c.SiteExists--
				c.SiteBuried++
			},
			wantMsgs: []string{"SiteBuried = 1, want 0"},
		},
		{
			// A firing whose site is counted into neither — the shape a THIRD entry
			// point into rebaseOuterLegValue would produce.
			name:     "a firing counted into Total but into no call site",
			mutate:   func(c *legLocalBakeCounters) { c.SiteExists-- },
			wantMsgs: []string{"SiteExists(173) + SiteBuried(0) = 173"},
		},
		{
			// The identity cut is carried along so the identity/Total partition
			// stays exact and the OUTCOME one is what breaks. The cross-cut breaks
			// with it and is named too: with the outcome partition broken the two
			// cuts stop coinciding, which is exactly when both messages are worth
			// having.
			name:   "a firing counted into Total and classified into no outcome",
			mutate: func(c *legLocalBakeCounters) { c.Total++; c.IdentityInLegDomain++ },
			wantMsgs: []string{
				"Declined(0) = 174, but Total = 175",
				"Baked(174) + MergedReAnchor(0) = 174, but IdentityInLegDomain = 175",
			},
		},
		{
			name:     "a re-anchored read with no reason",
			mutate:   func(c *legLocalBakeCounters) { c.MergedReAnchor++; c.Baked-- },
			wantMsgs: []string{"but MergedReAnchor = 1"},
		},
		{
			// The SECOND cut, over the SAME population the outcomes partition.
			// IdentityInLegDomain is the number of reads that can state their own
			// slot with no name consulted anywhere; if the three do not sum, that
			// number is a share of an unknown whole and the qualified-name
			// channel's retirement rests on an unmeasured quantity.
			name:   "a firing classified into no identity bucket",
			mutate: func(c *legLocalBakeCounters) { c.IdentityInLegDomain-- },
			wantMsgs: []string{
				"LazyNameOnly(0) = 173, but Total = 174",
				"Baked(174) + MergedReAnchor(0) = 174, but IdentityInLegDomain = 173",
			},
		},
		{
			// THE WHOLESALE MISFILING, in its smallest form: one firing whose
			// identity class does not match the arm that produced it. NOTHING the
			// two partitions check moves — Total is unchanged, every outcome
			// counter is unchanged, and the three identity counters still sum to
			// Total — so before the cross-cuts existed this state was green.
			//
			// It is the state that matters most, because at corpus scale it is what
			// a constant class at a census call site produces: every firing filed
			// under one class, IdentityInLegDomain inverted from the whole
			// population to none of it, and not one counter out of balance. The
			// number the qualified-name channel's retirement rests on would have
			// read its own opposite with the gate green.
			//
			// Both cross-cut messages are required. They are two readings of one
			// dispatch and this input reds both, so naming only one would leave the
			// other droppable in silence.
			name:   "a read filed under an identity its outcome contradicts",
			mutate: func(c *legLocalBakeCounters) { c.IdentityInLegDomain--; c.LazyNameOnly++ },
			wantMsgs: []string{
				"Baked(174) + MergedReAnchor(0) = 174, but IdentityInLegDomain = 173",
				"IdentityOtherDomain(0) + LazyNameOnly(1) = 1, but Declined = 0",
			},
		},
		{
			// The floors are what keep every zero above from holding vacuously.
			name:     "the leg-derivation population collapses",
			mutate:   func(c *legLocalBakeCounters) { *c = legLocalBakeCounters{} },
			wantMsgs: []string{"LegDerivations = 0, want >= 80"},
		},
		{
			// The merge-slot partition. MergeSlotsUntyped is an upper bound on the
			// one remaining path by which a leg's row is silently lost here, and a
			// bound is only a bound while its siblings account for the rest.
			name:     "a merge slot counted into no bucket",
			mutate:   func(c *legLocalBakeCounters) { c.MergeSlots++ },
			wantMsgs: []string{"but MergeSlots = 18359"},
		},
		{
			name: "the merge-slot population collapses",
			mutate: func(c *legLocalBakeCounters) {
				c.MergeSlots, c.MergeSlotsTyped, c.MergeSlotsScavenged, c.MergeSlotsUntyped = 0, 0, 0, 0
			},
			wantMsgs: []string{"MergeSlots = 0, want >= 1800"},
		},
		{
			name: "the read population collapses",
			mutate: func(c *legLocalBakeCounters) {
				c.Total, c.Baked, c.MergedReAnchor, c.LayoutAvailable = 0, 0, 0, 0
				c.IdentityInLegDomain = 0
			},
			wantMsgs: []string{"Total = 0, want >= 12"},
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
		for _, want := range tc.wantMsgs {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: the gate failed, but not for the stated reason — want a message "+
					"containing %q, got:\n%s\n"+
					"  A red for the wrong invariant is how a check stays green in the one "+
					"direction it was written for.", tc.name, want, out.String())
			}
		}
	}
}

// A partition over an EMPTY denominator holds as 0 == 0, and a green check over
// nobody reads exactly like a checked one. That is not a hypothetical: the
// identity cut partitioned the merged re-anchor, the re-anchor's population went
// to zero when the arm stopped degrading reads, and the assertion over it went on
// reporting green for a cut that described nobody — while the reads it should
// have described went unmeasured on the one axis the retirement decision rests
// on. The cut was repointed at Total, which is floored. The layout cut cannot be
// repointed (its three reasons are genuinely about re-anchored reads), so it
// announces its own vacuity instead, and this is the direction that pins the
// announcement.
//
// The notice is NOT a failure — a zero re-anchor population is the arm's correct
// present state — so both facts are asserted together: the notice is emitted AND
// the gate still passes.
func TestLegLocalBakeGateAnnouncesVacuousPartitions(t *testing.T) {
	t.Parallel()

	// The arm's present STRUCTURE: a live read population, no re-anchors. Shaped
	// after a corpus run for realism, not asserted to equal one — see the fixture
	// in TestLegLocalBakeGateFailsOnEachViolation for why a comment cannot carry
	// the live number.
	live := legLocalBakeCounters{
		Total: 174, Baked: 174,
		SiteExists:          174,
		IdentityInLegDomain: 174,
		FlowedLegs:          960, LegDerivations: 960,
		MergeSlots: 18358, MergeSlotsTyped: 17604, MergeSlotsScavenged: 4,
		MergeSlotsUntyped: 750,
	}
	floors := &LegLocalBakeFloors{Total: 12, SiteExists: 12, LegDerivations: 80, MergeSlots: 1800}

	var out strings.Builder
	if assertLegLocalBakeCounters(&out, live, floors) {
		t.Fatalf("the gate FAILED the corpus's present shape:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "VACUOUS") ||
		!strings.Contains(out.String(), "partitions MergedReAnchor, which is 0") {
		t.Errorf("the layout partition holds as 0 == 0 over an empty MergedReAnchor and "+
			"the gate said nothing about it:\n%s\n"+
			"  A partition over an empty denominator proves nothing, and reported without "+
			"that caveat it reads as a live check. That misreading is what let the "+
			"identity cut sit green over a population it had itself emptied.", out.String())
	}
	// And the notice must NOT fire for a partition that is genuinely populated —
	// otherwise it is noise and gets tuned out, which is the same outcome as
	// silence.
	if strings.Contains(out.String(), "partitions MergeSlots, which is 0") {
		t.Errorf("the gate called a POPULATED partition (MergeSlots = %d) vacuous:\n%s\n"+
			"  A vacuity notice that fires over live data is one nobody reads.",
			live.MergeSlots, out.String())
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

// classifyLegReadIdentity's three arms, pinned separately from the counter
// mutation for the reason its sibling above is: this classifier is what decides
// whether a leg-correlated read can state its own slot WITHOUT a name, and that
// is the fact CQ-53 phase 2's whole disposition rests on. A classifier that
// collapsed two arms would report the corpus convertible (or unconvertible)
// while measuring something else, and nothing downstream would notice.
//
// All three arms are REACHED. They were not: the classifier's only caller passed
// a literal `true` from inside a branch guarded on that same value, so two of
// the three could not be entered by any input the program constructs, and this
// test was pinning behaviour that existed nowhere else. The class is now
// computed ONCE from the read, BEFORE the arm dispatch, and the arm dispatches
// on it — so a read that declines reaches the arm that files it as declining.
//
// The (hasResolved=false, identityInLegDomain=true) combination is deliberately
// NOT a case. legSlotIdentity answers through fv.CorrelatedIdentityIn, which
// cannot answer for a reference carrying no resolved path, so that input does
// not exist — and a case asserting the ordering over it would pin the
// classifier's behaviour on data no caller can hand it, which is the same defect
// this test was just repaired for.
func TestClassifyLegReadIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                string
		hasResolved         bool
		identityInLegDomain bool
		want                legReadIdentity
		because             string
	}{
		{
			name:        "states an identity in the leg's own domain",
			hasResolved: true, identityInLegDomain: true,
			want: legReadIdentityInLegDomain,
			because: "the read carries a non-negative ordinal in a known domain that IS " +
				"the leg's row layout, so nothing has to be minted — no name is consulted " +
				"anywhere. This is the corpus's ENTIRE population — every firing, none in " +
				"either residue — and it is the measurement the qualified-name channel's " +
				"retirement rests on",
		},
		{
			name:        "resolved, but not against the leg's layout",
			hasResolved: true, identityInLegDomain: false,
			want: legReadIdentityOtherDomain,
			because: "a resolved path the leg's layout cannot read — a different domain, a " +
				"fused multi-accessor path, or a name-only negative ordinal. This was the " +
				"whole population before the resolver's correlated mint carried the " +
				"source's row: the ordinals indexed the leg correctly, but the reference's " +
				"own QuantifiedObjectValue was UnknownType, so the frontier legSlotIdentity " +
				"derived from it was unknown and OrdinalIn failed closed",
		},
		{
			name:        "no resolved path at all",
			hasResolved: false, identityInLegDomain: false,
			want: legReadIdentityLazyNameOnly,
			because: "a lazy carrier. Its display name is the only identity it states, so " +
				"the ONLY bake available to it is the deleted name-keyed one; a residue " +
				"here closes at the producer that minted it unresolved, never at this arm",
		},
	} {
		if got := classifyLegReadIdentity(tc.hasResolved, tc.identityInLegDomain); got != tc.want {
			t.Errorf("classifyLegReadIdentity(hasResolved=%v, identityInLegDomain=%v) = %v, want %v — %s",
				tc.hasResolved, tc.identityInLegDomain, got, tc.want, tc.because)
		}
	}
}
