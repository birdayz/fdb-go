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
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("C"), "C", 0, 1),
		},
	}

	// Guard the fixture: the dotted arm really would answer, and with the OTHER
	// slot — otherwise the ordering decides nothing and this passes vacuously.
	if _, found := rt.FieldIndex("CV"); !found {
		t.Fatal("fixture: leg C's window must declare CV for the dotted arm to answer")
	}

	got, ok := rowSlotForLegColumn(rt, "C.CV")
	if !ok {
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
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("GA"), "GA", 0, 2),
		},
	}

	// Guard the fixture, both halves: the LEAF the slice produces is present in
	// the window, and the genuine qualified twin resolves. Without these the
	// decline below could be "nothing here resolves".
	if _, found := rt.FieldIndex("V)"); !found {
		t.Fatal("fixture: the manufactured leaf \"V)\" must exist, or the qualifier is not what declines")
	}
	if slot, ok := rowSlotForLegColumn(rt, "GA.V"); !ok || slot != 0 {
		t.Fatalf("fixture: the genuine qualified read GA.V = (%d,%v), want (0,true) — "+
			"the dotted arm must be live for its refusal to mean anything", slot, ok)
	}

	if slot, ok := rowSlotForLegColumn(rt, "SUM(GA.V)"); ok {
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
			values.NewRecordTypeLeg(values.NamedCorrelationIdentifier("GA"), "GA", 0, 1),
		},
	}
	slot, ok := rowSlotForLegColumn(rt, "SUM(GA.V)")
	if !ok || slot != 1 {
		t.Fatalf("SUM(GA.V) = (%d,%v), want (1,true) — a public aggregate label the row "+
			"declares must resolve to its own column", slot, ok)
	}
}
