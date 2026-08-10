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

// leakElementSlots is the fixture half that makes the declines below MEAN
// something, and its absence is why this test passed for a whole release while
// the property it names was false.
//
// A decline is `(0, false)`. The fall-through it must prevent is `(9, true)` —
// the ELEMENT's slot, answering for a reference that names leg A. With an EMPTY
// element map (what this test used to pass) those two outcomes COLLAPSE: the
// element lookup misses, the bare-leg scan misses `K` on a non-flat leg, and the
// function returns `(0, false)` for want of anything to say. Every assertion
// passed, and the resolver was reading the wrong column in production the whole
// time. The map must be populated, and populated at the COLLIDING name, or the
// test is a tautology about an empty map.
//
// Slot 9 is deliberately far from every leg slot (0..2), so a leaked answer is
// unmistakable in a failure message rather than off by one.
func leakElementSlots() map[string]int { return map[string]int{"K": 9, "AONLY": 9} }

// dupNamedFlatWindows: leg D is FLAT and addressable, and declares `K` TWICE.
// FieldIndexUnique refuses a duplicate, so the qualified lookup MISSES on a
// window it can otherwise address — a third way to fall out of the qualified arm,
// and one that only became reachable when the first-match FieldIndex was deleted
// (a leg window's Typ is a leg-concat for a clustered box run and may legitimately
// repeat a leaf name).
func dupNamedFlatWindows() map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow {
	legD := values.NamedCorrelationIdentifier("D")
	return map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legD: {
			Kind: values.LegKindFlatRun, Offset: 0, Alias: legD,
			Typ: &values.RecordType{Fields: []values.Field{
				{Name: "K", FieldType: values.UnknownType, Ordinal: 0},
				{Name: "K", FieldType: values.UnknownType, Ordinal: 1},
			}},
		},
	}
}

// slotInGatheredSeed's QUALIFIED arm returns a FLAT SLOT INDEX. A reference that
// STATES a qualifier is answered by that qualifier's leg or not at all — every
// way of failing the leg lookup declines, and none of them may fall through to
// the element-first bare namespace, where the element answers whenever it shares
// a leaf name with the column actually named.
//
// Java gives the same answer structurally rather than by rule: each quantifier is
// bound under its OWN alias (RecordQueryFlatMapPlan.java:135,140) and the unnest
// element lives on the Explode's own quantifier (LogicalOperator.java:318-329),
// so no qualified read has a shared namespace to fall into.
func TestLegKind_GatheredGroupSlotDeclinesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	legA := values.NamedCorrelationIdentifier("A")
	// A correlation NO window is filed under. Nothing about it is malformed; it is
	// simply not a leg of this seed — an existential inner's quantifier, say.
	legAbsent := values.NamedCorrelationIdentifier("ZZ")

	for _, tc := range []struct {
		name     string
		windows  map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow
		corr     values.CorrelationIdentifier
		wantSlot int
		wantOK   bool
		why      string
	}{
		{
			name: "flatRun — the control", windows: kindWindows(values.LegKindFlatRun), corr: legA,
			wantSlot: 0, wantOK: true,
			why: "leg A's K is the seed's slot 0. Without this arm answering, every " +
				"decline below is a decline by a dead reader — and note it must be 0 and " +
				"not 9: the qualifier outranks a same-named element",
		},
		{
			name: "UNSET — the invalid zero", windows: kindWindows(values.LegKindUnset), corr: legA,
			wantOK: false,
			why: "a window reached a slot resolver with no stated kind. The group-by key " +
				"it would have resolved is better unresolved than resolved to a guess",
		},
		{
			name: "nested", windows: kindWindows(values.LegKindNested), corr: legA,
			wantOK: false,
			why: "Offset names ONE slot holding the leg's whole row, so there is no flat " +
				"slot for K at all — w.Offset+idx would name a neighbouring leg's slot",
		},
		{
			name: "correlation absent from the window map", windows: kindWindows(values.LegKindFlatRun), corr: legAbsent,
			wantOK: false,
			why: "the reference names a source this seed does not carry. It resolves to " +
				"NOTHING here; the element is a different column that merely shares a name",
		},
		{
			name: "flat leg declaring the column TWICE", windows: dupNamedFlatWindows(),
			corr: values.NamedCorrelationIdentifier("D"), wantOK: false,
			why: "FieldIndexUnique refuses a duplicate, so the qualified lookup misses on " +
				"an addressable window. Newly reachable since first-match FieldIndex was deleted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slot, ok := slotInGatheredSeed(tc.windows, leakElementSlots(), tc.corr, "K", true)
			if ok != tc.wantOK || (tc.wantOK && slot != tc.wantSlot) {
				t.Fatalf("slotInGatheredSeed(qualified <%s>.K) = (%d, %t), want (%d, %t) — %s\n"+
					"  The element map carries K at slot %d. A qualified read that resolves to %d "+
					"has read the ELEMENT for a reference naming a leg: a silent wrong-column read, "+
					"not a missing one.",
					tc.corr.Name(), slot, ok, tc.wantSlot, tc.wantOK, tc.why,
					leakElementSlots()["K"], leakElementSlots()["K"])
			}
		})
	}
}

// The control that stops the declines above from passing for the wrong reason.
//
// A gate on `qualified` is only correct if it is a gate on QUALIFIED-ness and not
// a blanket shutdown of the bare namespace. Turning the element arm off entirely
// would satisfy every decline above while breaking the resolver's actual job, so
// both bare arms are exercised here on the SAME layouts the declines use.
func TestLegKind_GatheredGroupSlotStillServesTheBareNamespace(t *testing.T) {
	t.Parallel()
	zero := values.CorrelationIdentifier{}

	// The ELEMENT arm: bare `K` is element-first, on every leg kind — this is the
	// answer the qualified declines above must NOT be reachable by.
	for _, kind := range []values.LegKind{values.LegKindFlatRun, values.LegKindUnset, values.LegKindNested} {
		slot, ok := slotInGatheredSeed(kindWindows(kind), leakElementSlots(), zero, "K", false)
		if !ok || slot != 9 {
			t.Fatalf("bare K over a kind=%v leg = (%d, %t), want (9, true). The qualified "+
				"decline must fire on the QUALIFIER; if it has switched off element-first "+
				"resolution as well, the declines it produces are vacuous", kind, slot, ok)
		}
	}

	// The bare-LEG scan: it lost its own `!qualified` guard to the single gate, so
	// it must still answer a bare column no element declares.
	slot, ok := slotInGatheredSeed(kindWindows(values.LegKindFlatRun), leakElementSlots(), zero, "BONLY", false)
	if !ok || slot != 2 {
		t.Fatalf("bare BONLY = (%d, %t), want (2, true) — the bare-leg scan stopped "+
			"answering when its own guard was folded into the qualified gate", slot, ok)
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

// rebaseLegRefsToBox — keyed reader #2 — bakes `w.Offset + FieldIndexUnique(col)` over
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
