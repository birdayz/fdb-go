package values

import (
	"errors"
	"testing"
)

// TestCheckPromotionsTrieDrivesEveryArm drives each arm of the admission
// PromoteValue.computePromotionsTrie makes, the refusals as well as the
// admissions. The SQL-visible verdicts (UPDATE SET against the target) are
// pinned against the JVM by the WS-J enum oracle.
func TestCheckPromotionsTrieDrivesEveryArm(t *testing.T) {
	t.Parallel()
	mood := NewEnumType("MOOD", true, []EnumValue{{Name: "JOYFUL", Number: 0}, {Name: "SAD", Number: 1}})
	moodTwin := NewEnumType("TWIN", false, []EnumValue{{Name: "JOYFUL", Number: 0}, {Name: "SAD", Number: 1}})
	other := NewEnumType("OTHER", true, []EnumValue{{Name: "SAD", Number: 0}, {Name: "JOYFUL", Number: 1}})
	rec := func(types ...Type) *RecordType {
		fields := make([]Field, len(types))
		for i, ty := range types {
			fields[i] = Field{Name: string(rune('A' + i)), FieldType: ty}
		}
		return NewRecordType("", true, fields)
	}
	for _, c := range []struct {
		name            string
		target, current Type
		refused         bool
		field           string
	}{
		{"anything into ANY", AnyType, rec(NullableInt), false, ""},
		{"an unresolved value is not a verdict", NullableInt, UnknownType, false, ""},
		{"into an unresolved slot is not a verdict", UnknownType, NullableString, false, ""},
		{"a primitive of the slot's code", NotNullInt, NullableInt, false, ""},
		{"INT widens to LONG", NullableLong, NotNullInt, false, ""},
		{"LONG does not narrow to INT", NullableInt, NullableLong, true, ""},
		{"DOUBLE does not narrow to LONG", NullableLong, NullableDouble, true, ""},
		{"a number is not a string", NullableString, NullableInt, true, ""},
		{"a string is not a number", NullableLong, NullableString, true, ""},
		{"a string promotes to an enum", mood, NullableString, false, ""},
		{"a string promotes to a UUID", NullableUuid, NullableString, false, ""},
		{"a number is not an enum", mood, NullableInt, true, ""},
		{"a number is not a UUID", NullableUuid, NullableLong, true, ""},
		{"a primitive is not an array", NewArrayType(true, NullableInt), NullableInt, true, ""},
		{"a primitive is not a record", rec(NullableInt), NullableInt, true, ""},
		{"NULL into an enum", mood, NullType, false, ""},
		{"NULL into an array", NewArrayType(true, NullableInt), NullType, false, ""},
		{"NULL into a record", rec(NullableInt), NullType, false, ""},
		{"NULL into a UUID has no operator", NullableUuid, NullType, true, ""},
		{"NULL into a DATE (Go's temporal extension)", NullableDate, NullType, false, ""},
		{"a DATE widens to a TIMESTAMP (Go's temporal extension)", NullableTimestamp, NullableDate, false, ""},
		{"the untyped [] into an array", NewArrayType(true, NullableString), NoneType, false, ""},
		{"the untyped [] is not a scalar", NullableString, NoneType, true, ""},
		{"an enum equal but for its name and nullability", mood, moodTwin, false, ""},
		{"an enum of other values", mood, other, true, ""},
		{"an enum is not a string", NullableString, mood, true, ""},
		{"a UUID into a UUID", NotNullUuid, NullableUuid, false, ""},
		{"an array is not a scalar", NullableInt, NewArrayType(true, NullableInt), true, ""},
		{"arrays element-wise, widening", NewArrayType(true, NullableLong), NewArrayType(false, NullableInt), false, ""},
		{"arrays element-wise, a string element into an enum", NewArrayType(true, mood), NewArrayType(true, NullableString), false, ""},
		{"arrays element-wise, refused", NewArrayType(true, mood), NewArrayType(true, NullableInt), true, ""},
		{"records field by field", rec(NullableLong, mood), rec(NullableInt, NullableString), false, ""},
		{"records of other arity", rec(NullableLong, mood), rec(NullableLong), true, ""},
		{"records name the refused field", rec(NullableLong, rec(mood)), rec(NullableInt, rec(NullableInt)), true, "B.A"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := CheckPromotionsTrie(c.target, c.current)
			if !c.refused {
				if err != nil {
					t.Fatalf("%v into %v refused: %v", c.current, c.target, err)
				}
				return
			}
			var incompatible *IncompatibleTypeError
			if !errors.As(err, &incompatible) {
				t.Fatalf("%v into %v = %v, want IncompatibleTypeError", c.current, c.target, err)
			}
			if incompatible.Field != c.field {
				t.Fatalf("refused field %q, want %q", incompatible.Field, c.field)
			}
		})
	}
}

// TestWithoutPseudoFieldsIsTheTableType pins the table type an INSERT's rows are
// admitted against: the planner layout without its trailing row-version
// pseudo-field, and a layout without one (or whose trailing field only shares
// the name) unchanged.
func TestWithoutPseudoFieldsIsTheTableType(t *testing.T) {
	t.Parallel()
	id := Field{Name: "ID", FieldType: NullableLong}
	version := Field{Name: PseudoFieldRowVersion, FieldType: NullableVersion, Ordinal: 1}
	withVersion := NewRecordType("T", false, []Field{id, version})
	if got := WithoutPseudoFields(withVersion); len(got.Fields) != 1 || got.Fields[0].Name != "ID" || len(withVersion.Fields) != 2 {
		t.Fatalf("WithoutPseudoFields(%v) = %v, want the one declared field and the input untouched", withVersion, got)
	}
	realColumn := NewRecordType("T", false, []Field{id, {Name: PseudoFieldRowVersion, FieldType: NullableLong, Ordinal: 1}})
	if got := WithoutPseudoFields(realColumn); got != realColumn {
		t.Fatalf("a REAL column named %s was stripped: %v", PseudoFieldRowVersion, got)
	}
	plain := NewRecordType("T", false, []Field{id})
	if got := WithoutPseudoFields(plain); got != plain {
		t.Fatalf("a layout without pseudo-fields changed: %v", got)
	}
	if err := CheckPromotionsTrie(WithoutPseudoFields(withVersion), NewRecordType("", false, []Field{{Name: "ID", FieldType: NotNullInt}})); err != nil {
		t.Fatalf("a row of the declared columns is refused against the table type: %v", err)
	}
}
