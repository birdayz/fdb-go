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

	// The DottedHitIdentityAvailable FLOOR is gone: RFC-212 §11.3 retitled the
	// producer and the dotted arm now answers ZERO times over the corpus, so a
	// floor on that population is unsatisfiable. It is replaced by a HARD ZERO —
	// the direction flipped, and growth is the alarm now.
	floors := &LegColumnProvenanceFloors{Calls: 1}

	// A state that HOLDS: the seven outcomes partition Calls, nothing diverged,
	// and the dotted arm answers NOTHING — the post-retitling steady state.
	ok := legColumnProvenanceCounters{
		Calls: 52, FlatHit: 44, NotDotted: 8,
	}
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
	revived := ok
	revived.FlatHit = 40
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

// The census's whole finding is a pair of SMALL numbers — the dotted arm answers
// four times in the corpus, and all four have an identity available — so a
// population that disappears reports the identical shape as one that is healthy:
// the partition holds as 0 == 0, the divergence zero holds vacuously, and "0 of
// 0 are identity-available" reads on a report exactly like "4 of 4".
//
// The floors are the only thing that separates those, and they are dropped on a
// narrowed run, so both directions get asserted: the floors must fire on an
// empty population, and dropping them must not drop the partition or the zero.
func TestLegColumnProvenanceGateFloors(t *testing.T) {
	t.Parallel()

	floors := &LegColumnProvenanceFloors{Calls: 1, DottedHitIdentityAvailable: 1}

	var b strings.Builder
	if !assertLegColumnProvenanceCounters(&b, legColumnProvenanceCounters{}, floors) {
		t.Fatal("an EMPTY census passed the gate. Every assertion above it holds " +
			"vacuously at zero population, so a reader that stopped being reached " +
			"reports as a reader with nothing wrong.")
	}
	if !strings.Contains(b.String(), "Calls = 0, want >= 1") {
		t.Errorf("the empty-population failure does not name Calls:\n%s", b.String())
	}

	// Calls alone does not cover it: the FLAT arm carries most of the traffic, so
	// the denominator can stay healthy while the arm the census exists for goes
	// silent. That is the state a Calls-only floor would pass.
	b.Reset()
	flatOnly := legColumnProvenanceCounters{Calls: 52, FlatHit: 44, NotDotted: 8}
	if !assertLegColumnProvenanceCounters(&b, flatOnly, floors) {
		t.Fatal("a census whose DOTTED arm answered zero times passed the gate on the " +
			"strength of its flat-hit traffic. The dotted arm is the only thing this " +
			"census measures; its disappearance is the failure, not the flat arm's health.")
	}
	if !strings.Contains(b.String(), "DottedHitIdentityAvailable = 0, want >= 1") {
		t.Errorf("the silent-dotted-arm failure does not name the counter:\n%s", b.String())
	}

	// Floors dropped (a narrowed run): an empty census must PASS, and a real
	// contradiction must still FAIL.
	b.Reset()
	if assertLegColumnProvenanceCounters(&b, legColumnProvenanceCounters{}, nil) {
		t.Errorf("with floors dropped, an EMPTY census failed:\n%s\n"+
			"  A narrowed -test.run reaches this reader zero times; failing there would "+
			"make every focused run red.", b.String())
	}
	b.Reset()
	if !assertLegColumnProvenanceCounters(&b,
		legColumnProvenanceCounters{Calls: 1, DottedHitIdentityDiverged: 1}, nil) {
		t.Error("with floors dropped, a DIVERGED dotted hit passed. The floors describe " +
			"a whole-suite population; the zero describes a contradiction, and dropping " +
			"the first must not drop the second.")
	}
}
