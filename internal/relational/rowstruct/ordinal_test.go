package rowstruct

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// testOrdinalRow is a positional row with a stated record type and kind, the
// shape executor.PositionalRow hands NewOrdinal.
type testOrdinalRow struct {
	typ   *values.RecordType
	kind  values.OrdinalCarrierKind
	slots []any
}

func (r *testOrdinalRow) Get(i int) (any, bool) {
	if i < 0 || i >= len(r.slots) {
		return nil, false
	}
	return r.slots[i], true
}
func (r *testOrdinalRow) OrdinalRecordType() *values.RecordType     { return r.typ }
func (r *testOrdinalRow) OrdinalRowKind() values.OrdinalCarrierKind { return r.kind }

var testMood = &values.EnumType{EnumName: "MOOD", Nullable: true, Values: []values.EnumValue{
	{Name: "JOYFUL", Number: 0}, {Name: "HAPPY", Number: 1}, {Name: "SAD", Number: 2},
}}

// TestOrdinalStructNamesEnums: an enum in a positional record reaches the
// client by name, as a column's does and as Java's RowStruct over the
// constructed message gives it (MessageTuple.sanitizeField): directly, in an
// array, and in a nested positional record. A number the enum does not declare
// stays a number. This is the arm the WS-J enum spec reaches through exactly
// one read shape (an unnested element of an array of structs); the unit pin
// keeps it covered if that shape is ever routed elsewhere.
func TestOrdinalStructNamesEnums(t *testing.T) {
	t.Parallel()
	inner := &values.RecordType{RecordName: "S", Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: testMood},
		{Name: "N", Ordinal: 1, FieldType: values.NullableLong},
	}}
	outer := &values.RecordType{RecordName: "R", Fields: []values.Field{
		{Name: "M", Ordinal: 0, FieldType: testMood},
		{Name: "MS", Ordinal: 1, FieldType: &values.ArrayType{ElementType: testMood, Nullable: true}},
		{Name: "ST", Ordinal: 2, FieldType: inner},
		{Name: "UNDECLARED", Ordinal: 3, FieldType: testMood},
		{Name: "NONE", Ordinal: 4, FieldType: testMood},
	}}
	row := &testOrdinalRow{typ: outer, kind: values.OrdinalCarrierRecord, slots: []any{
		int64(2),
		[]any{int64(1), int64(0)},
		&testOrdinalRow{typ: inner, kind: values.OrdinalCarrierRecord, slots: []any{int64(0), int64(7)}},
		int64(9),
		nil,
	}}
	s, err := NewOrdinal(row)
	if err != nil {
		t.Fatal(err)
	}
	attr := func(i int) any {
		t.Helper()
		v, err := s.Attribute(i)
		if err != nil {
			t.Fatalf("attribute %d: %v", i, err)
		}
		return v
	}
	if got := attr(1); got != "SAD" {
		t.Errorf("an enum: %v, want SAD", got)
	}
	if got, ok := attr(2).([]any); !ok || len(got) != 2 || got[0] != "HAPPY" || got[1] != "JOYFUL" {
		t.Errorf("an enum array: %v, want [HAPPY JOYFUL]", attr(2))
	}
	nested, ok := attr(3).(*OrdinalStruct)
	if !ok {
		t.Fatalf("a nested record: %T, want *OrdinalStruct", attr(3))
	}
	if got, err := nested.Attribute(1); err != nil || got != "JOYFUL" {
		t.Errorf("an enum in a nested record: %v, %v, want JOYFUL", got, err)
	}
	if got, err := nested.Attribute(2); err != nil || got != int64(7) {
		t.Errorf("a nested long: %v, %v, want 7", got, err)
	}
	if got := attr(4); got != int64(9) {
		t.Errorf("an undeclared number: %v, want 9 unchanged", got)
	}
	if got := attr(5); got != nil {
		t.Errorf("a NULL enum: %v, want nil", got)
	}
}

// TestNewOrdinalRefusals: a row that is not a record, a field whose ordinal is
// not its position, and a row with a slot missing or one too many are
// internal errors, never a struct with NULL attributes.
func TestNewOrdinalRefusals(t *testing.T) {
	t.Parallel()
	typ := &values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NullableLong}}}
	for name, row := range map[string]*testOrdinalRow{
		"a scalar carrier": {typ: typ, kind: values.OrdinalCarrierScalar, slots: []any{int64(1)}},
		"a misplaced ordinal": {
			typ:  &values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 1, FieldType: values.NullableLong}}},
			kind: values.OrdinalCarrierRecord, slots: []any{int64(1)},
		},
		"a missing slot": {typ: typ, kind: values.OrdinalCarrierRecord},
		"an extra slot":  {typ: typ, kind: values.OrdinalCarrierRecord, slots: []any{int64(1), int64(2)}},
	} {
		if _, err := NewOrdinal(row); err == nil {
			t.Errorf("%s: NewOrdinal accepted it", name)
		}
	}
	if _, err := NewOrdinal(nil); err == nil {
		t.Error("a nil row: NewOrdinal accepted it")
	}
}
