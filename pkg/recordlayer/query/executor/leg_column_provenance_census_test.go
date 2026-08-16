package executor

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The provenance census's DECISION, exercised without touching process-global
// state — and its gate, exercised on the counter states that must fail.
//
// A census is a claim about which states are reportable and which are wrong, and
// a claim reachable only by driving the executor into a defective state is a
// claim nothing pins. Every bucket below can now be shown, and the one zero the
// gate asserts can be shown red.
func TestLegColumnProvenanceClassification(t *testing.T) {
	t.Parallel()

	legged := func(legs ...values.RecordTypeLeg) *values.RecordType {
		return &values.RecordType{
			Fields: []values.Field{
				{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "CV", FieldType: values.NotNullLong, Ordinal: 1},
			},
			Legs: legs,
		}
	}
	stated := values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("C"), "C", 0, 2)
	unstated := values.RecordTypeLeg{Name: "C", Start: 0, Width: 2}
	diverged := values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("Q$7"), "C", 0, 2)

	for _, tc := range []struct {
		name     string
		rt       *values.RecordType
		column   string
		flatHit  bool
		flatAmbi bool
		matched  *values.RecordTypeLeg
		qual     string
		want     legColumnProvenanceClass
		wantWhen string
	}{
		{
			"flat hit dominates", legged(stated), "C.CV", true, false, &stated, "C",
			legColumnProvenanceFlatHit,
			"the row's own type declared the name, so the dotted arm was never reached — " +
				"nothing about the leg table may be held against a call that did not consult it",
		},
		{
			"undotted miss", legged(stated), "CV", false, false, nil, "",
			legColumnProvenanceNotDotted,
			"a miss on an unqualified name is not a name DECISION; folding it in would " +
				"inflate the population the re-keying claim is a share of",
		},
		{
			"dotted over a legless row", &values.RecordType{}, "C.CV", false, false, nil, "C",
			legColumnProvenanceNoLegs,
			"there was no leg table to consult",
		},
		{
			"dotted miss", legged(stated), "Z.CV", false, false, nil, "Z",
			legColumnProvenanceMiss,
			"legs were present and none declared the column",
		},
		{
			"identity available", legged(stated), "C.CV", false, false, &stated, "C",
			legColumnProvenanceIdentityAvailable,
			"the matched leg states an alias naming the same thing, so an identity-keyed " +
				"reader would answer identically",
		},
		{
			"identity unstated", legged(unstated), "C.CV", false, false, &unstated, "C",
			legColumnProvenanceIdentityUnstated,
			"the matched leg states NO alias, so an identity-keyed reader would MISS here " +
				"— this is the population that blocks re-keying, and it closes at the producer",
		},
		{
			"identity diverged", legged(diverged), "C.CV", false, false, &diverged, "C",
			legColumnProvenanceIdentityDiverged,
			"the leg's text and its stated alias name DIFFERENT things and the lookup " +
				"resolved on the text — two keys for one leg, disagreeing",
		},
		{
			"bare ambiguous name", legged(stated), "ID", false, true, nil, "",
			legColumnProvenanceFlatAmbiguous,
			"the row declares the name twice and it carries no qualifier, so nothing can " +
				"choose. This used to classify as NotDotted — indistinguishable from an " +
				"ordinary miss — which is exactly why the reader could start declining rows " +
				"it used to bind with the census reporting no change at all",
		},
		{
			"dotted, flat-ambiguous, no leg window answered", legged(stated), "Z.CV", false, true, nil, "Z",
			legColumnProvenanceFlatAmbiguous,
			"the qualifier resolved no window, so the flat ambiguity is what stands — " +
				"reporting this as a plain DottedMiss would hide that the name was present " +
				"twice rather than absent",
		},
		{
			"dotted, flat-ambiguous, but a leg window ANSWERED", legged(stated), "C.CV", false, true, &stated, "C",
			legColumnProvenanceIdentityAvailable,
			"a qualifier that names a leg window is strictly more information than the flat " +
				"namespace carries, so the answer is not a guess and the flat ambiguity is " +
				"resolved rather than merely outvoted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyLegColumnProvenance(tc.rt, tc.column, tc.flatHit, tc.flatAmbi, tc.matched, tc.qual)
			if got != tc.want {
				t.Fatalf("classified as %d, want %d — %s", got, tc.want, tc.wantWhen)
			}
		})
	}
}

func TestLegColumnProvenanceGate(t *testing.T) {
	t.Parallel()

	// EVERY floor is gone, and each went for its own reason. The
	// DottedHitIdentityAvailable floor went when RFC-212 §11.3 retitled the
	// producer and drove the dotted arm to zero; the Calls floor went when the
	// exact-ordinal seed removed adaptLegPositional's permutation gather — the
	// reader's only driver — from the live path. Both are replaced by HARD ZEROS
	// with the direction flipped: growth is the alarm now.
	floors := &LegColumnProvenanceFloors{}

	// A state that HOLDS: the reader is not entered at all, which is the retired
	// steady state, and the partition holds trivially over it.
	ok := legColumnProvenanceCounters{}
	var b strings.Builder
	if assertLegColumnProvenanceCounters(&b, ok, floors) {
		t.Fatalf("a partitioning, divergence-free census FAILED the gate:\n%s", b.String())
	}

	// A state that must FAIL: dotted hits reached the reader but no owner verdict
	// was recorded for them. Without this the sub-partition can quietly stop being
	// filled and its zeros — including "no dotted hit could have been resolved by
	// identity", the finding that blocks the conversion — hold over nothing.
	// A state that must FAIL: the dotted arm ANSWERED. Zero is the steady state
	// after the retitling, so any answer means a producer is again naming a leg
	// type's only column with a dot-containing title.
	b.Reset()
	revived := legColumnProvenanceCounters{Calls: 52, FlatHit: 40, NotDotted: 8}
	revived.DottedHitIdentityAvailable = 4
	revived.DottedHitOwnerNamesNoLeg = 4
	if !assertLegColumnProvenanceCounters(&b, revived, floors) {
		t.Fatal("the dotted arm answered 4 times and the gate PASSED.\n" +
			"  RFC-212 §11.3 drove this population to zero by retitling the producer;\n" +
			"  zero is now the steady state and the dangerous direction is GROWTH.\n" +
			"  WHAT THIS RE-ARMS: a producer naming a quantifier's flowed column with a\n" +
			"  title that splits at a dot, so a reference resolves through a LEG and\n" +
			"  COLUMN the leg does not have — silently, because the arm answers.")
	}

	// The OWNER sub-partition must still be checked, and it is only meaningful
	// over a state that HAS dotted hits — so it is built from the revived one.
	// Such a state also trips the new hard zero, which is why the MESSAGE is
	// asserted rather than just the boolean: otherwise this case would pass on
	// the zero alone and stop testing the sub-partition entirely.
	b.Reset()
	ownerGap := revived
	ownerGap.DottedHitOwnerNamesNoLeg = 0
	if !assertLegColumnProvenanceCounters(&b, ownerGap, floors) {
		t.Fatal("the gate accepted dotted hits with no owner verdict recorded for any " +
			"of them.")
	}
	if !strings.Contains(b.String(), "owner sub-partition") {
		t.Fatalf("the gate failed, but NOT on the owner sub-partition — so that check\n"+
			"  is no longer exercised and could stop being filled unnoticed:\n%s", b.String())
	}

	// A state that must FAIL: a call left the reader by a path with no counter.
	b.Reset()
	gap := ok
	gap.Calls = 53
	if !assertLegColumnProvenanceCounters(&b, gap, floors) {
		t.Fatal("the gate accepted a census whose outcomes do not sum to Calls. Every " +
			"share it prints — including the one that decides whether this reader can be " +
			"re-keyed by identity — is then a share of an unknown whole.")
	}

	// And the one ZERO: a leg whose text and whose stated alias disagree.
	b.Reset()
	div := ok
	div.DottedHitIdentityAvailable = 3
	div.DottedHitIdentityDiverged = 1
	if !assertLegColumnProvenanceCounters(&b, div, floors) {
		t.Fatal("the gate accepted a DIVERGED dotted hit. That is not a residue to " +
			"shrink but a contradiction: two keys for one leg, disagreeing, with only " +
			"the weaker one consulted — one of them is already resolving to the wrong " +
			"window.")
	}
	if !strings.Contains(b.String(), "DottedHitIdentityDiverged") {
		t.Fatalf("the divergence failure does not name the counter that fired:\n%s", b.String())
	}

	// And the OTHER zero, whose alarm direction is also GROWTH: a leg-type column
	// name the source row declares twice. adaptLegPositional now FAILS the query
	// on this rather than binding an arbitrary duplicate or leaving the slot nil,
	// so a non-zero here is a real user-visible error and the gate must say so
	// rather than let it pass as ordinary traffic.
	b.Reset()
	ambi := ok
	ambi.NotDotted = 7
	ambi.FlatAmbiguous = 1
	if !assertLegColumnProvenanceCounters(&b, ambi, floors) {
		t.Fatal("the gate accepted a FLAT-AMBIGUOUS bind. The reader was handed a leg " +
			"column name its source row declares at two slots: binding either is a " +
			"wrong-leg read, and skipping it answers NULL for a column that has a value. " +
			"Zero is the measured steady state and GROWTH is the alarm — a non-zero means " +
			"a producer started emitting a leg type whose column names are ambiguous " +
			"against the merged row they are adapted against.")
	}
	if !strings.Contains(b.String(), "FlatAmbiguous = 1, want 0") {
		t.Fatalf("the flat-ambiguity failure does not name the counter that fired:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "GROWTH") {
		t.Fatalf("the flat-ambiguity failure does not state which DIRECTION is the alarm. "+
			"A guard whose expected value is zero is read as dead-instrument news unless "+
			"it says growth is the danger:\n%s", b.String())
	}
}

// The reader is RETIRED: adaptLegPositional's layout-permutation gather is its
// only driver, and the exact-ordinal seed — which bakes against the chosen
// physical leg layout, as Java's translateCorrelations does — makes every leg
// row pass positionalMatchesLegType, so the gather is skipped.
//
// The guard that watched this population for COLLAPSE therefore inverts. There
// is nothing left to floor, and the zero must hold on a narrowed run too: a
// revival alarm that only fires on full runs stops watching exactly when someone
// is iterating on the seed that would revive it.
func TestLegColumnProvenanceGateRetirement(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	if assertLegColumnProvenanceCounters(&b, legColumnProvenanceCounters{}, &LegColumnProvenanceFloors{}) {
		t.Fatalf("the RETIRED steady state (nothing reaches the reader) failed the gate:\n%s",
			b.String())
	}

	b.Reset()
	if !assertLegColumnProvenanceCounters(&b,
		legColumnProvenanceCounters{Calls: 1, FlatHit: 1}, &LegColumnProvenanceFloors{}) {
		t.Fatal("a single call to the retired reader PASSED the gate.\n" +
			"  The permutation gather is entered only when a leg's seeded type and the row\n" +
			"  its physical leg emits are permutations of each other, which the exact\n" +
			"  ordinal seed is supposed to make impossible. One call says a seed stopped\n" +
			"  baking against the chosen physical layout — a finding about the PRODUCER.")
	}
	if !strings.Contains(b.String(), "want 0") {
		t.Errorf("the revival failure does not state the expectation:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "revival") {
		t.Errorf("the revival failure does not state which DIRECTION is the alarm. A guard "+
			"whose expected value is zero reads as dead-instrument news unless it says "+
			"growth is the danger:\n%s", b.String())
	}

	// Floors dropped (a narrowed run): the retirement zero must still hold, and a
	// real contradiction must still fail.
	b.Reset()
	if assertLegColumnProvenanceCounters(&b, legColumnProvenanceCounters{}, nil) {
		t.Errorf("with floors dropped, the retired steady state failed:\n%s", b.String())
	}
	b.Reset()
	if !assertLegColumnProvenanceCounters(&b,
		legColumnProvenanceCounters{Calls: 1, DottedHitIdentityDiverged: 1}, nil) {
		t.Error("with floors dropped, a DIVERGED dotted hit passed. Dropping the " +
			"whole-suite calibrations must not drop the contradictions.")
	}
}
