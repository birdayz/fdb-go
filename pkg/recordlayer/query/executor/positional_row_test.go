package executor

import (
	"reflect"
	"testing"

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
