package executor

import (
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
