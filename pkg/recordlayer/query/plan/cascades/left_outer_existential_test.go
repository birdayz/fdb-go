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

// The DRIFT tripwire's true branch, which nothing had ever taken.
//
// rebaseOuterLegValueOrdinal's `!isLeg` arm normally means "a genuinely non-outer
// reference" — the FlatMap inner — and passes the node through. But if the merged
// type's own Legs contain the reference's correlation while the derived `windows`
// do not, the two layout derivations have DRIFTED, and passing the node through
// would silently read the inner row where an outer leg was meant. So the arm
// declines LOUD instead.
//
// That branch had zero positive population: over the whole measured corpus the
// tripwire's comparison was "neither" on 1000 of 1000 samples, so the conversion
// of that comparison to values.SameLeg was unfalsified — every test exercised only
// the pass-through side. A guard whose firing path is never taken is a guard
// nobody has checked, and this one is the difference between a loud decline and
// wrong rows.
//
// So the drift is constructed directly: a merged type whose Legs name correlation
// "D" against a windows map that does not. Through the real function, not a copy —
// a copy would pin the copy.
func TestRebaseOuterLegValueOrdinal_DriftBetweenWindowsAndMergedLegs_Declines(t *testing.T) {
	t.Parallel()

	driftCorr := values.NamedCorrelationIdentifier("D")
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "DK", FieldType: values.NullableInt, Ordinal: 0},
	}}

	mergedFields := make([]values.Field, 8)
	for i := range mergedFields {
		mergedFields[i] = values.Field{Name: fmt.Sprintf("M%d", i), FieldType: values.NullableInt, Ordinal: i}
	}
	// The merged type KNOWS about leg D — this is the half of the layout that is
	// derived from the plan's own leg boundaries.
	mergedType := &values.RecordType{
		Fields: mergedFields,
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(driftCorr, "D", 0, 1),
		},
	}
	mergedQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mergedType)

	// ...and the OTHER half does not. The windows map carries an unrelated leg, so
	// a reference over D misses it. That mismatch is the drift.
	windows := map[string]ordinalLegWindow{
		"L": {Offset: 4, Typ: legType},
	}

	ref := &values.FieldValue{
		Field: "DK",
		Typ:   values.NullableInt,
		Child: values.NewQuantifiedObjectValue(driftCorr),
	}

	if _, ok := rebaseOuterLegValueOrdinal(ref, windows, mergedQOV); ok {
		t.Error("the rebase ACCEPTED a reference whose correlation is a known leg boundary " +
			"of the merged type but absent from the derived windows. The two derivations " +
			"have drifted, and accepting means the reference is passed through to read the " +
			"FlatMap INNER row where an OUTER leg was meant — wrong rows, silently. This " +
			"arm must decline.")
	}

	// The negative control, which is what makes the assertion above about DRIFT
	// rather than about any missing window: a reference over a correlation the
	// merged type does NOT claim as a leg is a genuine non-outer reference, and must
	// pass through untouched. Without this, a tripwire that fired on everything
	// would satisfy the test above while declining every legitimate inner reference.
	innerCorr := values.NamedCorrelationIdentifier("INNER")
	innerRef := &values.FieldValue{
		Field: "IK",
		Typ:   values.NullableInt,
		Child: values.NewQuantifiedObjectValue(innerCorr),
	}
	out, ok := rebaseOuterLegValueOrdinal(innerRef, windows, mergedQOV)
	if !ok {
		t.Error("the rebase declined a genuine FlatMap-INNER reference — a correlation the " +
			"merged type does not claim as a leg is not drift, and declining it would " +
			"reject every correlated inner reference there is")
	}
	if out != innerRef {
		t.Errorf("an inner reference must pass through UNTOUCHED, got %v", out)
	}

	// And the tripwire must answer by leg IDENTITY, not by folded text. A leg whose
	// identity is a planner-minted lowercase q$N must not be matched by a quoted
	// user alias that upper-folds onto it: that would fire the drift decline on a
	// perfectly good plan, which is a silent loss of the whole existential rewrite.
	minted := values.NamedCorrelationIdentifier("q$11")
	forgedType := &values.RecordType{
		Fields: mergedFields,
		Legs: []values.RecordTypeLeg{
			values.NewRecordTypeLeg(minted, "Q$11", 0, 1),
		},
	}
	forgedMergedQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), forgedType)
	forgedRef := &values.FieldValue{
		Field: "FK",
		Typ:   values.NullableInt,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("Q$11")),
	}
	if _, ok := rebaseOuterLegValueOrdinal(forgedRef, windows, forgedMergedQOV); !ok {
		t.Error(`the drift tripwire fired for a quoted "Q$11" against a planner-minted q$11 ` +
			`leg — it is matching leg TEXT (or a fold) rather than leg IDENTITY, so a ` +
			`case-variant alias is mistaken for the leg it guards and a valid plan declines`)
	}
}
