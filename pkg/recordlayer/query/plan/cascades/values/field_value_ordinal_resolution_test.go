package values

import (
	"errors"
	"testing"
)

// TestFieldValue_ResolveOrdinal pins resolveOrdinal's contract: a FieldValue
// over a record-typed child does not resolve an ordinal from its name at
// runtime — it DECLINES. The ordinal is bound at PLAN time on a BAKED
// accessor, which resolveOrdinal then returns directly. The declared ordinal
// still round-trips to its field name (the invariant positional evaluation
// rests on), and the unresolvable cases — nil-Child leaf, non-record child,
// absent lazy field — correctly decline.
func TestFieldValue_ResolveOrdinal(t *testing.T) {
	t.Parallel()
	rec := NewRecordType("R", false, []Field{
		{Name: "id", FieldType: NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: NullableString, Ordinal: 1},
	})
	qov := mustQOV(t, NamedCorrelationIdentifier("q"), rec)

	// A LAZY typed-child node does not derive an ordinal by name.
	if _, ok := newFieldValue(qov, "name", NullableString).resolveOrdinal(); ok {
		t.Fatal("lazy typed-child must DECLINE (name-derive not supported)")
	}
	// The declared ordinal for "name" is its slice position (1), and it round-trips
	// to the field name — the invariant positional evaluation rests on.
	ord, ok := rec.FieldIndexUnique("name")
	if !ok || ord != 1 {
		t.Fatalf("FieldIndexUnique(name) = (%d,%v), want (1,true)", ord, ok)
	}
	if got, ok2 := rec.GetField(ord); !ok2 || got.Name != "name" {
		t.Fatalf("round-trip: GetField(%d).Name = %q, want %q", ord, got.Name, "name")
	}
	// A BAKED accessor carries that ordinal — resolveOrdinal returns it directly.
	baked := newCorrelatedFieldValueWithResolvedOrdinal(qov, "name", ord, NullableString)
	if bord, bok := baked.resolveOrdinal(); !bok || bord != 1 {
		t.Fatalf("baked resolveOrdinal(name) = (%d,%v), want (1,true)", bord, bok)
	}

	// nil-Child leaf: unresolvable (no child type to bind against).
	if _, ok := newFlatFieldValue("id", NotNullLong).resolveOrdinal(); ok {
		t.Error("nil-Child leaf must not resolve an ordinal")
	}
	// Non-record child: unresolvable.
	prim := mustQOV(t, NamedCorrelationIdentifier("p"), NotNullLong)
	if _, ok := newFieldValue(prim, "x", NotNullLong).resolveOrdinal(); ok {
		t.Error("non-record child must not resolve an ordinal")
	}
	// Absent field: unresolvable.
	if _, ok := newFieldValue(qov, "missing", UnknownType).resolveOrdinal(); ok {
		t.Error("absent field must not resolve an ordinal")
	}
}

// TestFieldValue_ResolveOrdinal_RawDivergentRecord pins a robustness
// Exact QOV admission rejects a RAW RecordType whose stored field ordinal
// diverges from its slice position. Letting that shape through would give the
// same field two ordinal identities depending on which reader is consulted.
func TestFieldValue_ResolveOrdinal_RawDivergentRecord(t *testing.T) {
	t.Parallel()
	// Raw record (bypasses NewRecordType): field "b" is at slice position 1 but
	// carries a stored Ordinal 0. resolveOrdinal must still return position 1.
	raw := &RecordType{RecordName: "M", Fields: []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 1},
		{Name: "b", FieldType: NotNullLong, Ordinal: 0},
	}}
	// The legacy name resolver still answers by slice position. That does not
	// make the graph exact: its stored ordinal metadata disagrees and admission
	// must reject the contradiction before a QOV exists.
	ord, ok := raw.FieldIndexUnique("b")
	if !ok || ord != 1 {
		t.Fatalf("raw-record FieldIndexUnique(b) = (%d,%v), want (1,true) — the SLICE POSITION, not the stored Ordinal 0", ord, ok)
	}
	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("raw"), raw)
	if qov != nil {
		t.Fatalf("malformed record produced partial QOV %T", qov)
	}
	var resolution *ResolutionError
	if !errors.As(err, &resolution) || resolution.Code() != TypeMalformedOrdinal {
		t.Fatalf("malformed record error = %v, want TypeMalformedOrdinal", err)
	}
}

// TestFieldValue_OrdinalOrderingPrecondition pins the soundness invariant the
// ordinal substrate rests on: ordinal access (resolveOrdinal -> declared
// Field.Ordinal) must agree with slice-position access (GetField) — and
// therefore with the authoritative name path. This holds IFF Fields[i].Ordinal
// == i. NewRecordType ENFORCES that by normalising Ordinal to slice position
// (matching Java's positionally-indexed Record), so the substrate is sound by
// construction. The raw-bypass case is the teeth: it shows the wrong-field
// read that normalisation prevents — without it the invariant would be untested.
func TestFieldValue_OrdinalOrderingPrecondition(t *testing.T) {
	t.Parallel()
	// Well-ordered control: round-trip holds.
	ordered := NewRecordType("R", false, []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 0},
		{Name: "b", FieldType: NotNullLong, Ordinal: 1},
	})
	// A LAZY node declines; the ordinal is bound at plan time (slice position
	// via FieldIndexUnique) and a BAKED accessor returns it.
	ordB, ok := ordered.FieldIndexUnique("b")
	if !ok || ordB != 1 {
		t.Fatalf("ordered: FieldIndexUnique(b) = (%d,%v), want (1,true)", ordB, ok)
	}
	fvB := newCorrelatedFieldValueWithResolvedOrdinal(mustQOV(t, NamedCorrelationIdentifier("q"), ordered), "b", ordB, NotNullLong)
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
	ordA, ok := normalized.FieldIndexUnique("a")
	if !ok || ordA != 0 {
		t.Fatalf("normalised: FieldIndexUnique(a) = (%d,%v), want (0,true)", ordA, ok)
	}
	fvA := newCorrelatedFieldValueWithResolvedOrdinal(mustQOV(t, NamedCorrelationIdentifier("n"), normalized), "a", ordA, NotNullLong)
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
	f, _ := raw.LookupFieldUnique("a") // declared Ordinal 1
	if g, _ := raw.GetField(f.Ordinal); g.Name == "a" {
		t.Fatal("raw mis-ordered record should expose the ordinal!=slice-position divergence the guard prevents")
	}
}
