package cascades

// The NAME arm of rebaseOuterLegValueOrdinal against a leg window that holds two
// columns of one name — the case its two SIBLING arms in the same switch both
// defend against and it does not.
//
// THIS FILE PINS A DEFECT, NOT A CONTRACT. Every assertion below states what the
// code does TODAY, and every failure message says which way to flip it. A green
// run here means the asymmetry is still present.
//
// The asymmetry, stated precisely. rebaseOuterLegValueOrdinal dispatches a leg
// reference to one of three arms, all resolving against the SAME window type
// `w.Typ`:
//
//	multi-accessor -> FieldPath.ReAnchorRootInto, which COUNTS matches and
//	                  declines on `dupes > 1` (values.go: "root column name is
//	                  ambiguous in the flowed layout")
//	FrontierPinned -> carries acc.Ordinal and performs no name lookup at all,
//	                  its comment naming this exact hazard: "an OPAQUE box leg
//	                  can expose DUPLICATE buried column names (`A.K` and `B.K`
//	                  merged into one leg), where FieldIndex("K") would remap the
//	                  already-baked ref to the FIRST match and silently probe the
//	                  WRONG column (wrong rows)"
//	name arm       -> w.Typ.FieldIndex(fv.Field), a FIRST-MATCH scan with no
//	                  duplicate detection (type.go RecordType.FieldIndex),
//	                  justified in place by "A single source leg has no duplicate
//	                  column names, so FieldIndex(Field) is the leg-local
//	                  ordinal."
//
// That justification is the claim under test, and its own siblings contradict
// it: the window is not always a single source leg. A CLUSTERED BOX run window
// concatenates every buried leaf's columns, and finalizeSeedWindows narrows only
// the RIGHTMOST leaf's entry (ordinal_seed_layout.go: "an alias-qualified read
// must window the leaf rather than FieldIndex across the whole concat"). A run
// window that was not narrowed therefore can hold `A.K` and `B.K` as two fields
// both named K.
//
// WHY THE CARRIED ORDINAL MAKES THIS SHARPER THAN "LAZY REFS ARE UNDEFENDED".
// The name arm is not reached only by lazy references. A SOURCE-RELATIVE baked
// reference — resolved, unpinned, single-accessor — reaches it too, and it
// ARRIVES CARRYING the correct leg-local ordinal. The arm discards that ordinal
// and re-derives one by first-match name. The multi-accessor arm, given the same
// disagreement, declines outright ("carried ordinal disagrees with the flowed
// layout"). So one arm treats the carried ordinal as authoritative enough to
// refuse on, and another silently overrules it.
//
// A duplicate-named RecordType is built as a RAW literal here because
// NewRecordType PANICS on duplicate field names. That is not a harness dodge —
// it is how such a window arises in production too, at ordinal_seed_layout.go,
// and it is why FieldIndex's own doc advertises working on "a raw RecordType
// that was built without NewRecordType's normalization".

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// dupNamedLegFixture builds the concatenated box-run shape: leg `L` = [K, K, Z]
// where the two K columns stand for two buried leaves' same-named columns, plus
// a merged row in which the leg's run starts at slot 10.
//
// Z exists so the leg is not degenerate — a two-column all-duplicate type would
// make "first match" and "only match" indistinguishable for any control.
func dupNamedLegFixture() (leg *values.RecordType, mergedQOV *values.QuantifiedObjectValue) {
	leg = &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "K", FieldType: values.NullableInt, Ordinal: 1},
		{Name: "Z", FieldType: values.NullableInt, Ordinal: 2},
	}}
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedQOV = values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("M"), values.NewRecordType("", false, mergedFields))
	return leg, mergedQOV
}

func dupNamedLegWindows(leg *values.RecordType) map[values.CorrelationIdentifier]ordinalLegWindow {
	return map[values.CorrelationIdentifier]ordinalLegWindow{
		values.NamedCorrelationIdentifier("L"): {
			Kind:   values.LegKindFlatRun,
			Offset: 10,
			Typ:    leg,
			Alias:  values.NamedCorrelationIdentifier("L"),
		},
	}
}

// rebasedOrdinals drives the production symbol and returns the merged path.
func rebasedOrdinals(t *testing.T, ref values.Value, leg *values.RecordType, merged *values.QuantifiedObjectValue) ([]int, bool) {
	t.Helper()
	out, ok := rebaseOuterLegValueOrdinal(ref, dupNamedLegWindows(leg), merged)
	if !ok {
		return nil, false
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved == nil {
		t.Fatalf("rebase produced %T (%v), want a baked *FieldValue — the fixture is "+
			"broken and every verdict in this file is void", out, out)
	}
	got := make([]int, len(fv.Resolved.Accessors))
	for i, a := range fv.Resolved.Accessors {
		got[i] = a.Ordinal
	}
	return got, true
}

// TestDupNamedLegWindow_NameArmDiscardsTheCarriedOrdinalAndFirstMatches is the
// defect itself, stated two-sidedly.
//
// The reference is `L.K` resolved to leg-local ordinal 1 — the SECOND K, which
// is the column the user asked for. Correct output is merged slot 11 (Offset 10
// + 1), or a DECLINE. What the code produces is slot 10.
func TestDupNamedLegWindow_NameArmDiscardsTheCarriedOrdinalAndFirstMatches(t *testing.T) {
	t.Parallel()

	leg, merged := dupNamedLegFixture()
	// Unpinned, single-accessor, resolved: the SOURCE-RELATIVE baked shape. It
	// carries ordinal 1 and is dispatched to the name arm, because arm 1 needs
	// arity > 1 and arm 2 needs the frontier pin.
	ref := &values.FieldValue{
		Field:    "K",
		Typ:      values.NullableInt,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		Resolved: values.NewFieldPathOfSingleInDomain("K", 1, false, values.OrdinalDomainOfType(leg)),
	}

	got, ok := rebasedOrdinals(t, ref, leg, merged)
	if !ok {
		t.Fatalf("rebaseOuterLegValueOrdinal DECLINED — and a decline is one of the two " +
			"CORRECT answers here.\n\nTHE DEFECT THIS FILE PINS IS FIXED. Retire this test " +
			"and the `dotted` debt entry for left_outer_existential.go # " +
			"rebaseOuterLegValueOrdinal is unaffected (its '.' probe is a separate " +
			"concern), but the in-place comment 'A single source leg has no duplicate " +
			"column names' must go with the fix.")
	}
	if fmt.Sprint(got) == fmt.Sprint([]int{11}) {
		t.Fatalf("the name arm produced merged slot 11 — the column the reference's own " +
			"carried ordinal names.\n\nTHE DEFECT THIS FILE PINS IS FIXED: the arm now " +
			"honours the carried leg-local ordinal instead of re-deriving one by " +
			"first-match name. FLIP THIS TEST to assert [11] as the contract, drop the " +
			"'first match' language, and correct the in-place comment 'A single source " +
			"leg has no duplicate column names, so FieldIndex(Field) is the leg-local " +
			"ordinal.'")
	}
	if fmt.Sprint(got) != fmt.Sprint([]int{10}) {
		t.Fatalf("the name arm produced %v; this file was written when it produced [10] "+
			"(first match) and [11] is the correct answer. An unrecognised third answer "+
			"means the dispatch changed shape — re-read the switch before trusting any "+
			"verdict here.", got)
	}

	t.Logf("PINNED DEFECT: `L.K` carrying leg-local ordinal 1 rebased to merged slot %v, "+
		"not [11]. The carried ordinal was discarded and FieldIndex(\"K\") first-matched "+
		"the leg's slot 0. This is a WRONG-COLUMN read, and it is silent: slot 10 is a "+
		"real merged column of the same type, so nothing downstream rejects it.", got)
}

// TestDupNamedLegWindow_SiblingArmsDefendAgainstTheSameWindow is the other side.
//
// Without it the test above is a bug report; with it, it is an ASYMMETRY — the
// same window, the same duplicate name, defended twice and undefended once. If
// either sibling ever stops defending, that is a REGRESSION in the opposite
// direction and this test is what reports it.
func TestDupNamedLegWindow_SiblingArmsDefendAgainstTheSameWindow(t *testing.T) {
	t.Parallel()

	t.Run("multi-accessor arm DECLINES on the ambiguous root", func(t *testing.T) {
		t.Parallel()
		leg, merged := dupNamedLegFixture()
		// `L.K.<member>` — arity 2 sends it to ReAnchorRootInto, which counts
		// matches for root name K, finds 2, and declines.
		ref := &values.FieldValue{
			Field: "K",
			Typ:   values.NullableInt,
			Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
			Resolved: &values.FieldPath{
				Accessors: []values.ResolvedAccessor{
					{Field: "K", Ordinal: 1},
					{Field: "SUB", Ordinal: 0},
				},
				Domain: values.OrdinalDomainOfType(leg),
			},
		}
		if got, ok := rebasedOrdinals(t, ref, leg, merged); ok {
			t.Fatalf("the multi-accessor arm ACCEPTED an ambiguous root and produced %v.\n\n"+
				"REGRESSION, opposite direction: ReAnchorRootInto's `dupes > 1` decline is "+
				"what makes the name arm's silence an asymmetry rather than a house style. "+
				"If this arm stopped counting duplicates, the wrong-column read now confined "+
				"to the name arm reaches nested references too.", got)
		}
	})

	t.Run("FrontierPinned arm honours the carried ordinal", func(t *testing.T) {
		t.Parallel()
		leg, merged := dupNamedLegFixture()
		// The SAME reference as the defect test, differing ONLY in the pin. That
		// one bit is the entire difference between a correct answer and a wrong
		// one, which is the sharpest statement of the asymmetry available.
		ref := &values.FieldValue{
			Field:    "K",
			Typ:      values.NullableInt,
			Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
			Resolved: values.NewFieldPathOfSingleInDomain("K", 1, true, values.OrdinalDomainOfType(leg)),
		}
		got, ok := rebasedOrdinals(t, ref, leg, merged)
		if !ok {
			t.Fatal("the FrontierPinned arm DECLINED a single-accessor pinned reference. " +
				"Every verdict in this file rests on the two siblings behaving differently " +
				"from the name arm; a decline here means the fixture no longer reaches the " +
				"arm it names.")
		}
		if fmt.Sprint(got) != fmt.Sprint([]int{11}) {
			t.Fatalf("the FrontierPinned arm produced %v, want [11] (Offset 10 + the carried "+
				"leg-local ordinal 1).\n\nREGRESSION, opposite direction: this arm exists to "+
				"carry the ordinal precisely so an opaque box leg's duplicate buried names "+
				"cannot remap it. If it produced [10] it has started first-matching by name "+
				"like its sibling, and the defect has SPREAD rather than been fixed.", got)
		}
	})
}

// TestDupNamedLegWindow_ControlsDistinguishFixtureFromFinding keeps the two
// tests above interpretable.
//
// A first-match answer is only evidence of first-matching if a NON-duplicate
// lookup in the same fixture lands where it should, and if the duplicate lookup
// asking for slot 0 also lands on slot 0. Without both, "always returns 10" and
// "first-matches" are the same observation.
func TestDupNamedLegWindow_ControlsDistinguishFixtureFromFinding(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		field    string
		carried  int
		wantPath []int
	}{
		// The UNIQUE column of the same window: the arm resolves it correctly, so
		// the fixture is not simply broken for every lookup.
		{"unique column Z resolves to its own slot", "Z", 2, []int{12}},
		// The duplicate name asking for the FIRST match: agrees with first-match
		// and with the carried ordinal at once, so it cannot discriminate — which
		// is exactly why the defect test carries ordinal 1 instead.
		{"duplicate name carrying ordinal 0 is the non-discriminating case", "K", 0, []int{10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg, merged := dupNamedLegFixture()
			ref := &values.FieldValue{
				Field:    tc.field,
				Typ:      values.NullableInt,
				Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
				Resolved: values.NewFieldPathOfSingleInDomain(tc.field, tc.carried, false, values.OrdinalDomainOfType(leg)),
			}
			got, ok := rebasedOrdinals(t, ref, leg, merged)
			if !ok {
				t.Fatal("CONTROL DECLINED — the fixture does not reach the name arm at all, " +
					"so every verdict in this file is void")
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantPath) {
				t.Fatalf("CONTROL produced %v, want %v", got, tc.wantPath)
			}
		})
	}
}
