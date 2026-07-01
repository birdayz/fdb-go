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
