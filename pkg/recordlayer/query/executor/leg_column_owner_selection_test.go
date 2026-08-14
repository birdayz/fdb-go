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

// TestLegColumnOwner_TheDestinationLegTypeCarriesNoLegTable pins the second half:
// why no channel other than the qualifier text names the source leg.
//
// The seed leg types that drive the dotted arm are SINGLE-COLUMN and their Legs
// slice is empty — measured at all four hits. RecordType.Legs is layout metadata
// that the type's identity ignores, so a producer COULD populate it without
// moving any name, any label, or any type identity. That is the shape of the
// unblocking change, and this test states the precondition it would remove.
//
// This test asserts the EQUALS half. The identity-hash half is
// TestLegColumnOwner_TheLegTableReachesNoMemoIdentity, and the split is
// deliberate: this comment used to say "Equals and Hash ignore it" while only
// Equals was checked, and RecordType has no Hash method at all — the sentence
// named a mechanism that does not exist, so a reader chasing it would have found
// nothing and concluded the claim was covered.
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
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("O"), "O", 0, 1),
		},
	}
	if !seedLeg.Equals(withLegs) {
		t.Fatal("adding a leg table changed the RecordType's identity. RecordType.Equals is " +
			"documented to ignore Legs (layout metadata only); if that is no longer true, " +
			"populating the seed leg's table is a plan-visible change and the label-stability " +
			"argument for doing it has to be re-made.")
	}
}

// TestLegColumnOwner_TheLegTableReachesNoMemoIdentity is the identity-hash twin
// of the test above, and it exists because the claim it checks was stated
// against a mechanism that does not exist.
//
// RecordType.Legs' own doc says "Equals/Hash ignore it — layout metadata only",
// and the sibling test's comment repeated it. There is NO Hash method on
// RecordType, on Type, or anywhere in values/type.go. So the second half of the
// claim named nothing, and a reader verifying it would have found no such method
// and moved on — the most durable way for an unchecked claim to read as checked.
//
// WHAT THE MEMO ACTUALLY KEYS ON, read rather than assumed. Two channels, and a
// *RecordType can ride into both:
//
//   - values.SemanticHashCode (semantic_hash.go), the memo's structural hash.
//     Its QuantifiedObjectValue arm folds the bare tag "qov" with the alias AND
//     the type excluded; its FieldValue arm folds ordinals only; its
//     RecordConstructorValue arm folds field NAMES only.
//   - values.EqualsWithoutChildren (map_field_values.go:320), the memo's
//     structural equality, reached through SemanticEqualsUnderAliasMap and
//     expressions.memoEqual. Its QOV arm compares Correlation only; its
//     FieldValue arm compares the resolved ordinal path (or, lazy, the name);
//     its RC arm compares field names. The only arms that look at a type at all
//     are Cast/Promote, through typesEqual — which compares Code() and
//     IsNullable() and never reaches a record's fields, let alone its legs.
//
// THE HAZARD THIS GUARDS, stated so a future reader does not have to reconstruct
// it: RFC-200 gives RecordTypeLeg an explicit Kind and lets a producer populate
// a leg table where none was populated before. If any identity channel started
// reading Legs, two structurally identical rows carrying different leg tables
// would stop interning as one memo member — silently, as a duplicate group and
// a lost dedup rather than as an error — and every "populating the leg table is
// label-safe" argument in this file and in RFC-200 §Decision would be void.
//
// It is asserted over every carrier a leg-table-bearing record type actually
// travels on, not just one, because the arms above are per-carrier: a hash arm
// that started folding a type would move exactly one of these.
func TestLegColumnOwner_TheLegTableReachesNoMemoIdentity(t *testing.T) {
	t.Parallel()

	fields := []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "QTY", FieldType: values.NotNullLong, Ordinal: 1},
	}
	bare := &values.RecordType{Fields: fields}
	withLegs := &values.RecordType{
		Fields: fields,
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("O"), "O", 0, 1),
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("I"), "I", 1, 1),
		},
	}
	corr := values.NamedCorrelationIdentifier("Q")

	fieldValueOver := func(rt *values.RecordType) values.Value {
		return mustExecutorConstruct(values.ResolveOrdinalSeedField(mustTestQOV(t, corr, rt), 1))
	}
	rcOver := func(rt *values.RecordType) values.Value {
		return values.NewRawRecordConstructorValue(
			values.RecordConstructorField{
				Name:  values.OrdinalFieldName(0),
				Value: mustTestQOV(t, corr, rt),
			})
	}

	for _, tc := range []struct {
		carrier string
		a, b    values.Value
	}{
		{
			carrier: "a typed QuantifiedObjectValue — how a leg's row reaches a seed",
			a:       mustTestQOV(t, corr, bare),
			b:       mustTestQOV(t, corr, withLegs),
		},
		{
			carrier: "a baked FieldValue over that QOV — how a seed slot addresses it",
			a:       fieldValueOver(bare),
			b:       fieldValueOver(withLegs),
		},
		{
			carrier: "a RecordConstructorValue holding it — the positional merge's own shape",
			a:       rcOver(bare),
			b:       rcOver(withLegs),
		},
	} {
		if ha, hb := values.SemanticHashCode(tc.a), values.SemanticHashCode(tc.b); ha != hb {
			t.Errorf("%s: SemanticHashCode moved when a leg table was added (%d vs %d).\n"+
				"  RecordType.Legs is documented as layout metadata carrying NO identity "+
				"semantics, and the memo's hash is the channel that claim has to hold in. "+
				"With it reading Legs, two structurally identical rows carrying different "+
				"leg tables stop interning as one member — silently, as a duplicate group "+
				"and a lost dedup, never as an error. Every 'populating the leg table is "+
				"label-safe' argument in this file and in RFC-200 is void until this is "+
				"fixed at the hash, not here.", tc.carrier, ha, hb)
		}
		if !values.SemanticEqualsUnderAliasMap(tc.a, tc.b, values.EmptyAliasMap()) {
			t.Errorf("%s: SemanticEqualsUnderAliasMap says a leg table changes the value's "+
				"identity.\n"+
				"  This is the equality half of the same claim, and it is the half that "+
				"decides membership after the hash decides the bucket. Equal-implies-"+
				"same-hash still holds if BOTH move, so both are asserted separately — a "+
				"test that only checked the hash would pass while interning was broken.",
				tc.carrier)
		}
		// EqualsWithoutChildren is asserted SEPARATELY and not as a formality.
		// SemanticEqualsUnderAliasMap INTERCEPTS correlation-bearing leaves before
		// the structural path (semantic_equals.go:25-56), so a QuantifiedObjectValue
		// never reaches EqualsWithoutChildren's own QOV arm through it — the two are
		// independent copies of the "a QOV is its correlation" rule, and a mutation
		// to one is invisible through the other. Measured: teaching
		// EqualsWithoutChildren's QOV arm to read Legs left the
		// SemanticEqualsUnderAliasMap assertion above entirely GREEN.
		//
		// The arm is live for the relational layer's own EqualsWithoutChildren
		// (RFC-040 040.2), so it is a second real channel, not dead code.
		if !values.EqualsWithoutChildren(tc.a, tc.b) {
			t.Errorf("%s: EqualsWithoutChildren says a leg table changes the node's "+
				"identity.\n"+
				"  This is a SECOND identity rule for the same carrier, reached by a "+
				"different path than the one above, and the two can drift apart because "+
				"neither consults the other.", tc.carrier)
		}
	}

	// And the RENDERING channel, which is not identity at all. A leg table
	// appearing here would move any diagnostic or golden that prints a record
	// type, and the movement would be indistinguishable from a real change.
	// Whether any golden currently prints one is deliberately NOT claimed.
	if bare.String() != withLegs.String() {
		t.Errorf("RecordType.String() renders the leg table:\n  %s\n  %s\n"+
			"  This is the RENDERING channel, not an identity one, and the claim is "+
			"correspondingly narrow: RecordType.String() is what type-printing "+
			"diagnostics and any golden that prints a record type would show. Which "+
			"goldens actually print one is NOT asserted here — that would be a claim "+
			"about the golden set, which this test cannot see. What it does hold is "+
			"that populating a leg table changes no rendering at all, so the question "+
			"never arises.", bare.String(), withLegs.String())
	}
}
