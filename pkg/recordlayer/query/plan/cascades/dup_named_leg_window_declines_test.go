package cascades

// The NAME arm of rebaseOuterLegValueOrdinal against a leg window that holds two
// columns of one name — the case its two SIBLING arms in the same switch always
// defended against and it, for a while, did not.
//
// THIS FILE PINS A CLOSURE. It was written to pin the DEFECT — a green run then
// meant the asymmetry was still present — and RFC-228 closed it, so the same
// assertions now read the other way: a green run means all three arms decline on
// the ambiguous name, and a resolved answer coming back means a first-match
// lookup has returned. The failure messages carry that reading.
//
// The asymmetry it was written against. rebaseOuterLegValueOrdinal dispatches a
// leg reference to one of three arms, all resolving against the SAME window type
// `w.Typ`:
//
//	multi-accessor -> FieldPath.ReAnchorRootInto, which COUNTS matches and
//	                  declines on `dupes > 1` (values.go: "root column name is
//	                  ambiguous in the flowed layout")
//	FrontierPinned -> carries acc.Ordinal and performs no name lookup at all,
//	                  its comment naming this exact hazard: an opaque box leg
//	                  can expose DUPLICATE buried column names (`A.K` and `B.K`
//	                  merged into one leg), where a first-match remap of the
//	                  already-baked ref would silently probe the WRONG column
//	name arm       -> a FIRST-MATCH scan with no duplicate detection, justified
//	                  in place by "A single source leg has no duplicate column
//	                  names, so FieldIndex(Field) is the leg-local ordinal."
//
// That justification was the claim under test, and its own siblings contradicted
// it: the window is not always a single source leg. A CLUSTERED BOX run window
// concatenates every buried leaf's columns, and finalizeSeedWindows narrows only
// the RIGHTMOST leaf's entry, so a run window that was not narrowed can hold
// `A.K` and `B.K` as two fields both named K. The arm now resolves through
// RecordType.FieldIndexUnique, which declines on exactly that shape; the
// first-match lookup it used to call was deleted rather than left as a copy
// target.
//
// WHY THE CARRIED ORDINAL MADE THIS SHARPER THAN "LAZY REFS ARE UNDEFENDED".
// The name arm is not reached only by lazy references. A SOURCE-RELATIVE baked
// reference — resolved, unpinned, single-accessor — reaches it too, and it
// ARRIVES CARRYING the correct leg-local ordinal. The arm discarded that ordinal
// and re-derived one by first-match name, while the multi-accessor arm, given
// the same disagreement, declined outright ("carried ordinal disagrees with the
// flowed layout"). One arm treated the carried ordinal as authoritative enough
// to refuse on, and another silently overruled it. Both decline now — and note
// the decline, not the carried ordinal, is what this file asserts: honouring the
// ordinal in the name arm is a different and larger change.
//
// A duplicate-named RecordType is built as a RAW literal here because
// NewRecordType PANICS on duplicate field names. That is not a harness dodge —
// it is how such a window arises in production too, at ordinal_seed_layout.go,
// which is why the by-name lookup has to work on "a raw RecordType that was
// built without NewRecordType's normalization" and therefore has to face
// duplicates at all.

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

// TestDupNamedLegWindow_NameArmDeclinesOnTheAmbiguousName is the closure,
// stated two-sidedly.
//
// The reference is `L.K` resolved to leg-local ordinal 1 — the SECOND K, which
// is the column the user asked for. An acceptable output is merged slot 11
// (Offset 10 + 1) or a DECLINE; the arm produces the DECLINE. Before the fix it
// produced slot 10, the first match.
func TestDupNamedLegWindow_NameArmDeclinesOnTheAmbiguousName(t *testing.T) {
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
	if ok {
		which := "an unrecognised slot"
		switch fmt.Sprint(got) {
		case fmt.Sprint([]int{10}):
			which = "merged slot 10 — the FIRST match, which is the defect this file was " +
				"written to pin. A first-match name lookup came back"
		case fmt.Sprint([]int{11}):
			which = "merged slot 11 — the column the reference's own carried ordinal " +
				"names. That is the CORRECT column, and the arm is not entitled to it: " +
				"its contract is that its input is non-baked, so an ordinal arriving here " +
				"is stated in a layout this site cannot identify. If the arm was " +
				"deliberately taught to honour the carried ordinal, that is a different " +
				"and larger change than the decline, and this test should be replaced " +
				"rather than relaxed"
		}
		t.Fatalf("the name arm RESOLVED and produced %v (%s).\n\n"+
			"Two buried columns of one name sit in this leg window, so no answer here "+
			"is distinguishable from a guess: slots 10 and 11 are both real merged "+
			"columns of the same type and nothing downstream rejects the wrong one. "+
			"The arm must DECLINE, exactly as its two siblings do against this same "+
			"window.", got, which)
	}

	t.Logf("`L.K` over a leg window declaring K twice DECLINES, so the wrong-column " +
		"read is closed. Before the fix it produced merged slot 10: the carried " +
		"leg-local ordinal 1 was discarded and a first-match name lookup answered the " +
		"leg's slot 0 instead. The lookup that did that no longer exists.")
}

// TestDupNamedLegWindow_SiblingArmsDefendAgainstTheSameWindow is the other side.
//
// Without it the test above is a lone assertion; with it, the three arms are
// pinned as ONE property — the same window, the same duplicate name, defended
// three times. It was written when the count was two-of-three and the gap was
// the finding; the siblings still need pinning, because if either ever stops
// defending, that is a regression this is what reports.
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
					"so every verdict in this file is void. In particular the decline " +
					"asserted above would be passing for the wrong reason: an arm that " +
					"refuses everything is not an arm that detects ambiguity.")
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantPath) {
				t.Fatalf("CONTROL produced %v, want %v", got, tc.wantPath)
			}
		})
	}

	// The duplicate name asking for the FIRST match. Before the fix this was the
	// NON-DISCRIMINATING case: first-match and the carried ordinal both answered
	// slot 10, which is why the defect test carries ordinal 1 instead. It is now
	// a second decline, and it earns its place by ruling out a narrower fix —
	// one that declined only when the carried ordinal DISAGREED with the first
	// match would still resolve here, and would still be a guess.
	t.Run("duplicate name carrying ordinal 0 declines too", func(t *testing.T) {
		t.Parallel()
		leg, merged := dupNamedLegFixture()
		ref := &values.FieldValue{
			Field:    "K",
			Typ:      values.NullableInt,
			Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
			Resolved: values.NewFieldPathOfSingleInDomain("K", 0, false, values.OrdinalDomainOfType(leg)),
		}
		if got, ok := rebasedOrdinals(t, ref, leg, merged); ok {
			t.Fatalf("an ambiguous name whose carried ordinal happens to AGREE with the "+
				"first match resolved to %v. The ambiguity is a property of the WINDOW, "+
				"not of whether the two candidate answers coincide — a decline "+
				"conditioned on disagreement resolves this case and is still guessing.", got)
		}
	})
}
