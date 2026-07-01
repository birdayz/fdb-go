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

// TestFieldValue_OrdinalOrderingPrecondition_RFC173P1 is P1's dual-mode assert,
// expressed structurally: the ordinal path (resolveOrdinal -> declared Field.Ordinal)
// agrees with slice-position access (GetField) — and therefore with the
// authoritative name path — IFF the RecordType's Fields are ordinal-ordered
// (Fields[i].Ordinal == i). NewRecordType does NOT enforce that (it copies fields
// verbatim), so this pins the precondition the migration must honor: P2's positional
// row must be indexed by declared Ordinal (as Java's RecordType maintains), not by
// slice position, or ordinal access reads the wrong field. The mis-ordered case is
// the teeth — without it the assert would be vacuous.
func TestFieldValue_OrdinalOrderingPrecondition_RFC173P1(t *testing.T) {
	t.Parallel()
	// Well-ordered control: declared Ordinal == slice position → round-trip holds.
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

	// Mis-ordered (the teeth): slice positions a@0/b@1 but DECLARED ordinals a=1/b=0.
	// NewRecordType keeps them verbatim, so resolveOrdinal (declared) and GetField
	// (slice position) DIVERGE — the exact failure the migration must prevent.
	misordered := NewRecordType("M", false, []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 1},
		{Name: "b", FieldType: NotNullLong, Ordinal: 0},
	})
	fvA := NewFieldValue(NewQuantifiedObjectValueOfType(NamedCorrelationIdentifier("m"), misordered), "a", NotNullLong)
	ordA, ok := fvA.resolveOrdinal()
	if !ok || ordA != 1 {
		t.Fatalf("mis-ordered: resolveOrdinal(a) = (%d,%v), want its declared Ordinal (1,true)", ordA, ok)
	}
	got, ok := misordered.GetField(ordA)
	if !ok {
		t.Fatalf("mis-ordered: GetField(%d) not found", ordA)
	}
	if got.Name == fvA.Field {
		t.Fatal("expected mis-ordered record to expose ordinal!=slice-position divergence, but round-trip held — the precondition assert would be vacuous")
	}
	if got.Name != "b" {
		t.Fatalf("mis-ordered: GetField(1).Name = %q, want b (slice position 1) — proving ordinal access reads the WRONG field when Fields aren't ordinal-ordered", got.Name)
	}
}
