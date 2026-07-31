package values

import "testing"

// TestFinalizeSeedWindows_BoxBuried pins finalizeSeedWindows's per-buried-leaf
// sub-window derivation for a mixed seed: a CLUSTERED BOX outer (a
// FULL/LEFT-OUTER outer under EXISTS) decomposes into per-leaf windows, and
// two aliases' DUP-NAMED columns resolve to DIFFERENT absolute slots. Without
// this decomposition, a seed would carry only the box-run window (named after
// its rightmost leaf), so a qualified read of the left leaf would silently
// miss and a read of the right leaf would first-match the earlier leaf's
// dup-named slot. Multi-alias outers are still declined at
// unnestExistsSeedSafe, so this substrate currently has no production caller.
func TestFinalizeSeedWindows_BoxBuried(t *testing.T) {
	t.Parallel()
	o := NamedCorrelationIdentifier("O")
	c := NamedCorrelationIdentifier("C")
	// Box concat of two leaves O[0,2) and C[2,2), BOTH with dup-named ID + V. The
	// run is keyed by its RIGHTMOST leaf C (sourceBinding convention).
	//
	// The leaves state their IDENTITIES, and they have to: the window map is keyed
	// by identity, so a leg table whose entries omit Alias files every one of them
	// under the zero identifier and the second leaf displaces the first. The
	// fixture used to omit them, which modelled a leg table production cannot
	// build.
	boxType := &RecordType{
		Fields: []Field{
			{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: NotNullLong, Ordinal: 1},
			{Name: "ID", FieldType: NotNullLong, Ordinal: 2},
			{Name: "V", FieldType: NotNullLong, Ordinal: 3},
		},
		Legs: []RecordTypeLeg{
			NewRecordTypeLeg(o, "O", 0, 2),
			NewRecordTypeLeg(c, "C", 2, 2),
		},
	}
	windows := map[CorrelationIdentifier]OrdinalSeedLegWindow{
		c: {Offset: 0, Typ: boxType, Alias: c}, // the box run at prefix offset 0
	}
	names := map[CorrelationIdentifier]string{c: "C"}
	out, mergedType := finalizeSeedWindows(windows, names, make([]Field, 4))

	ow, hasO := out[o]
	cw, hasC := out[c]
	if !hasO || ow.Offset != 0 {
		t.Fatalf("O (left leaf) window = %+v (present=%v), want Offset 0", ow, hasO)
	}
	if !hasC || cw.Offset != 2 {
		t.Fatalf("C (right leaf) window = %+v (present=%v), want Offset 2 (box run REPLACED by rightmost leaf)", cw, hasC)
	}
	// Each leaf window is leaf-relative (its own [ID,V]); a dup-named ID resolves to
	// DIFFERENT absolute slots — O.ID -> 0, C.ID -> 2 (not the flat first-match 0).
	oi, okO := ow.Typ.FieldIndex("ID")
	ci, okC := cw.Typ.FieldIndex("ID")
	if !okO || !okC || ow.Offset+oi != 0 || cw.Offset+ci != 2 {
		t.Fatalf("dup-named ID disambiguation: O.ID abs=%d (want 0), C.ID abs=%d (want 2)", ow.Offset+oi, cw.Offset+ci)
	}
	// The merged Legs carry the per-leaf boundaries (a box run emits its SUBS only,
	// never a run-level entry that would shadow the rightmost leaf with the concat).
	if mergedType == nil || len(mergedType.Legs) != 2 {
		t.Fatalf("merged Legs = %v, want the 2 per-leaf boundaries", mergedType)
	}
	// The sub-windows carry the buried legs' OWN display bindings into the merged
	// leg table. That text still has live readers (the dotted channel) and it is
	// the producer's stated Name, not a re-spelling of the identity.
	byName := map[string]RecordTypeLeg{}
	for _, l := range mergedType.Legs {
		byName[l.Name] = l
	}
	for _, want := range []struct {
		name  string
		alias CorrelationIdentifier
		start int
	}{{"O", o, 0}, {"C", c, 2}} {
		got, ok := byName[want.name]
		if !ok {
			t.Fatalf("merged leg %q absent from %v", want.name, mergedType.Legs)
		}
		if !SameLeg(got.Alias, want.alias) || got.Start != want.start {
			t.Fatalf("merged leg %q = alias %q start %d, want alias %q start %d",
				want.name, got.Alias.Name(), got.Start, want.alias.Name(), want.start)
		}
	}
}

// TestOrdinalSeedLegWindows_CaseDisjointLegsAreTwoWindows is the identity-keying
// conversion's own regression pin, and it is the one shape that separates an
// identity-keyed window map from the upper-folded text map it replaced.
//
// The two alias namespaces are deliberately DISJOINT: user correlations are
// upper-folded at the semantic scope's registration chokepoint, and
// UniqueCorrelationIdentifier mints the machine counter lowercase, so a quoted
// "q$5" must not be able to forge a planner-minted q$5. SameLeg exists to hold
// that line. A map keyed by the UPPER FOLD of a correlation crosses it: `q$5`
// and `Q$5` become one key, the second leg reads as a duplicate of the first,
// and the whole seed DECLINES — a two-leg join silently losing its ordinal
// layout because two unrelated quantifiers were spelled alike.
//
// Under identity keying they are two legs with two windows, each addressable by
// its own correlation.
func TestOrdinalSeedLegWindows_CaseDisjointLegsAreTwoWindows(t *testing.T) {
	t.Parallel()
	machine := NamedCorrelationIdentifier("q$5") // a planner mint
	quoted := NamedCorrelationIdentifier("Q$5")  // a user's quoted alias
	leg := func(corr CorrelationIdentifier, cols ...string) []RecordConstructorField {
		fields := make([]Field, len(cols))
		for i, c := range cols {
			fields[i] = Field{Name: c, FieldType: NotNullLong, Ordinal: i}
		}
		qov := NewQuantifiedObjectValueOfType(corr, &RecordType{Fields: fields})
		out := make([]RecordConstructorField, len(cols))
		for i, c := range cols {
			fv, err := NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				t.Fatalf("bake %s.%s: %v", corr.Name(), c, err)
			}
			fv.Field = c
			out[i] = RecordConstructorField{Name: c, Value: fv}
		}
		return out
	}
	fields := append(leg(machine, "A1", "A2"), leg(quoted, "B1")...)
	windows, merged := OrdinalSeedLegWindows(NewRawRecordConstructorValue(fields...))
	if windows == nil || merged == nil {
		t.Fatal("a two-leg seed whose legs differ only in the CASE of their " +
			"correlations was DECLINED. That is the upper-folded key namespace " +
			"collapsing two distinct quantifiers into one, which the run-boundary " +
			"and duplicate checks then read as a split run. The legs are distinct " +
			"identities and must window separately.")
	}
	mw, hasM := windows[machine]
	qw, hasQ := windows[quoted]
	if !hasM || !hasQ {
		t.Fatalf("windows are missing a case-disjoint leg: q$5 present=%v, Q$5 present=%v", hasM, hasQ)
	}
	if mw.Offset != 0 || len(mw.Typ.Fields) != 2 {
		t.Fatalf("q$5 window = offset %d width %d, want offset 0 width 2", mw.Offset, len(mw.Typ.Fields))
	}
	if qw.Offset != 2 || len(qw.Typ.Fields) != 1 {
		t.Fatalf("Q$5 window = offset %d width %d, want offset 2 width 1", qw.Offset, len(qw.Typ.Fields))
	}
	// Each window states the identity it is filed under. A reader keyed by the
	// correlation and a consumer asking "which leg is this?" must not be able to
	// disagree, which is the whole point of the key and the field being one fact.
	if !SameLeg(mw.Alias, machine) || !SameLeg(qw.Alias, quoted) {
		t.Fatalf("a window's key and its stated Alias disagree: q$5 -> %q, Q$5 -> %q",
			mw.Alias.Name(), qw.Alias.Name())
	}
}
