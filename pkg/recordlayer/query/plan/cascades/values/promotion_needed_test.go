package values

import "testing"

// IsPromotionNeeded is PromoteValue.isPromotionNeeded: nullability never
// needs a promotion; a record needs one when a field's type, name or position
// differs; Java's INCOMPATIBLE_TYPE arms report ok=false.
func TestIsPromotionNeededIsJavas(t *testing.T) {
	t.Parallel()
	long := NewPrimitiveType(TypeCodeLong, false)
	record := func(nullable bool, names ...string) Type {
		fields := make([]Field, len(names))
		for i, n := range names {
			fields[i] = Field{Name: n, FieldType: NullableLong, Ordinal: i}
		}
		return NewRecordType("R", nullable, fields)
	}
	for _, c := range []struct {
		name         string
		from, to     Type
		needed, isOK bool
	}{
		{"to ANY", NullableString, AnyType, false, true},
		{"from NULL", NullType, NullableLong, true, true},
		{"from NONE", NoneType, NewArrayType(true, NullableLong), true, true},
		{"nullability only", long, NullableLong, false, true},
		{"widening", NullableInt, NullableLong, true, true},
		{"array element nullability", NewArrayType(false, long), NewArrayType(true, NullableLong), false, true},
		{"array element widening", NewArrayType(true, NullableInt), NewArrayType(true, NullableLong), true, true},
		{"record nullability only", record(false, "Y", "Z"), record(true, "Y", "Z"), false, true},
		{"record field names", record(false, "_0", "_1"), record(true, "Y", "Z"), true, true},
		{"record arity", record(false, "Y"), record(true, "Y", "Z"), false, false},
		{"vector nullability", NewVectorType(false, 32, 3), NewVectorType(true, 32, 3), false, false},
		{"vector", NewVectorType(true, 32, 3), NewVectorType(true, 32, 3), false, true},
		{"structure against scalar", record(true, "Y"), NullableLong, false, false},
		{"string to long", NullableString, NullableLong, true, true},
	} {
		needed, ok := IsPromotionNeeded(c.from, c.to)
		if needed != c.needed || ok != c.isOK {
			t.Errorf("%s: needed %v ok %v, want %v %v", c.name, needed, ok, c.needed, c.isOK)
		}
	}
}
