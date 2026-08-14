package executor

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPositionalRow_Basics pins the positional row primitives: ordinal Get/Set,
// out-of-range safety, and nil-safety. There is NO name-keyed read on the type:
// a column's slot is bound at plan time via the Type's FieldIndexUnique.
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

	// The plan-time bind rule: FieldIndexUnique names each slot; a bake against
	// the type reads the same slot Set wrote.
	for i, f := range typ.Fields {
		idx, ok := typ.FieldIndexUnique(f.Name)
		if !ok || idx != i {
			t.Fatalf("FieldIndexUnique(%q) = (%d,%v), want (%d,true)", f.Name, idx, ok, i)
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

func TestPositionalRowKindSeparatesScalarTransportFromOneFieldRecord(t *testing.T) {
	t.Parallel()

	oneField := values.NewRecordType("", false, []values.Field{{
		Name: values.OrdinalFieldName(0), FieldType: values.NotNullLong, Ordinal: 0,
	}})
	record := &PositionalRow{Type: oneField, Slots: []any{int64(7)}}
	scalar := scalarPositionalRowOfType(int64(7), values.NotNullLong)
	if record.OrdinalRowKind() != values.OrdinalCarrierRecord {
		t.Fatalf("genuine one-field row kind = %v, want record", record.OrdinalRowKind())
	}
	if scalar.OrdinalRowKind() != values.OrdinalCarrierScalar {
		t.Fatalf("purpose-built scalar row kind = %v, want scalar", scalar.OrdinalRowKind())
	}
	if scalar.Type == nil || !scalar.Type.Equals(oneField) {
		t.Fatalf("mutation premise lost: scalar type = %v, want exact same shape as record %v", scalar.Type, oneField)
	}

	// Layout attachment is copy-on-write and must preserve transport provenance.
	layout, err := values.NewScalarOrdinalLayoutForCarrierType(values.NotNullLong)
	if err != nil {
		t.Fatalf("scalar layout: %v", err)
	}
	// Scalar layouts cannot attach to record-shaped transport, but an ordinary
	// struct copy preserves the private kind; pin the copy site used by
	// qualification directly without mutating the source.
	copyRow := *scalar
	copyRow.Slots = append([]any(nil), scalar.Slots...)
	if copyRow.OrdinalRowKind() != values.OrdinalCarrierScalar ||
		scalar.OrdinalRowKind() != values.OrdinalCarrierScalar {
		t.Fatalf("scalar provenance lost across copy: source=%v copy=%v layout=%v",
			scalar.OrdinalRowKind(), copyRow.OrdinalRowKind(), layout.CarrierKind())
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
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 2},
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

// TestProtoToPositional_EmptyRepeatedIsEmptyArray pins the read-side rule that
// makes a NOT NULL array behave: a REPEATED field is read before any presence
// test, so an EMPTY one materializes as the empty array and never as NULL.
//
// An empty repeated field is byte-identical to an absent one on the wire, so
// emptiness cannot be recovered from presence — protoreflect's Has() reports
// false for both. The TYPE settles it: a non-nullable SQL array is stored FLAT
// repeated (Java's layout too) and its type forbids NULL, so absent must read
// back as []. Java does this by branching on isRepeated() before the presence
// test in MessageHelpers.getFieldOnMessage (MessageHelpers.java:125).
//
// Reading such a column as NULL made `IS NULL` true and `= []` UNKNOWN on a
// column that cannot hold NULL. If this regresses, Tags reads (nil, true) and
// the empty-array assertion below is what catches it.
func TestProtoToPositional_EmptyRepeatedIsEmptyArray(t *testing.T) {
	t.Parallel()
	// Tags is a FLAT repeated field: the non-nullable-array storage shape.
	// Never set -> zero elements -> nothing on the wire.
	msg := &gen.Order{OrderId: proto.Int64(1)}
	row := protoToPositional(msg)
	v, ok := getByName(row, "TAGS")
	if !ok {
		t.Fatal("TAGS not in the positional row")
	}
	got, isList := v.([]any)
	if !isList {
		t.Fatalf("empty repeated TAGS = %#v (%T), want the empty array []any{}; "+
			"nil here is the NOT NULL array reading back as SQL NULL", v, v)
	}
	if len(got) != 0 {
		t.Fatalf("empty repeated TAGS = %#v, want length 0", got)
	}
	// A POPULATED repeated field still carries its elements, and a SINGULAR
	// field that is genuinely unset is still NULL — the repeated-first branch
	// must not flatten real absence.
	pop := protoToPositional(&gen.Order{OrderId: proto.Int64(2), Tags: []string{"a", "b"}})
	if v, ok := getByName(pop, "TAGS"); !ok || !reflect.DeepEqual(v, []any{"a", "b"}) {
		t.Fatalf("populated TAGS = (%#v,%v), want ([a b],true)", v, ok)
	}
	if v, ok := getByName(row, "PRICE"); !ok || v != nil {
		t.Fatalf("unset singular PRICE = (%#v,%v), want (nil,true)", v, ok)
	}
	// The name-keyed oracle agrees on every field, empty array included.
	if bad := shadowMismatch(row, protoToMap(msg)); bad != "" {
		t.Fatalf("protoToPositional shadow mismatch on field %q", bad)
	}
}

// TestPositionalRow_DuplicateNames pins the duplicate-output-name model: a
// projection with duplicate output names (SELECT a, a; a join projecting both
// legs' `id`) keeps BOTH values positionally — positionalTypeFromNames uses a
// raw RecordType (NewRecordType would panic on the duplicate); ordinal access
// is unambiguous, and the plan-time name bind DECLINES on the duplicate, so
// ordinal access is the only way to reach either column.
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
	// A name bind cannot resolve here AT ALL, and that is the point: the lookup
	// used to answer the first match, which silently handed slot 0 to a
	// reference that may have meant slot 1. Now it declines, so the only way to
	// read either column is by ordinal — which is what makes
	// duplicate-name-correct reads REQUIRE the plan-time bake rather than merely
	// prefer it.
	if _, ok := typ.FieldIndexUnique("ID"); ok {
		t.Fatal("FieldIndexUnique(ID) resolved on a dup-named type — a name bind must decline here, not answer the first match")
	}
}
