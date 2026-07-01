package values

import "testing"

// TestFieldValue_ResolveOrdinal_RFC173P1 pins P1's name->ordinal substrate for
// the RFC-173 column-resolution migration: a FieldValue over a record-typed
// child resolves f.Field to its declared ordinal (dark, non-authoritative), the
// ordinal round-trips to the same field name (the invariant P2's positional
// evaluation rests on), and the unresolvable cases — nil-Child leaf, non-record
// child, absent field — correctly decline (those references stay on the name
// path until P2 lands).
func TestFieldValue_ResolveOrdinal_RFC173P1(t *testing.T) {
	t.Parallel()
	rec := NewRecordType("R", false, []Field{
		{Name: "id", FieldType: NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: NullableString, Ordinal: 1},
	})
	qov := NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), rec)

	// Resolvable field -> its declared ordinal.
	fv := NewFieldValue(qov, "name", NullableString)
	ord, ok := fv.resolveOrdinal()
	if !ok || ord != 1 {
		t.Fatalf("resolveOrdinal(name) = (%d,%v), want (1,true)", ord, ok)
	}
	// Round-trip invariant P2 will rely on: GetField(ordinal).Name == f.Field.
	if got, ok2 := rec.GetField(ord); !ok2 || got.Name != fv.Field {
		t.Fatalf("round-trip: GetField(%d).Name = %q, want %q", ord, got.Name, fv.Field)
	}

	// nil-Child leaf: unresolvable (the P1 hard case — stays on the name path).
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
	fvB := NewFieldValue(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("q"), ordered), "b", NotNullLong)
	ordB, ok := fvB.resolveOrdinal()
	if !ok || ordB != 1 {
		t.Fatalf("ordered: resolveOrdinal(b) = (%d,%v), want (1,true)", ordB, ok)
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
	fvA := NewFieldValue(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("n"), normalized), "a", NotNullLong)
	ordA, ok := fvA.resolveOrdinal()
	if !ok || ordA != 0 {
		t.Fatalf("normalised: resolveOrdinal(a) = (%d,%v), want (0,true)", ordA, ok)
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
