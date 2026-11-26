package querycensus

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mintedSeedOwner is the shape of the identity every call site actually holds at
// a dotted hit: a planner-minted quantifier from the LOWERCASE machine namespace.
const mintedSeedOwner = "q$976"

// TestLegColumnOwner_MintedSeedOwnerNamesNoSourceLeg pins the fact that refuted
// the identity-keyed selection proposal.
//
// The fixture is the corpus witness (TestFDB_CorrelatedScalarJoinInner): a merged
// source row with legs O and I under their user aliases, and an owner that is the
// seed's own minted quantifier.
func TestLegColumnOwner_MintedSeedOwnerNamesNoSourceLeg(t *testing.T) {
	t.Parallel()

	merged := &values.RecordType{
		Fields: []values.Field{
			{Name: "ID", Ordinal: 0},
			{Name: "CUSTOMER_ID", Ordinal: 1},
			{Name: "ID", Ordinal: 2},
			{Name: "ORDER_ID", Ordinal: 3},
			{Name: "QTY", Ordinal: 4},
		},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("O"), "O", 0, 2),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("I"), "I", 2, 3),
		},
	}
	textChose := &merged.Legs[1] // what "I.QTY" resolves to today

	owner := values.NamedCorrelationIdentifier(mintedSeedOwner)
	if got := classifyLegColumnOwner(merged, textChose, owner); got != legColumnOwnerNamesNoLeg {
		t.Fatalf("classifyLegColumnOwner(minted owner %q over legs [O I]) = %v, want NamesNoLeg.\n"+
			"  The measurement this test records found 4 of 4 corpus dotted hits in that\n"+
			"  bucket: the identity a call site holds is the DESTINATION leg's minted\n"+
			"  quantifier and the source row's legs are USER aliases, so selection by\n"+
			"  Alias == owner resolves nothing.\n"+
			"  If this now says SameLeg, a producer has started stating the source leg's\n"+
			"  identity where the reader can see it — which is exactly the change that\n"+
			"  unblocks the identity-keyed conversion. Re-run the corpus census and read\n"+
			"  the owner sub-partition before assuming it.", mintedSeedOwner, got)
	}

	// The two namespaces cannot be bridged by relaxing the comparison, which is
	// the first thing anyone will try. SameLeg is exact by design; even a folding
	// comparison finds no candidate, because no leg here is named anything like a
	// minted quantifier.
	for i := range merged.Legs {
		if values.SameLeg(merged.Legs[i].Alias, owner) {
			t.Fatalf("leg %q matched the minted owner %q — the fixture no longer has the "+
				"disjoint namespaces the finding is about", merged.Legs[i].Name, mintedSeedOwner)
		}
	}
}

// TestLegColumnOwner_ASourceLegOwnerWouldSelect is the positive control.
//
// Without it the test above passes for the wrong reason — a classifier that
// always answered NamesNoLeg would satisfy it — and the finding would read as
// "identity selection never works" rather than "the identity this reader is
// handed is not the one that would work".
func TestLegColumnOwner_ASourceLegOwnerWouldSelect(t *testing.T) {
	t.Parallel()

	merged := &values.RecordType{
		Fields: []values.Field{{Name: "ID", Ordinal: 0}, {Name: "QTY", Ordinal: 1}},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("O"), "O", 0, 1),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("I"), "I", 1, 1),
		},
	}

	if got := classifyLegColumnOwner(merged, &merged.Legs[1],
		values.NamedCorrelationIdentifier("I")); got != legColumnOwnerSameLeg {
		t.Fatalf("an owner naming the SOURCE leg the text chose classified as %v, want SameLeg — "+
			"the classifier cannot distinguish the blocked case from the working one", got)
	}
	if got := classifyLegColumnOwner(merged, &merged.Legs[1],
		values.NamedCorrelationIdentifier("O")); got != legColumnOwnerOtherLeg {
		t.Fatalf("an owner naming a DIFFERENT source leg classified as %v, want OtherLeg — "+
			"this is the outcome that would make a conversion a wrong-window read rather "+
			"than a refactor, and it must be distinguishable", got)
	}
	if got := classifyLegColumnOwner(merged, &merged.Legs[0],
		values.CorrelationIdentifier{}); got != legColumnOwnerUnstated {
		t.Fatalf("a zero owner classified as %v, want Unstated", got)
	}
}
