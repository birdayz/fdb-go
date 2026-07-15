package values

import "testing"

// TestFieldValue_ResolveOrdinal_RFC173P1 pins the RFC-173 item C contract for
// resolveOrdinal: the LAZY runtime name->ordinal derivation is RETIRED, so a
// FieldValue over a record-typed child no longer resolves an ordinal by name —
// it DECLINES. The ordinal is bound at PLAN time on a BAKED accessor, which
// resolveOrdinal then returns directly. The declared ordinal still round-trips to
// its field name (the invariant positional evaluation rests on), and the
// unresolvable cases — nil-Child leaf, non-record child, absent lazy field —
// correctly decline.
func TestFieldValue_ResolveOrdinal_RFC173P1(t *testing.T) {
	t.Parallel()
	rec := NewRecordType("R", false, []Field{
		{Name: "id", FieldType: NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: NullableString, Ordinal: 1},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rec)

	// RFC-173 item C: a LAZY typed-child node no longer derives an ordinal by name.
	if _, ok := NewFieldValue(qov, "name", NullableString).resolveOrdinal(); ok {
		t.Fatal("lazy typed-child must DECLINE post item-C (name-derive retired)")
	}
	// The declared ordinal for "name" is its slice position (1), and it round-trips
	// to the field name — the invariant positional evaluation rests on.
	ord, ok := rec.FieldIndex("name")
	if !ok || ord != 1 {
		t.Fatalf("FieldIndex(name) = (%d,%v), want (1,true)", ord, ok)
	}
	if got, ok2 := rec.GetField(ord); !ok2 || got.Name != "name" {
		t.Fatalf("round-trip: GetField(%d).Name = %q, want %q", ord, got.Name, "name")
	}
	// A BAKED accessor carries that ordinal — resolveOrdinal returns it directly.
	baked := NewCorrelatedFieldValueWithResolvedOrdinal(qov, "name", ord, NullableString)
	if bord, bok := baked.resolveOrdinal(); !bok || bord != 1 {
		t.Fatalf("baked resolveOrdinal(name) = (%d,%v), want (1,true)", bord, bok)
	}

	// nil-Child leaf: unresolvable (no child type to bind against).
	if _, ok := NewFlatFieldValue("id", NotNullLong).resolveOrdinal(); ok {
		t.Error("nil-Child leaf must not resolve an ordinal")
	}
	// Non-record child: unresolvable.
	prim := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("p"), NotNullLong)
	if _, ok := NewFieldValue(prim, "x", NotNullLong).resolveOrdinal(); ok {
		t.Error("non-record child must not resolve an ordinal")
	}
	// Absent field: unresolvable.
	if _, ok := NewFieldValue(qov, "missing", UnknownType).resolveOrdinal(); ok {
		t.Error("absent field must not resolve an ordinal")
	}
}

// TestFieldValue_ResolveOrdinal_RawDivergentRecord_RFC173P1 pins the robustness
// the P1 review converged on: resolveOrdinal returns the SLICE
// POSITION (RecordType.FieldIndex), which is the Java ordinal, so it is sound even
// for a RAW RecordType that bypassed NewRecordType's normalization and carries a
// stored Field.Ordinal that diverges from position. This is the axis the
// enforcement test can't reach (NewRecordType normalizes the divergence away).
func TestFieldValue_ResolveOrdinal_RawDivergentRecord_RFC173P1(t *testing.T) {
	t.Parallel()
	// Raw record (bypasses NewRecordType): field "b" is at slice position 1 but
	// carries a stored Ordinal 0. resolveOrdinal must still return position 1.
	raw := &RecordType{RecordName: "M", Fields: []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 1},
		{Name: "b", FieldType: NotNullLong, Ordinal: 0},
	}}
	// Post RFC-173 item C the ordinal is bound at PLAN time. The plan-time
	// derivation (RecordType.FieldIndex) uses the SLICE POSITION (1), NOT the stored
	// Field.Ordinal (0) — so a raw divergent record bakes the correct slot. A lazy
	// node declines (name-derive retired); the baked node returns the derived slot.
	ord, ok := raw.FieldIndex("b")
	if !ok || ord != 1 {
		t.Fatalf("raw-record FieldIndex(b) = (%d,%v), want (1,true) — the SLICE POSITION, not the stored Ordinal 0", ord, ok)
	}
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("raw"), raw)
	if _, lok := NewFieldValue(qov, "b", NotNullLong).resolveOrdinal(); lok {
		t.Fatal("lazy typed-child must DECLINE post item-C (name-derive retired)")
	}
	fv := NewCorrelatedFieldValueWithResolvedOrdinal(qov, "b", ord, NotNullLong)
	rord, rok := fv.resolveOrdinal()
	if !rok || rord != 1 {
		t.Fatalf("baked resolveOrdinal(b) = (%d,%v), want (1,true)", rord, rok)
	}
	if g, _ := raw.GetField(rord); g.Name != "b" {
		t.Fatalf("raw round-trip: GetField(%d).Name = %q, want b", rord, g.Name)
	}
}

// TestFieldValue_OrdinalOrderingPrecondition_RFC173P1 pins the soundness
// invariant the ordinal substrate rests on: ordinal access (resolveOrdinal ->
// declared Field.Ordinal) must agree with slice-position access (GetField) — and
// therefore with the authoritative name path. This holds IFF Fields[i].Ordinal
// == i. NewRecordType now ENFORCES that by normalising Ordinal to slice position
// (RFC-173, matching Java's positionally-indexed Record), so the substrate is
// sound by construction. The raw-bypass case is the teeth: it shows the wrong-field
// read that normalisation prevents — without it the invariant would be untested.
func TestFieldValue_OrdinalOrderingPrecondition_RFC173P1(t *testing.T) {
	t.Parallel()
	// Well-ordered control: round-trip holds.
	ordered := NewRecordType("R", false, []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 0},
		{Name: "b", FieldType: NotNullLong, Ordinal: 1},
	})
	// Post RFC-173 item C a LAZY node declines; the ordinal is bound at plan time
	// (slice position via FieldIndex) and a BAKED accessor returns it.
	ordB, ok := ordered.FieldIndex("b")
	if !ok || ordB != 1 {
		t.Fatalf("ordered: FieldIndex(b) = (%d,%v), want (1,true)", ordB, ok)
	}
	fvB := NewCorrelatedFieldValueWithResolvedOrdinal(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), ordered), "b", ordB, NotNullLong)
	if o, bok := fvB.resolveOrdinal(); !bok || o != 1 {
		t.Fatalf("ordered: baked resolveOrdinal(b) = (%d,%v), want (1,true)", o, bok)
	}
	if g, ok := ordered.GetField(ordB); !ok || g.Name != fvB.Field {
		t.Fatalf("ordered round-trip broke: GetField(%d).Name=%q, want %q", ordB, g.Name, fvB.Field)
	}

	// ENFORCEMENT: NewRecordType normalises divergent input ordinals to slice
	// position, so ordinal access is sound regardless of what the caller passed.
	normalized := NewRecordType("N", false, []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 9}, // divergent inputs...
		{Name: "b", FieldType: NotNullLong, Ordinal: 3},
	})
	for i, f := range normalized.Fields {
		if f.Ordinal != i {
			t.Fatalf("NewRecordType must normalise Field[%d].Ordinal to %d, got %d", i, i, f.Ordinal)
		}
	}
	ordA, ok := normalized.FieldIndex("a")
	if !ok || ordA != 0 {
		t.Fatalf("normalised: FieldIndex(a) = (%d,%v), want (0,true)", ordA, ok)
	}
	fvA := NewCorrelatedFieldValueWithResolvedOrdinal(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("n"), normalized), "a", ordA, NotNullLong)
	if o, aok := fvA.resolveOrdinal(); !aok || o != 0 {
		t.Fatalf("normalised: baked resolveOrdinal(a) = (%d,%v), want (0,true)", o, aok)
	}
	if g, _ := normalized.GetField(ordA); g.Name != "a" {
		t.Fatalf("normalised round-trip broke: GetField(%d).Name=%q, want a", ordA, g.Name)
	}

	// TEETH — why the guard matters: a RAW RecordType (bypassing NewRecordType)
	// with Ordinal != slice position makes ordinal access read the WRONG field.
	// This is exactly the divergence NewRecordType's normalisation prevents.
	raw := &RecordType{RecordName: "M", Fields: []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 1},
		{Name: "b", FieldType: NotNullLong, Ordinal: 0},
	}}
	f, _ := raw.LookupField("a") // declared Ordinal 1
	if g, _ := raw.GetField(f.Ordinal); g.Name == "a" {
		t.Fatal("raw mis-ordered record should expose the ordinal!=slice-position divergence the guard prevents")
	}
}
