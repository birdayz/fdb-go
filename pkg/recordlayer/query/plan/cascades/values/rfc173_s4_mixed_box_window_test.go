package values

import "testing"

// TestRFC173S4_FinalizeSeedWindows_BoxBuried pins the RFC-173 S4 channel-2
// substrate: the mixed-seed branch now runs the SAME per-buried-leaf sub-window
// derivation as the pristine branch (shared finalizeSeedWindows), so a CLUSTERED
// BOX outer (a FULL/LEFT-OUTER outer under EXISTS) decomposes into per-leaf
// windows and two aliases' DUP-NAMED columns resolve to DIFFERENT absolute slots.
// Before this, the mixed branch emitted only the box-run window (named after its
// rightmost leaf), so a qualified read of the left leaf silently missed and a
// read of the right leaf first-matched the earlier leaf's dup-named slot — the
// c5a hazard. Guard stays UP (multi-alias outers are still declined at
// unnestExistsSeedSafe), so this is a dormant substrate landing.
func TestRFC173S4_FinalizeSeedWindows_BoxBuried(t *testing.T) {
	t.Parallel()
	// Box concat of two leaves O[0,2) and C[2,2), BOTH with dup-named ID + V. The
	// run is keyed by its RIGHTMOST leaf C (sourceBinding convention).
	boxType := &RecordType{
		Fields: []Field{
			{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: NotNullLong, Ordinal: 1},
			{Name: "ID", FieldType: NotNullLong, Ordinal: 2},
			{Name: "V", FieldType: NotNullLong, Ordinal: 3},
		},
		Legs: []RecordTypeLeg{{Name: "O", Start: 0, Width: 2}, {Name: "C", Start: 2, Width: 2}},
	}
	windows := map[string]OrdinalSeedLegWindow{
		"C": {Offset: 0, Typ: boxType}, // the box run at prefix offset 0
	}
	out, mergedType := finalizeSeedWindows(windows, make([]Field, 4))

	ow, hasO := out["O"]
	cw, hasC := out["C"]
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
}
