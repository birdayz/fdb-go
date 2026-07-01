package executor

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPositionalRow_RFC173P2 pins the P2 positional row: ordinal Get/Set, the
// name->ordinal bridge (GetByName via FieldIndex) that the shadow assert relies
// on, out-of-range safety, and nil-safety.
func TestPositionalRow_RFC173P2(t *testing.T) {
	t.Parallel()
	typ := values.NewRecordType("R", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: values.NullableString, Ordinal: 1},
	})
	row := NewPositionalRow(typ)
	if len(row.Slots) != 2 {
		t.Fatalf("NewPositionalRow slots = %d, want 2", len(row.Slots))
	}
	// Fresh slots are nil (SQL NULL).
	if v, ok := row.Get(0); !ok || v != nil {
		t.Fatalf("fresh Get(0) = (%v,%v), want (nil,true)", v, ok)
	}

	// Set/Get by ordinal.
	if !row.Set(0, int64(7)) || !row.Set(1, "alice") {
		t.Fatal("Set in range must succeed")
	}
	if v, ok := row.Get(1); !ok || v != "alice" {
		t.Fatalf("Get(1) = (%v,%v), want (alice,true)", v, ok)
	}

	// Name bridge: GetByName resolves via FieldIndex and reads the same slot.
	if v, ok := row.GetByName("id"); !ok || v != int64(7) {
		t.Fatalf("GetByName(id) = (%v,%v), want (7,true)", v, ok)
	}
	if v, ok := row.GetByName("name"); !ok || v != "alice" {
		t.Fatalf("GetByName(name) = (%v,%v), want (alice,true)", v, ok)
	}
	// GetByName agrees with positional access for every field — the property the
	// shadow assert generalizes.
	for i, f := range typ.Fields {
		byOrd, _ := row.Get(i)
		byName, _ := row.GetByName(f.Name)
		if byOrd != byName {
			t.Fatalf("field %q: Get(%d)=%v disagrees with GetByName=%v", f.Name, i, byOrd, byName)
		}
	}

	// Out-of-range and unknown-name decline.
	if _, ok := row.Get(2); ok {
		t.Error("Get out of range must return false")
	}
	if row.Set(-1, 0) {
		t.Error("Set out of range must return false")
	}
	if _, ok := row.GetByName("missing"); ok {
		t.Error("GetByName unknown must return false")
	}

	// Nil-safety.
	var nilRow *PositionalRow
	if _, ok := nilRow.Get(0); ok {
		t.Error("nil row Get must return false")
	}
	if _, ok := nilRow.GetByName("id"); ok {
		t.Error("nil row GetByName must return false")
	}
	// Nil type yields an empty row.
	if r := NewPositionalRow(nil); len(r.Slots) != 0 {
		t.Errorf("NewPositionalRow(nil) slots = %d, want 0", len(r.Slots))
	}
}

// TestPositionalRow_ShadowAssert_RFC173P2 pins the dual-emission bridge
// (positionalRowFromMap) and the shadow assert (shadowMismatch): a row built from
// a name-keyed map agrees with it field-for-field (including list values and
// absent=NULL fields), and a divergent row is caught.
func TestPositionalRow_ShadowAssert_RFC173P2(t *testing.T) {
	t.Parallel()
	typ := values.NewRecordType("R", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NullableString, Ordinal: 1},
		{Name: "TAGS", FieldType: values.UnknownType, Ordinal: 2},
	})
	m := map[string]any{"ID": int64(7), "NAME": "alice", "TAGS": []any{"a", "b"}}

	// Round-trip: the built row shadow-agrees with the source map on every field.
	row := positionalRowFromMap(typ, m)
	if bad := shadowMismatch(row, m); bad != "" {
		t.Fatalf("round-trip shadow mismatch on field %q", bad)
	}
	// List value survives via reflect.DeepEqual (not ==).
	if v, _ := row.Get(2); !reflect.DeepEqual(v, []any{"a", "b"}) {
		t.Fatalf("list slot = %v, want [a b]", v)
	}

	// A field the map omits -> nil slot -> still agrees (NULL on both sides).
	m2 := map[string]any{"ID": int64(7)}
	row2 := positionalRowFromMap(typ, m2)
	if bad := shadowMismatch(row2, m2); bad != "" {
		t.Fatalf("absent-field shadow mismatch on %q (absent must be NULL both sides)", bad)
	}
	if v, ok := row2.Get(1); !ok || v != nil {
		t.Fatalf("absent NAME slot = (%v,%v), want (nil,true)", v, ok)
	}

	// TEETH: a divergent positional row is caught by the shadow assert.
	row.Set(1, "MALLORY")
	if bad := shadowMismatch(row, m); bad != "NAME" {
		t.Fatalf("shadow assert should catch divergence at NAME, got %q", bad)
	}
}

// TestProtoToPositional_ShadowsMap_RFC173P2 pins the first real producer wiring:
// protoToPositional (which FromStoredRecord emits for every scanned row) mirrors
// protoToMap field-for-field — set fields carry the value, unset fields are NULL
// on both sides — over a real proto message with a mix of set and unset fields.
func TestProtoToPositional_ShadowsMap_RFC173P2(t *testing.T) {
	t.Parallel()
	msg := &gen.TypedRecord{
		Id:        proto.Int64(7),
		ValInt64:  proto.Int64(42),
		ValString: proto.String("alice"),
		ValBool:   proto.Bool(true),
		// remaining fields unset -> SQL NULL on both sides
	}
	m := protoToMap(msg)
	row := protoToPositional(msg)

	// The scan's positional row shadow-agrees with its name-keyed map on every field.
	if bad := shadowMismatch(row, m); bad != "" {
		t.Fatalf("protoToPositional shadow mismatch on field %q", bad)
	}
	// A set field resolves positionally (via the name bridge).
	if v, ok := row.GetByName("VAL_STRING"); !ok || v != "alice" {
		t.Fatalf("GetByName(VAL_STRING) = (%v,%v), want (alice,true)", v, ok)
	}
	// An unset field is NULL, present as a nil slot (not absent) — the positional
	// row is dense over the schema, unlike the sparse map.
	if v, ok := row.GetByName("VAL_INT32"); !ok || v != nil {
		t.Fatalf("unset VAL_INT32 = (%v,%v), want (nil,true)", v, ok)
	}
	if _, present := m["VAL_INT32"]; present {
		t.Fatal("protoToMap should omit the unset VAL_INT32 key (sparse map)")
	}
}

// TestPositionalRow_DuplicateNames_RFC173P2 pins the finding that drove the
// projection wiring: a projection with duplicate output names (SELECT a, a; a join
// projecting both legs' `id`) keeps BOTH values positionally, where the name-keyed
// map is last-wins. projectionPositionalType uses a raw RecordType (NewRecordType
// would panic on the duplicate); ordinal access is unambiguous, and the shadow
// assert legitimately DIFFERS from the last-wins map on the duplicate (the §5
// models-must-differ case, not a bug — it's the Slice-4 collision fix).
func TestPositionalRow_DuplicateNames_RFC173P2(t *testing.T) {
	t.Parallel()
	typ := projectionPositionalType([]string{"ID", "ID"})
	if len(typ.Fields) != 2 {
		t.Fatalf("dup-name type fields = %d, want 2 (both kept, distinct by ordinal)", len(typ.Fields))
	}
	row := &PositionalRow{Type: typ, Slots: []any{int64(1), int64(2)}}
	// Both values coexist positionally (the map would keep only the last).
	if v0, _ := row.Get(0); v0 != int64(1) {
		t.Fatalf("Get(0) = %v, want 1", v0)
	}
	if v1, _ := row.Get(1); v1 != int64(2) {
		t.Fatalf("Get(1) = %v, want 2", v1)
	}
	// GetByName resolves to the FIRST match (FieldIndex first-match semantics).
	if v, ok := row.GetByName("ID"); !ok || v != int64(1) {
		t.Fatalf("GetByName(ID) = (%v,%v), want (1,true) — first match", v, ok)
	}
	// Shadow against a last-wins map DIFFERS at ID (map has 2, positional field 0
	// has 1) — the §5 legitimate difference, surfaced not silently lost.
	lastWinsMap := map[string]any{"ID": int64(2)}
	if bad := shadowMismatch(row, lastWinsMap); bad != "ID" {
		t.Fatalf("dup-name shadow should differ at ID (map last-wins vs positional dense), got %q", bad)
	}
}
