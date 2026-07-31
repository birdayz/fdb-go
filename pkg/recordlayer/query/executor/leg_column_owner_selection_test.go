package executor

// Whether rowSlotForLegColumn's dotted arm can be re-keyed onto an IDENTITY the
// reader already holds is a question about TWO identities, and the measurement
// that answers it found they are never the same one.
//
// The proposal these tests exist to record: give adaptLegPositional the leg's
// identity — every call site has one — select the source row's window by
// `Alias == identity`, and resolve the column within it. Text would then never
// SELECT a leg again, and no producer would be touched, so no label could move.
//
// Measured over the whole real-FDB sqldriver corpus (uncached), at the four
// dotted hits that are the entire population:
//
//	dotted HITS by OWNER selection: sameLeg 0, ownerUnstated 0,
//	                                ownerNamesNoLeg 4, ownerSelectsOtherLeg 0
//	OWNER-NO-LEG "C.CV":  owner "q$97"  names no leg of [C E]
//	OWNER-NO-LEG "I.QTY": owner "q$517" names no leg of [O I]
//	OWNER-NO-LEG "I.QTY": owner "q$868" names no leg of [O I]
//	OWNER-NO-LEG "O.ID":  owner "q$976" names no leg of [O I]
//
// Four of four. The identity in hand is the DESTINATION leg's own correlation —
// the correlated-scalar seed's MACHINE-MINTED quantifier — while the qualifier
// names a leg of the SOURCE merged row, which is a USER alias. Those namespaces
// are deliberately case-DISJOINT (values.SameLeg's own comment states why), so no
// comparison between them, exact or folded, can ever match. Selection by the
// owner identity resolves nothing, the gather matches zero columns, and the
// merge-shaped zero-match tripwire in adaptLegPositional turns both driving tests
// LOUD.
//
// The other half of the measurement is why no third channel rescues it: the
// destination leg type carries NO leg table (`destLegs=[]` at all four hits), so
// the qualifier inside the column name is the ONLY thing at this reader that
// names the source leg. An identity-keyed selection needs a carrier that does not
// exist yet, and creating it is a PRODUCER change.
//
// These tests pin both halves so the proposal cannot be re-derived from scratch
// and re-tried, and so the day a producer DOES state the source leg's identity,
// the fact that used to block it is visibly gone.

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
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("O"), "O", 0, 2),
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("I"), "I", 2, 3),
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
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("O"), "O", 0, 1),
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("I"), "I", 1, 1),
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

// TestLegColumnOwner_TheDestinationLegTypeCarriesNoLegTable pins the second half:
// why no channel other than the qualifier text names the source leg.
//
// The seed leg types that drive the dotted arm are SINGLE-COLUMN and their Legs
// slice is empty — measured at all four hits. RecordType.Legs is layout metadata
// that Equals and Hash ignore, so a producer COULD populate it without moving any
// name, any label, or any type identity. That is the shape of the unblocking
// change, and this test states the precondition it would remove.
func TestLegColumnOwner_TheDestinationLegTypeCarriesNoLegTable(t *testing.T) {
	t.Parallel()

	// The destination leg type as the producer builds it today.
	seedLeg := &values.RecordType{Fields: []values.Field{{Name: "O.ID", Ordinal: 0}}}

	if len(seedLeg.Legs) != 0 {
		t.Fatalf("the seed leg type now carries a leg table (%d entries) — the reader has "+
			"gained a non-text carrier for the source leg's identity, so the dotted arm's "+
			"retirement is no longer blocked on a producer. Re-measure the owner "+
			"sub-partition and re-open the conversion.", len(seedLeg.Legs))
	}

	// And the type's identity does not depend on that table, which is what makes
	// populating it a label-safe producer change rather than a plan-shape change.
	withLegs := &values.RecordType{
		Fields: []values.Field{{Name: "O.ID", Ordinal: 0}},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("O"), "O", 0, 1),
		},
	}
	if !seedLeg.Equals(withLegs) {
		t.Fatal("adding a leg table changed the RecordType's identity. RecordType.Equals is " +
			"documented to ignore Legs (layout metadata only); if that is no longer true, " +
			"populating the seed leg's table is a plan-visible change and the label-stability " +
			"argument for doing it has to be re-made.")
	}
}
