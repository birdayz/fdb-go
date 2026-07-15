package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPositionalRow_Basics pins the positional row primitives: ordinal Get/Set,
// out-of-range safety, and nil-safety. There is NO name-keyed read on the type
// (RFC-173): a column's slot is bound at plan time via the Type's FieldIndex.
func TestPositionalRow_Basics(t *testing.T) {
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

	// The plan-time bind rule: FieldIndex names each slot; a bake against the
	// type reads the same slot Set wrote.
	for i, f := range typ.Fields {
		idx, ok := typ.FieldIndex(f.Name)
		if !ok || idx != i {
			t.Fatalf("FieldIndex(%q) = (%d,%v), want (%d,true)", f.Name, idx, ok, i)
		}
	}

	// Out-of-range decline.
	if _, ok := row.Get(2); ok {
		t.Error("Get out of range must return false")
	}
	if row.Set(-1, 0) {
		t.Error("Set out of range must return false")
	}

	// Nil-safety.
	var nilRow *PositionalRow
	if _, ok := nilRow.Get(0); ok {
		t.Error("nil row Get must return false")
	}
	// Nil type yields an empty row.
	if r := NewPositionalRow(nil); len(r.Slots) != 0 {
		t.Errorf("NewPositionalRow(nil) slots = %d, want 0", len(r.Slots))
	}
}

// TestPositionalRow_ShadowAssert pins the test oracle (shadowMismatch): a
// positional row that matches the expectation map agrees field-for-field
// (including list values and absent=NULL fields), and a divergent slot is
// caught.
func TestPositionalRow_ShadowAssert(t *testing.T) {
	t.Parallel()
	typ := values.NewRecordType("R", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NullableString, Ordinal: 1},
		{Name: "TAGS", FieldType: values.UnknownType, Ordinal: 2},
	})
	m := map[string]any{"ID": int64(7), "NAME": "alice", "TAGS": []any{"a", "b"}}

	// A row matching the map shadow-agrees on every field (incl. the list value,
	// compared by reflect.DeepEqual).
	row := NewPositionalRow(typ)
	row.Set(0, int64(7))
	row.Set(1, "alice")
	row.Set(2, []any{"a", "b"})
	if bad := shadowMismatch(row, m); bad != "" {
		t.Fatalf("matching row shadow mismatch on field %q", bad)
	}

	// A field the map omits + a nil slot -> agrees (NULL on both sides).
	m2 := map[string]any{"ID": int64(7)}
	row2 := NewPositionalRow(typ)
	row2.Set(0, int64(7))
	if bad := shadowMismatch(row2, m2); bad != "" {
		t.Fatalf("absent-field shadow mismatch on %q (absent must be NULL both sides)", bad)
	}

	// TEETH: a divergent slot is caught by the shadow assert.
	row.Set(1, "MALLORY")
	if bad := shadowMismatch(row, m); bad != "NAME" {
		t.Fatalf("shadow assert should catch divergence at NAME, got %q", bad)
	}
}

// TestProtoToPositional_ShadowsMap pins the scan-row producer: protoToPositional
// (which FromStoredRecord emits for every scanned row) agrees field-for-field
// with the independent protoToMap oracle — set fields carry the value, unset
// fields are NULL — over a real proto message with a mix of set and unset
// fields.
func TestProtoToPositional_ShadowsMap(t *testing.T) {
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

	// The scan's positional row shadow-agrees with the name-keyed oracle on
	// every field.
	if bad := shadowMismatch(row, m); bad != "" {
		t.Fatalf("protoToPositional shadow mismatch on field %q", bad)
	}
	// A set field occupies its schema slot.
	if v, ok := getByName(row, "VAL_STRING"); !ok || v != "alice" {
		t.Fatalf("VAL_STRING = (%v,%v), want (alice,true)", v, ok)
	}
	// An unset field is NULL, present as a nil slot (not absent) — the positional
	// row is dense over the schema, unlike the sparse map.
	if v, ok := getByName(row, "VAL_INT32"); !ok || v != nil {
		t.Fatalf("unset VAL_INT32 = (%v,%v), want (nil,true)", v, ok)
	}
	if _, present := m["VAL_INT32"]; present {
		t.Fatal("protoToMap should omit the unset VAL_INT32 key (sparse map)")
	}
}

// TestPositionalRow_DuplicateNames pins the duplicate-output-name model: a
// projection with duplicate output names (SELECT a, a; a join projecting both
// legs' `id`) keeps BOTH values positionally — positionalTypeFromNames uses a
// raw RecordType (NewRecordType would panic on the duplicate); ordinal access
// is unambiguous, and the plan-time name bind (FieldIndex) is first-match.
func TestPositionalRow_DuplicateNames(t *testing.T) {
	t.Parallel()
	typ := positionalTypeFromNames([]string{"ID", "ID"})
	if len(typ.Fields) != 2 {
		t.Fatalf("dup-name type fields = %d, want 2 (both kept, distinct by ordinal)", len(typ.Fields))
	}
	row := &PositionalRow{Type: typ, Slots: []any{int64(1), int64(2)}}
	// Both values coexist positionally (a name-keyed map would keep only one).
	if v0, _ := row.Get(0); v0 != int64(1) {
		t.Fatalf("Get(0) = %v, want 1", v0)
	}
	if v1, _ := row.Get(1); v1 != int64(2) {
		t.Fatalf("Get(1) = %v, want 2", v1)
	}
	// The plan-time name bind resolves to the FIRST match; the second slot is
	// reachable only by ordinal — which is why duplicate-name-correct reads
	// REQUIRE the plan-time bake (RFC-173's founding case).
	if idx, ok := typ.FieldIndex("ID"); !ok || idx != 0 {
		t.Fatalf("FieldIndex(ID) = (%d,%v), want (0,true) — first match", idx, ok)
	}
}
