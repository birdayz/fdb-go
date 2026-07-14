package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRebaseOuterLegValueOrdinal_LegLocalFrontierPinned_UsesBakedOrdinal pins the
// review-hardened leg-local arm of rebaseOuterLegValueOrdinal: a LEG-LOCAL baked
// ref (`ofOrdinal(QOV(leg), i)`, child a source leg still in `windows`, NOT the
// merged QOV) must translate onto the merged row using its BAKED ordinal
// (acc.Ordinal), not by re-deriving the slot from the display field name.
//
// The trap this guards: an OPAQUE box leg can expose DUPLICATE buried column names
// (`A.K` and `B.K` concatenated into one leg's type). If the ref were resolved by
// name (FieldIndex), a ref baked to the SECOND "K" (leg-local ordinal 1) would be
// silently remapped to the FIRST "K" (ordinal 0) — probing/filtering the wrong
// column, wrong rows. The merged-QOV idempotence guard only passes MERGED refs
// through, so a leg-local one reaches this arm; it must carry acc.Ordinal.
func TestRebaseOuterLegValueOrdinal_LegLocalFrontierPinned_UsesBakedOrdinal(t *testing.T) {
	// A box leg's type with DUPLICATE column names: two "K" columns at leg-local
	// ordinals 0 and 1 (the A.K / B.K concat a merge-opaque box can carry).
	dupLeg := &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "K", FieldType: values.NullableInt, Ordinal: 1},
	}}
	// The merged positional row — wide enough that offset+baked-ordinal is in range.
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedType := &values.RecordType{Fields: mergedFields}
	mergedQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mergedType)

	// The leg window: leg "L" starts at merged offset 10.
	const legOffset = 10
	windows := map[string]ordinalLegWindow{
		"L": {Offset: legOffset, Typ: dupLeg},
	}

	// A leg-local baked ref pointing at the SECOND "K" (leg-local ordinal 1), whose
	// display Field is the ambiguous "K". Child is the LEG QOV (not the merged QOV),
	// so it passes the merged-QOV idempotence guard and reaches the leg-local arm.
	ref := &values.FieldValue{
		Field:    "K",
		Typ:      values.NullableInt,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		Resolved: values.NewFieldPathOfSingle("K", 1, true),
	}

	out, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV)
	if !ok {
		t.Fatalf("rebase declined a valid leg-local baked ref")
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV {
		t.Fatalf("rebase produced %T, want *FieldValue", out)
	}
	acc, single := fv.Resolved.Single()
	if !single {
		t.Fatalf("rebased ref is not a single-accessor path: %+v", fv.Resolved)
	}
	// acc.Ordinal path: legOffset + baked ordinal 1 = 11 (B.K, the intended slot).
	// FieldIndex("K") path would give legOffset + first-match 0 = 10 (A.K, WRONG).
	if acc.Ordinal != legOffset+1 {
		t.Fatalf("rebased to merged ordinal %d, want %d (baked ordinal preserved); "+
			"%d would mean the buggy FieldIndex first-match remapped the dup-named leg column",
			acc.Ordinal, legOffset+1, legOffset)
	}
	// The rebased ref must be over the MERGED QOV.
	childQOV, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV || childQOV.Correlation != mergedQOV.Correlation {
		t.Fatalf("rebased ref child = %v, want merged QOV %v", fv.Child, mergedQOV.Correlation)
	}
}

// TestRebaseOuterLegValueOrdinal_LegLocalOrdinalPastWindow_Declines pins the
// bound-check: a leg-local baked ordinal relative to a FULL concat type that spills
// PAST a narrowed leg subwindow must DECLINE (correct-or-loud → name-model), not
// silently rebase onto a neighbouring leg's merged slot (a wrong-column read that
// NewFieldValueOfOrdinal's merged-range check would NOT catch).
func TestRebaseOuterLegValueOrdinal_LegLocalOrdinalPastWindow_Declines(t *testing.T) {
	// A NARROWED leg window: only 2 columns in view, but the baked ref's ordinal (5)
	// is relative to the child's wider full-concat type.
	narrowLeg := &values.RecordType{Fields: []values.Field{
		{Name: "P", FieldType: values.NullableInt, Ordinal: 0},
		{Name: "Q", FieldType: values.NullableInt, Ordinal: 1},
	}}
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedType := &values.RecordType{Fields: mergedFields}
	mergedQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mergedType)
	windows := map[string]ordinalLegWindow{"L": {Offset: 10, Typ: narrowLeg}}

	ref := &values.FieldValue{
		Field:    "Q",
		Typ:      values.NullableInt,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L")),
		Resolved: values.NewFieldPathOfSingle("Q", 5, true), // ordinal 5 spills past the 2-field window
	}
	if _, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV); ok {
		t.Fatal("a baked ordinal past the leg window must DECLINE (correct-or-loud), not rebase onto a neighbouring slot")
	}
}

// TestRebaseOuterLegValueOrdinal_MergedRefPassesThrough pins the idempotence guard:
// a ref ALREADY baked over the merged QOV (a multi-esq peel box's plan-time bake)
// is final and must pass through untouched — re-baking would double-count the leg
// offset (the spurious-decline bug the guard fixes).
func TestRebaseOuterLegValueOrdinal_MergedRefPassesThrough(t *testing.T) {
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	mergedType := &values.RecordType{Fields: mergedFields}
	mergedQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mergedType)
	windows := map[string]ordinalLegWindow{"L": {Offset: 10, Typ: mergedType}}

	// An already-merged baked ref: child IS the merged QOV, ordinal 12 (final).
	baked, err := values.NewFieldValueOfOrdinal(mergedQOV, 12)
	if err != nil {
		t.Fatalf("build baked ref: %v", err)
	}
	out, ok := rebaseOuterLegValueOrdinal(baked, windows, mergedQOV)
	if !ok {
		t.Fatalf("rebase declined an already-merged baked ref")
	}
	if out != values.Value(baked) {
		t.Fatalf("already-merged ref was not passed through unchanged: got %v", out)
	}
}
