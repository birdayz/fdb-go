package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

type driverOrdinalFixture struct {
	typ   *values.RecordType
	slots []any
	kind  values.OrdinalCarrierKind
}

func (r *driverOrdinalFixture) Get(ordinal int) (any, bool) {
	if r == nil || ordinal < 0 || ordinal >= len(r.slots) {
		return nil, false
	}
	return r.slots[ordinal], true
}

func (r *driverOrdinalFixture) OrdinalRecordType() *values.RecordType { return r.typ }

func (r *driverOrdinalFixture) OrdinalRowKind() values.OrdinalCarrierKind { return r.kind }

func TestMaterializeDriverValueWrapsExactOrdinalRecordAsStruct(t *testing.T) {
	t.Parallel()

	childType := values.NewRecordType("test.public.CHILD", true, []values.Field{
		{Name: "NAME", FieldType: values.NullableString, Ordinal: 0},
	})
	rowType := values.NewRecordType("test.public.ELEM", false, []values.Field{
		{Name: "SUB", FieldType: values.NewArrayType(false, values.NotNullInt), Ordinal: 0},
		{Name: "K", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "CHILD", FieldType: childType, Ordinal: 2},
	})
	child := &executor.PositionalRow{Type: childType, Slots: []any{"leaf"}}
	row := &executor.PositionalRow{
		Type:  rowType,
		Slots: []any{[]any{int64(1), int64(7)}, int64(9), child},
	}

	got := materializeDriverValue(row)
	s, ok := got.(api.Struct)
	if !ok {
		t.Fatalf("materialized ordinal record = %T, want api.Struct", got)
	}
	if s.AttributeCount() != 3 || s.MetaData().TypeName() != "ELEM" {
		t.Fatalf("struct metadata = %q/%d, want ELEM/3",
			s.MetaData().TypeName(), s.AttributeCount())
	}
	if name, err := s.MetaData().AttributeName(1); err != nil || name != "SUB" {
		t.Fatalf("attribute 1 name = %q, %v, want SUB", name, err)
	}
	if typ, err := s.MetaData().AttributeDataType(1); err != nil || typ.Code() != api.CodeArray || typ.IsNullable() {
		t.Fatalf("SUB data type = %v, %v, want non-null ARRAY", typ, err)
	}
	if gotSub, err := s.AttributeByName("sub"); err != nil {
		t.Fatalf("SUB: %v", err)
	} else if sub, ok := gotSub.([]any); !ok || len(sub) != 2 || sub[0] != int64(1) || sub[1] != int64(7) {
		t.Fatalf("SUB = %#v, want [1 7]", gotSub)
	}
	gotChild, err := s.Attribute(3)
	if err != nil {
		t.Fatalf("CHILD: %v", err)
	}
	childStruct, ok := gotChild.(api.Struct)
	if !ok {
		t.Fatalf("CHILD = %T, want api.Struct", gotChild)
	}
	if leaf, err := childStruct.AttributeByName("NAME"); err != nil || leaf != "leaf" {
		t.Fatalf("CHILD.NAME = %#v, %v, want leaf", leaf, err)
	}

	// Boundary materialization is a read-only view; the engine's exact ordinal
	// transport remains available to downstream FieldValues and is not rewritten.
	if row.Slots[2] != child || child.Slots[0] != "leaf" {
		t.Fatalf("materialization mutated source rows: outer=%#v child=%#v", row.Slots, child.Slots)
	}

	// Anonymous exact record types use the same public name as the protobuf
	// record-constructor path, never an empty or generated internal type name.
	anonymousType := values.NewRecordType("", false, []values.Field{{
		Name: "VALUE", FieldType: values.NotNullLong, Ordinal: 0,
	}})
	anonymous := &executor.PositionalRow{Type: anonymousType, Slots: []any{int64(42)}}
	anonymousStruct, ok := materializeDriverValue(anonymous).(api.Struct)
	if !ok {
		t.Fatalf("anonymous record = %T, want api.Struct", materializeDriverValue(anonymous))
	}
	if anonymousStruct.MetaData().TypeName() != "RECORD" {
		t.Fatalf("anonymous record type = %q, want RECORD", anonymousStruct.MetaData().TypeName())
	}

	// `_0` is a legal authored field name. Record-vs-scalar is producer
	// provenance, never a shape inference: this genuine anonymous one-field
	// record must remain a public STRUCT even though its row shape is identical
	// to the executor's private scalar envelope.
	oneFieldType := values.NewRecordType("", false, []values.Field{{
		Name: values.OrdinalFieldName(0), FieldType: values.NotNullLong, Ordinal: 0,
	}})
	oneField := &executor.PositionalRow{Type: oneFieldType, Slots: []any{int64(43)}}
	oneFieldStruct, ok := materializeDriverValue(oneField).(api.Struct)
	if !ok {
		t.Fatalf("anonymous one-field record = %T, want api.Struct", materializeDriverValue(oneField))
	}
	if got, err := oneFieldStruct.Attribute(1); err != nil || got != int64(43) {
		t.Fatalf("anonymous one-field record value = %#v, %v, want 43", got, err)
	}
	if oneField.Slots[0] != int64(43) {
		t.Fatalf("one-field record materialization mutated source: %#v", oneField.Slots)
	}
}

func TestMaterializeDriverValueDeclinesMalformedOrScalarOrdinalRows(t *testing.T) {
	t.Parallel()

	recordType := values.NewRecordType("ELEM", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableString, Ordinal: 1},
	})
	wrongWidth := &executor.PositionalRow{Type: recordType, Slots: []any{int64(1)}}
	if got := materializeDriverValue(wrongWidth); got != wrongWidth {
		t.Fatalf("wrong-width row was guessed into %T, want original row", got)
	}

	wrongOrdinalType := &values.RecordType{RecordName: "ELEM", Fields: []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	wrongOrdinal := &executor.PositionalRow{Type: wrongOrdinalType, Slots: []any{int64(1)}}
	if got := materializeDriverValue(wrongOrdinal); got != wrongOrdinal {
		t.Fatalf("wrong-ordinal row was guessed into %T, want original row", got)
	}

	// Use the exact same `_0` RecordType as the admitted one-field record in
	// the positive test. Only explicit producer kind distinguishes this private
	// scalar envelope; field names and width are deliberately not consulted.
	scalarType := values.NewRecordType("", false, []values.Field{{
		Name: values.OrdinalFieldName(0), FieldType: values.NotNullLong, Ordinal: 0,
	}})
	scalar := &driverOrdinalFixture{
		typ: scalarType, slots: []any{int64(42)}, kind: values.OrdinalCarrierScalar,
	}
	if got := materializeDriverValue(scalar); got != scalar {
		t.Fatalf("bare scalar transport was materialized as %T, want original row", got)
	}
}
