package values

import "testing"

// TestTranslateNullExtendedPhaseRootCrossesOnlyTheWideningBoundary drives every
// arm of the null-extension crossing.
//
// The crossing exists because it is the one phase boundary where a row's exact
// type legitimately changes, and it is therefore the one place a bridge could be
// talked into accepting two rows that are NOT the same row. Each arm below is a
// distinct way to try: the wrong direction, a genuinely different shape, and a
// root that merely arrived alongside. Only the corpus shape (widening) exercises
// the accept arm on its own, so the rejects are driven here rather than left to
// whatever plans happen to be planned.
func TestTranslateNullExtendedPhaseRootCrossesOnlyTheWideningBoundary(t *testing.T) {
	t.Parallel()

	row := &RecordType{Fields: []Field{
		{Name: "BID", Ordinal: 0, FieldType: NullableLong},
		{Name: "REF", Ordinal: 1, FieldType: NullableLong},
	}}
	present := mustTypedQOV(t, "present", row)
	absentCapable := mustTypedQOV(t, "absent", WithNullability(row, true))

	fieldOf := func(source QuantifiedObjectValue, ordinal int) Value {
		t.Helper()
		v, err := ResolveFieldOrdinals(source, []int{ordinal})
		if err != nil {
			t.Fatalf("field %d: %v", ordinal, err)
		}
		return v
	}

	// The accept arm: a read INTO the row is unchanged by whether the row may be
	// absent, so the ordinal survives and the leaf type must not move.
	read := fieldOf(present, 1)
	crossed, err := TranslateNullExtendedPhaseRoot(read, present, absentCapable)
	if err != nil {
		t.Fatalf("widening a present row to an absent-capable one was refused: %v", err)
	}
	crossedField, ok := AsFieldValue(crossed)
	if !ok {
		t.Fatalf("crossing produced %T, want a FieldValue", crossed)
	}
	if crossedField.ChildValue() != absentCapable {
		t.Fatal("the crossed read is not rooted on the null-extended row")
	}
	if got := crossedField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("crossed ordinal path = %v, want [1] — the null-extension moves no columns", got)
	}
	if !crossedField.Type().Equals(NullableLong) {
		t.Fatalf("crossed leaf type = %s, want LONG NULL unchanged", crossedField.Type())
	}

	// The whole-row read IS supposed to change type: the caller asked for the row
	// this operator emits, and that row may be absent.
	whole, err := TranslateNullExtendedPhaseRoot(present, present, absentCapable)
	if err != nil {
		t.Fatalf("whole-row crossing: %v", err)
	}
	if whole != Value(absentCapable) {
		t.Fatalf("whole-row crossing produced %v, want the null-extended root itself", whole)
	}

	// Wrong direction. Narrowing asserts that a row which may be absent is always
	// present — the exact fact an outer join exists to record.
	if _, err := TranslateNullExtendedPhaseRoot(
		fieldOf(absentCapable, 0), absentCapable, present); err == nil {
		t.Fatal("narrowing an absent-capable row onto a present phase was ACCEPTED; that " +
			"claims a null-extended row is always there")
	}

	// Same nullability on both sides is not a null-extension at all; the ordinary
	// phase translation owns that case and this one must not silently take it.
	otherPresent := mustTypedQOV(t, "other", row)
	if _, err := TranslateNullExtendedPhaseRoot(
		fieldOf(present, 0), present, otherPresent); err == nil {
		t.Fatal("a same-nullability phase pair was accepted as a null-extension")
	}

	// A genuinely different row. Only the TOP-LEVEL bit may move; arity, names,
	// ordinals and nested nullability are shape.
	narrower := mustTypedQOV(t, "narrower", WithNullability(&RecordType{Fields: []Field{
		{Name: "BID", Ordinal: 0, FieldType: NullableLong},
	}}, true))
	if _, err := TranslateNullExtendedPhaseRoot(fieldOf(present, 0), present, narrower); err == nil {
		t.Fatal("a row of different arity was accepted as the same row widened")
	}
	renamed := mustTypedQOV(t, "renamed", WithNullability(&RecordType{Fields: []Field{
		{Name: "BID", Ordinal: 0, FieldType: NullableLong},
		{Name: "OTHER", Ordinal: 1, FieldType: NullableLong},
	}}, true))
	if _, err := TranslateNullExtendedPhaseRoot(fieldOf(present, 0), present, renamed); err == nil {
		t.Fatal("a row with a different field name was accepted as the same row widened")
	}

	// A root that merely travelled alongside is left alone — byte-for-byte, so a
	// caller cannot mistake "untouched" for "resolved".
	foreign := mustTypedQOV(t, "foreign", row)
	foreignRead := fieldOf(foreign, 0)
	kept, err := TranslateNullExtendedPhaseRoot(foreignRead, present, absentCapable)
	if err != nil {
		t.Fatalf("a foreign root made the crossing fail instead of passing through: %v", err)
	}
	if kept != foreignRead {
		t.Fatal("a foreign root was rewritten by a crossing that has no authority over it")
	}
}

func mustTypedQOV(t *testing.T, name string, typ Type) QuantifiedObjectValue {
	t.Helper()
	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(name), typ)
	if err != nil {
		t.Fatalf("QOV %s: %v", name, err)
	}
	return qov
}
