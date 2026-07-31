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
				{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
				{Name: "CV", FieldType: values.UnknownType, Ordinal: 1},
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
		matched  *values.RecordTypeLeg
		qual     string
		want     legColumnProvenanceClass
		wantWhen string
	}{
		{
			"flat hit dominates", legged(stated), "C.CV", true, &stated, "C",
			legColumnProvenanceFlatHit,
			"the row's own type declared the name, so the dotted arm was never reached — " +
				"nothing about the leg table may be held against a call that did not consult it",
		},
		{
			"undotted miss", legged(stated), "CV", false, nil, "",
			legColumnProvenanceNotDotted,
			"a miss on an unqualified name is not a name DECISION; folding it in would " +
				"inflate the population the re-keying claim is a share of",
		},
		{
			"dotted over a legless row", &values.RecordType{}, "C.CV", false, nil, "C",
			legColumnProvenanceNoLegs,
			"there was no leg table to consult",
		},
		{
			"dotted miss", legged(stated), "Z.CV", false, nil, "Z",
			legColumnProvenanceMiss,
			"legs were present and none declared the column",
		},
		{
			"identity available", legged(stated), "C.CV", false, &stated, "C",
			legColumnProvenanceIdentityAvailable,
			"the matched leg states an alias naming the same thing, so an identity-keyed " +
				"reader would answer identically",
		},
		{
			"identity unstated", legged(unstated), "C.CV", false, &unstated, "C",
			legColumnProvenanceIdentityUnstated,
			"the matched leg states NO alias, so an identity-keyed reader would MISS here " +
				"— this is the population that blocks re-keying, and it closes at the producer",
		},
		{
			"identity diverged", legged(diverged), "C.CV", false, &diverged, "C",
			legColumnProvenanceIdentityDiverged,
			"the leg's text and its stated alias name DIFFERENT things and the lookup " +
				"resolved on the text — two keys for one leg, disagreeing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyLegColumnProvenance(tc.rt, tc.column, tc.flatHit, tc.matched, tc.qual)
			if got != tc.want {
				t.Fatalf("classified as %d, want %d — %s", got, tc.want, tc.wantWhen)
			}
		})
	}
}

func TestLegColumnProvenanceGate(t *testing.T) {
	t.Parallel()

	floors := &LegColumnProvenanceFloors{Calls: 1, DottedHitIdentityAvailable: 1}

	// A state that HOLDS: the seven outcomes partition Calls, nothing diverged, and
	// the OWNER sub-partition accounts for every dotted hit. The owner numbers are
	// the corpus's own — four hits, none of which an identity-keyed selection could
	// have resolved, because the identity the reader holds names no leg of the
	// source row.
	ok := legColumnProvenanceCounters{
		Calls: 52, FlatHit: 40, NotDotted: 8, DottedHitIdentityAvailable: 4,
		DottedHitOwnerNamesNoLeg: 4,
	}
	var b strings.Builder
	if assertLegColumnProvenanceCounters(&b, ok, floors) {
		t.Fatalf("a partitioning, divergence-free census FAILED the gate:\n%s", b.String())
	}

	// A state that must FAIL: dotted hits reached the reader but no owner verdict
	// was recorded for them. Without this the sub-partition can quietly stop being
	// filled and its zeros — including "no dotted hit could have been resolved by
	// identity", the finding that blocks the conversion — hold over nothing.
	b.Reset()
	ownerGap := ok
	ownerGap.DottedHitOwnerNamesNoLeg = 0
	if !assertLegColumnProvenanceCounters(&b, ownerGap, floors) {
		t.Fatal("the gate accepted 4 dotted hits with no owner verdict recorded for any " +
			"of them. The owner sub-partition is what says whether an identity-keyed " +
			"selection would resolve the same window; unfilled, it reads as all-zero, " +
			"which is indistinguishable from a measured all-miss.")
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
