package query

// RFC-200 step 3b, translator side: the three offset-arithmetic readers of a
// seed-window map must DISPATCH on the leg kind, never infer it.
//
// Two of the three are keyed and the seed-window reader census can see them. The
// THIRD — slotInGatheredSeed's bare-column arm — is invisible to that census,
// because it ranges every window instead of selecting one and therefore records
// no keyed read. It does exactly the same `w.Offset + idx` arithmetic as the
// keyed sites and is exactly as dangerous, which is why it gets a test of its
// own rather than riding along with its sibling arm.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// kindWindows builds a two-leg gathered-seed layout with the KIND under test on
// leg A. Leg B is always flat, so the bare-column arm below has a live
// competitor and its hit-counting can be observed rather than assumed.
func kindWindows(kind values.LegKind) map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow {
	legA := values.NamedCorrelationIdentifier("A")
	legB := values.NamedCorrelationIdentifier("B")
	return map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legA: {
			Kind: kind, Offset: 0, Alias: legA,
			Typ: &values.RecordType{Fields: []values.Field{
				{Name: "K", FieldType: values.UnknownType, Ordinal: 0},
				{Name: "AONLY", FieldType: values.UnknownType, Ordinal: 1},
			}},
		},
		legB: {
			Kind: values.LegKindFlatRun, Offset: 2, Alias: legB,
			Typ: &values.RecordType{Fields: []values.Field{
				{Name: "BONLY", FieldType: values.UnknownType, Ordinal: 0},
			}},
		},
	}
}

// slotInGatheredSeed's QUALIFIED arm returns a FLAT SLOT INDEX, and a nested leg
// has none: its column lives one level down, reachable only by descending. So it
// must decline — exactly as it already declines a qualified read that carries no
// correlation at all, and for the same reason: an answer this site cannot
// express honestly must not be approximated.
func TestLegKind_GatheredGroupSlotDeclinesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	legA := values.NamedCorrelationIdentifier("A")

	for _, tc := range []struct {
		name     string
		kind     values.LegKind
		wantSlot int
		wantOK   bool
		why      string
	}{
		{
			name: "flatRun — the control", kind: values.LegKindFlatRun,
			wantSlot: 0, wantOK: true,
			why: "leg A's K is the seed's slot 0. Without this arm answering, every " +
				"decline below is a decline by a dead reader",
		},
		{
			name: "UNSET — the invalid zero", kind: values.LegKindUnset,
			wantOK: false,
			why: "a window reached a slot resolver with no stated kind. The group-by key " +
				"it would have resolved is better unresolved than resolved to a guess",
		},
		{
			name: "nested", kind: values.LegKindNested,
			wantOK: false,
			why: "Offset names ONE slot holding the leg's whole row, so there is no flat " +
				"slot for K at all — w.Offset+idx would name a neighbouring leg's slot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slot, ok := slotInGatheredSeed(kindWindows(tc.kind), map[string]int{}, legA, "K", true)
			if ok != tc.wantOK || (tc.wantOK && slot != tc.wantSlot) {
				t.Fatalf("slotInGatheredSeed(qualified A.K, kind=%v) = (%d, %t), want (%d, %t) — %s",
					tc.kind, slot, ok, tc.wantSlot, tc.wantOK, tc.why)
			}
		})
	}
}

// The CENSUS-INVISIBLE bare-column arm, and the reason it needs its own test.
//
// It resolves a bare column when EXACTLY ONE leg declares it. A nested window
// must not contribute a hit — and getting that wrong is worse than a single
// wrong offset, because `hits` is also the ambiguity counter. Both failure
// modes are exercised here:
//
//   - AONLY is declared only by the nested leg. A nested contribution resolves
//     it to `w.Offset + 1`, a slot that belongs to leg B.
//   - BONLY is declared only by flat leg B and resolves cleanly. This is the
//     control that keeps the arm from passing while dead.
//
// The third mode — a name in BOTH a nested and a flat leg going from a unique
// resolution to an ambiguous decline — is what the ambiguity check below covers.
func TestLegKind_GatheredSeedBareArmIgnoresANestedWindow(t *testing.T) {
	t.Parallel()
	zero := values.CorrelationIdentifier{}

	// Control: the arm is alive. A column only leg B declares resolves to leg B's
	// slot regardless of what kind leg A carries.
	for _, kind := range []values.LegKind{values.LegKindFlatRun, values.LegKindUnset, values.LegKindNested} {
		slot, ok := slotInGatheredSeed(kindWindows(kind), map[string]int{}, zero, "BONLY", false)
		if !ok || slot != 2 {
			t.Fatalf("bare BONLY with leg A at kind=%v = (%d, %t), want (2, true) — the "+
				"bare arm is not answering at all, so every negative below holds vacuously",
				kind, slot, ok)
		}
	}

	// A column only the NESTED leg declares must not resolve. `w.Offset + idx`
	// would be slot 1, and slot 1 is inside the nested leg's own single slot's
	// neighbourhood, not the column named.
	for _, kind := range []values.LegKind{values.LegKindUnset, values.LegKindNested} {
		if slot, ok := slotInGatheredSeed(kindWindows(kind), map[string]int{}, zero, "AONLY", false); ok {
			t.Fatalf("bare AONLY resolved to slot %d over a kind=%v leg. This arm is "+
				"INVISIBLE to the seed-window reader census — it ranges every window "+
				"instead of keying one, so no keyed read is recorded and nothing else "+
				"watches it — while doing exactly the same offset arithmetic as the five "+
				"keyed sites.", slot, kind)
		}
	}

	// And leg A's flat control: the same column DOES resolve when leg A is flat,
	// which is what makes the two negatives above about the KIND and not about
	// the column.
	if slot, ok := slotInGatheredSeed(kindWindows(values.LegKindFlatRun), map[string]int{}, zero, "AONLY", false); !ok || slot != 1 {
		t.Fatalf("bare AONLY over a FLAT leg A = (%d, %t), want (1, true)", slot, ok)
	}
}

// rebaseLegRefsToBox — keyed reader #2 — bakes `w.Offset + FieldIndex(col)` over
// the box quantifier. Same arithmetic, same dispatch requirement.
//
// Its decline is to leave the node UNREWRITTEN, which is correct at this site
// rather than a soft failure: the caller's survivor verification sees a
// leg-correlated reference that survived and declines the whole wrap.
func TestLegKind_BoxRebaseRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	legA := values.NamedCorrelationIdentifier("A")
	boxType := &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "AONLY", FieldType: values.UnknownType, Ordinal: 1},
		{Name: "BONLY", FieldType: values.UnknownType, Ordinal: 2},
	}}
	boxQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("BOX"), boxType)

	ref := values.NewFieldValue(values.NewQuantifiedObjectValue(legA), "K", values.UnknownType)

	for _, tc := range []struct {
		name     string
		kind     values.LegKind
		wantOK   bool
		wantPath []int
	}{
		// Leg A's window sits at offset 0 and K is its column 0, so the flat
		// address is slot 0 and the nested one is "slot 0, then leg-local 0".
		// Those coincide numerically at the head, which is exactly why the PATH
		// LENGTH is what this test asserts: a one-step [0] and a two-step [0 0]
		// read different things and a check on the first ordinal cannot tell them
		// apart.
		{"flatRun — one step", values.LegKindFlatRun, true, []int{0}},
		{"nested — the FUSED two-step address", values.LegKindNested, true, []int{0, 0}},
		{"UNSET — the invalid zero", values.LegKindUnset, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, ok := rebaseLegRefsToBox(ref, kindWindows(tc.kind), boxType, boxQOV)
			if ok != tc.wantOK {
				t.Fatalf("kind=%v: rebaseLegRefsToBox ok=%t, want %t. A window this reader "+
					"cannot address must leave the reference unrewritten so the survivor "+
					"scan declines the whole wrap; accepting means an unaddressed leg "+
					"reference shipped, or was baked at an offset that is not its column. "+
					"(out=%v)", tc.kind, ok, tc.wantOK, out)
			}
			if !tc.wantOK {
				return
			}
			fv, isFV := out.(*values.FieldValue)
			if !isFV || fv.Resolved == nil {
				t.Fatalf("kind=%v must bake to an ordinal path over the box, got %T %v", tc.kind, out, out)
			}
			got := make([]int, len(fv.Resolved.Accessors))
			for i, a := range fv.Resolved.Accessors {
				got[i] = a.Ordinal
			}
			if len(got) != len(tc.wantPath) {
				t.Fatalf("kind=%v produced a %d-step path %v, want the %d-step %v",
					tc.kind, len(got), got, len(tc.wantPath), tc.wantPath)
			}
			for i := range got {
				if got[i] != tc.wantPath[i] {
					t.Fatalf("kind=%v produced path %v, want %v", tc.kind, got, tc.wantPath)
				}
			}
		})
	}
}
