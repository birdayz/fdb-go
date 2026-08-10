package executor

// rowSlotForLegColumn's DOTTED arm recovers a qualifier by slicing a leg-type
// column name at its first dot. Its input is text — `adaptLegPositional` passes
// `legType.Fields[i].Name` — so the reader cannot tell a qualified `LEG.COL`
// from a name whose dot means something else.
//
// Something else DOES reach it. Measured over the real-FDB sqldriver corpus,
// eight dotted leg-type field names arrive here and only four are references:
// the other four are `SUM(GA.V)`, a rendered AGGREGATE OUTPUT LABEL (a public
// column name — TestFDB_UnionQualifiedAggregate) whose dot sits inside the
// function argument. Sliced, it manufactures the leg alias `SUM(GA`.
//
// It has never misresolved, and these tests pin the two facts that are WHY —
// neither of which was pinned before, so both were properties nobody could see
// change:
//
//  1. the flat exact match is consulted FIRST, and wins even where the dotted
//     slice would also answer with a DIFFERENT slot;
//  2. a manufactured qualifier that names no leg DECLINES — the arm does not
//     fall back to matching the leaf alone, so the containment does not depend
//     on the leaf being absent.
//
// Both are ORDERING and REACH properties of a reader with no type information,
// which is exactly the kind that reads as obviously-fine right up until a
// layout moves underneath it. The producer conversion is the real fix; these
// pin the containment that has to hold until then, and afterwards.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRowSlotForLegColumn_FlatExactMatchWinsOverTheLegWindow pins the ordering.
//
// The row carries BOTH a flat column literally named `C.CV` and a leg `C` whose
// window declares `CV`. The two are different columns at different slots, and
// the only thing choosing between them is which lookup runs first. A merged
// concat row can carry both — the flat dotted spelling is how the seed names a
// leg column, and the leg boundaries are on the same type.
func TestRowSlotForLegColumn_FlatExactMatchWinsOverTheLegWindow(t *testing.T) {
	t.Parallel()
	rt := &values.RecordType{
		Fields: []values.Field{{Name: "CV", Ordinal: 0}, {Name: "C.CV", Ordinal: 1}},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("C"), "C", 0, 1),
		},
	}

	// Guard the fixture: the dotted arm really would answer, and with the OTHER
	// slot — otherwise the ordering decides nothing and this passes vacuously.
	if _, found := rt.FieldIndexUnique("CV"); !found {
		t.Fatal("fixture: leg C's window must declare CV for the dotted arm to answer")
	}

	got, bind := rowSlotForLegColumn(rt, "C.CV", values.CorrelationIdentifier{})
	if bind != legColumnBound {
		t.Fatal("C.CV did not resolve at all")
	}
	if got != 1 {
		t.Fatalf("C.CV resolved to slot %d, want 1 — the row DECLARES a column named "+
			"\"C.CV\" and the dotted arm was consulted before it, so a column the type "+
			"names outright lost to a qualifier sliced out of that same name", got)
	}
}

// TestRowSlotForLegColumn_AManufacturedQualifierDeclines pins the reach.
//
// `SUM(GA.V)` slices into qualifier `SUM(GA` and leaf `V)`. The leaf is
// deliberately PRESENT in leg GA's window, so the decline can only come from the
// qualifier failing to name a leg — if the arm ever matched the leaf alone, or
// scanned windows without checking the qualifier, this resolves and a rendered
// aggregate label starts reading a neighbouring column.
func TestRowSlotForLegColumn_AManufacturedQualifierDeclines(t *testing.T) {
	t.Parallel()
	rt := &values.RecordType{
		Fields: []values.Field{{Name: "V", Ordinal: 0}, {Name: "V)", Ordinal: 1}},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("GA"), "GA", 0, 2),
		},
	}

	// Guard the fixture, both halves: the LEAF the slice produces is present in
	// the window, and the genuine qualified twin resolves. Without these the
	// decline below could be "nothing here resolves".
	if _, found := rt.FieldIndexUnique("V)"); !found {
		t.Fatal("fixture: the manufactured leaf \"V)\" must exist, or the qualifier is not what declines")
	}
	if slot, bind := rowSlotForLegColumn(rt, "GA.V", values.CorrelationIdentifier{}); bind != legColumnBound || slot != 0 {
		t.Fatalf("fixture: the genuine qualified read GA.V = (%d,%v), want (0,true) — "+
			"the dotted arm must be live for its refusal to mean anything", slot, bind)
	}

	if slot, bind := rowSlotForLegColumn(rt, "SUM(GA.V)", values.CorrelationIdentifier{}); bind == legColumnBound {
		t.Fatalf("the rendered aggregate label %q resolved to slot %d — its dot is inside "+
			"the function argument, so the slice manufactured the leg alias %q and the "+
			"reader bound a public output column to a leg window",
			"SUM(GA.V)", slot, "SUM(GA")
	}
}

// TestRowSlotForLegColumn_ARenderedLabelPresentInTheRowStillWins is the pair of
// the two above, in the arrangement the corpus actually produces: `SUM(GA.V)` is
// a real column of the row, and it must resolve to ITSELF.
//
// This is the disposition callers expect and the one the measurement found — the
// label reaches the reader four times over the corpus and takes the flat arm
// every time. Pinning it says what "the arm cannot fire for this shape" rests
// on: not that the dotted arm is unreachable, but that the flat name is there.
func TestRowSlotForLegColumn_ARenderedLabelPresentInTheRowStillWins(t *testing.T) {
	t.Parallel()
	rt := &values.RecordType{
		Fields: []values.Field{
			{Name: "V", Ordinal: 0},
			{Name: "SUM(GA.V)", Ordinal: 1},
		},
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(values.LegKindFlatRun, values.NamedCorrelationIdentifier("GA"), "GA", 0, 1),
		},
	}
	slot, bind := rowSlotForLegColumn(rt, "SUM(GA.V)", values.CorrelationIdentifier{})
	if bind != legColumnBound || slot != 1 {
		t.Fatalf("SUM(GA.V) = (%d,%v), want (1,true) — a public aggregate label the row "+
			"declares must resolve to its own column", slot, bind)
	}
}

// TestRowSlotForLegColumn_DuplicateBareNamesAcrossLegs is the hazard the dotted
// arm's RETIREMENT has to clear, pinned before the retirement rather than after.
//
// The obvious way to delete this arm is to make the seed emit BARE column names
// and let the flat lookup find them. That does not work, and the reason is a
// property of the merged row rather than of any producer: legs contribute their
// columns to one flat namespace, so two legs that both declare `ID` put `ID` at
// two slots. The retired first-match lookup answered the FIRST for both — a
// wrong-leg read returning a plausible value that nothing detects. The flat
// lookup now DECLINES instead, and the decline travels to the caller as
// legColumnAmbiguous so the gather refuses rather than binding a nil, which
// would be the same wrong answer with a different value in it. Both halves are
// asserted at the foot of this test.
//
// The witness is the real corpus shape (TestFDB_CorrelatedScalarJoinInner):
// rtFields [ID CUSTOMER_ID ID ORDER_ID QTY], leg O = [0,2), leg I = [2,5). `ID`
// is at slot 0 in O and slot 2 in I.
//
// So the retirement needs the OTHER half — resolution by leg IDENTITY plus a
// leg-local ordinal — and this test states the arithmetic that half must
// reproduce on all three corpus witnesses. It is the acceptance harness for that
// conversion: it passes today on the dotted arm, and it must still pass when the
// arm is gone and the identity path answers instead.
func TestRowSlotForLegColumn_DuplicateBareNamesAcrossLegs(t *testing.T) {
	t.Parallel()
	// The merged row of the corpus witness, with its leg windows.
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

	// THE HAZARD, now refused rather than answered. A bare lookup cannot tell the
	// two legs' ID apart, so it declines instead of handing back leg O's slot for
	// a reference that may mean leg I's.
	if _, found := merged.FieldIndexUnique("ID"); found {
		t.Fatal("a bare lookup resolved a name both legs declare — it must decline; " +
			"if this fixture stopped having a duplicate bare name across legs it has " +
			"stopped describing the hazard it exists for")
	}
	// Both legs really do declare it — that is what makes slot 0 a WRONG answer
	// for one of them rather than merely a first-match among equals.
	if merged.Fields[2].Name != "ID" {
		t.Fatalf("fixture: leg I must also declare ID (slot 2 is %q)", merged.Fields[2].Name)
	}

	// THE ARITHMETIC the identity path must reproduce: leg start + leg-local
	// ordinal, for each corpus witness. Stated as data so the conversion has a
	// target, not a description.
	for _, w := range []struct {
		name       string // the qualified leg-type column name, as it arrives today
		leg        string
		start      int
		legOrdinal int
		want       int
	}{
		{"O.ID", "O", 0, 0, 0},
		{"I.QTY", "I", 2, 2, 4},
	} {
		if got := w.start + w.legOrdinal; got != w.want {
			t.Fatalf("%s: leg start %d + leg-local ordinal %d = %d, want %d — the "+
				"witness arithmetic is stated wrong and the conversion would aim at it",
				w.name, w.start, w.legOrdinal, got, w.want)
		}
		got, bind := rowSlotForLegColumn(merged, w.name, values.CorrelationIdentifier{})
		if bind != legColumnBound || got != w.want {
			t.Fatalf("%s resolved to (%d,%v), want (%d,legColumnBound)", w.name, got, bind, w.want)
		}
	}

	// The pair that makes the hazard concrete: O.ID and I.ID are DIFFERENT slots,
	// and a bare `ID` collapses them onto the first. Any retirement that answers
	// both with 0 has reintroduced exactly this.
	oid, _ := rowSlotForLegColumn(merged, "O.ID", values.CorrelationIdentifier{})
	iid, iidBind := rowSlotForLegColumn(merged, "I.ID", values.CorrelationIdentifier{})
	if iidBind != legColumnBound {
		t.Fatal("I.ID did not resolve — leg I declares ID at slot 2")
	}
	if oid == iid {
		t.Fatalf("O.ID and I.ID both resolved to slot %d — the two legs' ID columns are "+
			"distinct and a resolution that conflates them reads the wrong leg's row",
			oid)
	}
	if iid != 2 {
		t.Fatalf("I.ID = %d, want 2 (leg I start 2 + leg-local ordinal 0)", iid)
	}

	// WHAT THE DECLINE DOES DOWNSTREAM — the half this test used to leave open.
	//
	// Asserting only that the LOOKUP declines says nothing about the answer the
	// query gets, and the two are not the same claim. A decline that the gather
	// folds into "column not present" leaves the slot nil, which is a WRONG
	// value (the row carries the column, twice) dressed as a missing one — worse
	// than the arbitrary-but-real column the retired first-match returned,
	// because a NULL flows on and dedups and compares.
	//
	// So both ends are pinned: the reader reports the ambiguity as its own
	// outcome, and the gather REFUSES on it.
	if _, bind := rowSlotForLegColumn(merged, "ID", values.CorrelationIdentifier{}); bind != legColumnAmbiguous {
		t.Fatalf("bare ID bound as %v, want legColumnAmbiguous — folding a duplicated "+
			"name into the ordinary miss hands the gather a nil for a column the row "+
			"carries at two slots, and nothing downstream can tell that apart from a "+
			"column the row genuinely lacks", bind)
	}
	legType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0}}}
	pos := NewPositionalRow(merged)
	pos.Slots = []any{int64(1), int64(10), int64(2), int64(20), int64(3)}
	adapted, err := adaptLegPositional(QueryResult{Positional: pos}, legType, values.CorrelationIdentifier{})
	if err == nil {
		t.Fatalf("adaptLegPositional accepted a leg column the source row declares twice "+
			"and produced %v. Either slot is another leg's column and a nil is a NULL for "+
			"a column that has a value; both are silent wrong rows, so this must fail the "+
			"query rather than degrade", adapted)
	}
	if !strings.Contains(err.Error(), "declared 2 times") {
		t.Fatalf("adaptLegPositional failed, but not on the ambiguity — the message must "+
			"name the duplicated column so the PRODUCER can be found, since that is where "+
			"the fix goes: %v", err)
	}
}
