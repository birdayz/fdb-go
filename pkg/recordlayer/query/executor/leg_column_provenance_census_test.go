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
	stated := values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("C"), "C", 0, 2)
	unstated := values.RecordTypeLeg{Name: "C", Start: 0, Width: 2}
	diverged := values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("Q$7"), "C", 0, 2)

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

	// A state that HOLDS: the seven outcomes partition Calls and nothing diverged.
	ok := legColumnProvenanceCounters{
		Calls: 52, FlatHit: 40, NotDotted: 8, DottedHitIdentityAvailable: 4,
	}
	var b strings.Builder
	if assertLegColumnProvenanceCounters(&b, ok) {
		t.Fatalf("a partitioning, divergence-free census FAILED the gate:\n%s", b.String())
	}

	// A state that must FAIL: a call left the reader by a path with no counter.
	b.Reset()
	gap := ok
	gap.Calls = 53
	if !assertLegColumnProvenanceCounters(&b, gap) {
		t.Fatal("the gate accepted a census whose outcomes do not sum to Calls. Every " +
			"share it prints — including the one that decides whether this reader can be " +
			"re-keyed by identity — is then a share of an unknown whole.")
	}

	// And the one ZERO: a leg whose text and whose stated alias disagree.
	b.Reset()
	div := ok
	div.DottedHitIdentityAvailable = 3
	div.DottedHitIdentityDiverged = 1
	if !assertLegColumnProvenanceCounters(&b, div) {
		t.Fatal("the gate accepted a DIVERGED dotted hit. That is not a residue to " +
			"shrink but a contradiction: two keys for one leg, disagreeing, with only " +
			"the weaker one consulted — one of them is already resolving to the wrong " +
			"window.")
	}
	if !strings.Contains(b.String(), "DottedHitIdentityDiverged") {
		t.Fatalf("the divergence failure does not name the counter that fired:\n%s", b.String())
	}
}
