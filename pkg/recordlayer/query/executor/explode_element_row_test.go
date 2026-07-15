package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestExplodeElementRow_DeclaredOrder pins that a row-valued explode element
// (map[string]any — a constant record array / VALUES list) is laid out in the
// element type's DECLARED field order, not alphabetical. A baked ordinal reads
// by POSITION, so an alphabetical sort would make a reference baked at a field's
// declared ordinal silently read a DIFFERENT field (the wrong-slot class).
func TestExplodeElementRow_DeclaredOrder(t *testing.T) {
	t.Parallel()
	// RECORD<Z, A> — declared Z before A (NON-alphabetical on purpose).
	elemType := &values.RecordType{Fields: []values.Field{
		{Name: "Z", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "A", FieldType: values.UnknownType, Ordinal: 1},
	}}
	row := explodeElementRow(map[string]any{"Z": int64(100), "A": int64(200)}, elemType)
	if v, _ := row.Get(0); v != int64(100) {
		t.Fatalf("slot 0 = Z (declared first) must be 100, got %v (an alphabetical sort puts A here = 200)", v)
	}
	if v, _ := row.Get(1); v != int64(200) {
		t.Fatalf("slot 1 = A must be 200, got %v", v)
	}
	if row.Type.Fields[0].Name != "Z" || row.Type.Fields[1].Name != "A" {
		t.Fatalf("row type must carry declared order [Z A], got %+v", row.Type.Fields)
	}

	// Case-insensitive element-map keys, and an absent declared field -> NULL slot.
	row2 := explodeElementRow(map[string]any{"z": int64(7)}, elemType)
	if v, _ := row2.Get(0); v != int64(7) {
		t.Fatalf("case-insensitive key: slot 0 (Z) must be 7, got %v", v)
	}
	if v, _ := row2.Get(1); v != nil {
		t.Fatalf("absent declared field A must be a NULL slot, got %v", v)
	}

	// nil element type -> best-effort sorted fallback (unchanged behavior).
	row3 := explodeElementRow(map[string]any{"B": int64(1), "A": int64(2)}, nil)
	if v, _ := row3.Get(0); v != int64(2) {
		t.Fatalf("nil elemType fallback is sorted: slot 0 (A) must be 2, got %v", v)
	}
}
