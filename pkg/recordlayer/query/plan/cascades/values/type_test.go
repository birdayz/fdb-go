package values

import "testing"

// TestTypeCode_String pins the rendered name for every code so a
// future enum add (e.g. TypeCodeDate when Phase 4.0 lands DATE
// columns) doesn't accidentally collide with an existing rendering.
func TestTypeCode_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code TypeCode
		want string
	}{
		{TypeCodeUnknown, "UNKNOWN"},
		{TypeCodeNull, "NULL"},
		{TypeCodeBoolean, "BOOLEAN"},
		{TypeCodeInt, "INT"},
		{TypeCodeLong, "LONG"},
		{TypeCodeFloat, "FLOAT"},
		{TypeCodeDouble, "DOUBLE"},
		{TypeCodeString, "STRING"},
		{TypeCodeBytes, "BYTES"},
		{TypeCodeVersion, "VERSION"},
		{TypeCodeEnum, "ENUM"},
		{TypeCodeRecord, "RECORD"},
		{TypeCodeArray, "ARRAY"},
		{TypeCodeRelation, "RELATION"},
		{TypeCodeAny, "ANY"},
		{TypeCodeNone, "NONE"},
		{TypeCodeUuid, "UUID"},
		{TypeCode(999), "UNKNOWN"}, // out-of-range falls back to UNKNOWN
	}
	for _, tc := range cases {
		if got := tc.code.String(); got != tc.want {
			t.Errorf("TypeCode(%d).String(): got %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestTypeCode_IsPrimitive pins the primitive/structured split.
// Java's Type.TypeCode.isPrimitive() drives a lot of fold-time
// decisions (e.g. type promotion only fires between primitive
// pairs); a regression here would silently break those.
func TestTypeCode_IsPrimitive(t *testing.T) {
	t.Parallel()
	primitives := []TypeCode{
		TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
		TypeCodeFloat, TypeCodeDouble,
		TypeCodeString, TypeCodeBytes, TypeCodeVersion, TypeCodeUuid,
	}
	for _, p := range primitives {
		if !p.IsPrimitive() {
			t.Errorf("%v should be primitive", p)
		}
	}
	nonPrimitives := []TypeCode{
		TypeCodeUnknown, TypeCodeNull,
		TypeCodeRecord, TypeCodeArray, TypeCodeRelation,
		TypeCodeAny, TypeCodeNone, TypeCodeEnum,
	}
	for _, np := range nonPrimitives {
		if np.IsPrimitive() {
			t.Errorf("%v should NOT be primitive", np)
		}
	}
}

// TestTypeCode_IsNumeric pins the numeric subset — only the four
// numeric codes (INT, LONG, FLOAT, DOUBLE) qualify. Used by future
// arithmetic-promotion rules to decide whether two operands are
// promotable.
func TestTypeCode_IsNumeric(t *testing.T) {
	t.Parallel()
	for _, c := range []TypeCode{TypeCodeInt, TypeCodeLong, TypeCodeFloat, TypeCodeDouble} {
		if !c.IsNumeric() {
			t.Errorf("%v should be numeric", c)
		}
	}
	for _, c := range []TypeCode{TypeCodeBoolean, TypeCodeString, TypeCodeBytes, TypeCodeUuid, TypeCodeUnknown, TypeCodeNull} {
		if c.IsNumeric() {
			t.Errorf("%v should NOT be numeric", c)
		}
	}
}

// TestPrimitiveType_Shape pins the basic getters + Equals over all
// primitive (code, nullable) combinations so any change to the
// invariants surfaces as a row-level failure (vs a single broad
// "everything's wrong").
func TestPrimitiveType_Shape(t *testing.T) {
	t.Parallel()
	codes := []TypeCode{
		TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
		TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBytes,
	}
	for _, c := range codes {
		for _, nullable := range []bool{true, false} {
			p := NewPrimitiveType(c, nullable)
			if p.Code() != c {
				t.Errorf("%v.Code(): got %v, want %v", p, p.Code(), c)
			}
			if p.IsNullable() != nullable {
				t.Errorf("%v.IsNullable(): got %v, want %v", p, p.IsNullable(), nullable)
			}
			// Equals — same shape.
			same := NewPrimitiveType(c, nullable)
			if !p.Equals(same) {
				t.Errorf("%v.Equals(%v): expected true", p, same)
			}
			// Equals — different nullability.
			diff := NewPrimitiveType(c, !nullable)
			if p.Equals(diff) {
				t.Errorf("%v.Equals(%v): expected false (different nullability)", p, diff)
			}
		}
	}
}

// TestPrimitiveType_String pins the rendering for the most common
// shapes — plan-cache key + EXPLAIN output rely on this being
// stable.
func TestPrimitiveType_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want string
	}{
		{NotNullInt, "INT NOT NULL"},
		{NullableInt, "INT NULL"},
		{NotNullLong, "LONG NOT NULL"},
		{NullableLong, "LONG NULL"},
		{NotNullString, "STRING NOT NULL"},
		{NullableString, "STRING NULL"},
		{NotNullBoolean, "BOOLEAN NOT NULL"},
		{NullableBoolean, "BOOLEAN NULL"},
		{NotNullDouble, "DOUBLE NOT NULL"},
		{NullableDouble, "DOUBLE NULL"},
		{NullType, "NULL NULL"},
		{UnknownType, "UNKNOWN NULL"},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("%v.String(): got %q, want %q", tc.t, got, tc.want)
		}
	}
}

// TestPrimitiveType_Equals_NilHandling pins the contract: comparing
// to nil returns false (not panic). The interface impl needs this
// because nil Type values legitimately occur during incremental
// migration.
func TestPrimitiveType_Equals_NilHandling(t *testing.T) {
	t.Parallel()
	if NotNullInt.Equals(nil) {
		t.Fatal("Equals(nil) should be false")
	}
}

// TestNewPrimitiveType_RejectsStructured pins the assertion that
// structured codes (RECORD / ARRAY / RELATION / ENUM) panic at
// construction — those need their own constructors that the seed
// doesn't have yet, and silently producing a degenerate
// PrimitiveType for them would mask the missing port.
func TestNewPrimitiveType_RejectsStructured(t *testing.T) {
	t.Parallel()
	for _, c := range []TypeCode{TypeCodeRecord, TypeCodeArray, TypeCodeRelation, TypeCodeEnum} {
		c := c
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for code %v", c)
				}
			}()
			_ = NewPrimitiveType(c, false)
		})
	}
}

// TestConstantValue_Type_OverridesTypField pins the contract: the
// presence/absence of Value is the AUTHORITATIVE nullability signal,
// regardless of which singleton the caller stored in Typ. So
// ConstantValue with Typ=NotNullLong + Value=nil is nullable
// (typed-NULL), and ConstantValue with Typ=NullableLong + Value=5
// is NOT NULL (literal carries a value).
//
// Without this override, callers would have to pre-pick the right
// NotNull/Nullable singleton for Typ — error-prone and unnecessary.
func TestConstantValue_Type_OverridesTypField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		typ     Type
		value   any
		wantStr string
	}{
		{"NotNullLong + 5 → NOT NULL", NotNullLong, int64(5), "LONG NOT NULL"},
		{"NotNullLong + nil → NULL", NotNullLong, nil, "LONG NULL"},
		{"NullableLong + 5 → NOT NULL", NullableLong, int64(5), "LONG NOT NULL"},
		{"NullableLong + nil → NULL", NullableLong, nil, "LONG NULL"},
		{"NullableString + \"hi\" → NOT NULL", NullableString, "hi", "STRING NOT NULL"},
		{"NotNullBoolean + nil → NULL", NotNullBoolean, nil, "BOOLEAN NULL"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &ConstantValue{Value: tc.value, Typ: tc.typ}
			if got := c.Type().String(); got != tc.wantStr {
				t.Errorf("got %q, want %q", got, tc.wantStr)
			}
		})
	}
}

// TestLegacyConstants_Aliases pins that the legacy ValueType-named
// constants (TypeInt / TypeBool / TypeString / TypeFloat / TypeUnknown)
// continue to point at the canonical Type singletons after the Track
// G1 retirement. Existing call sites of the form
// `Typ: values.TypeInt` keep working — only the value's Go type
// changes (`Type` instead of the retired `ValueType`).
func TestLegacyConstants_Aliases(t *testing.T) {
	t.Parallel()
	if TypeBool != NullableBoolean {
		t.Errorf("TypeBool should alias NullableBoolean; got %v", TypeBool)
	}
	if TypeInt != NullableLong {
		t.Errorf("TypeInt should alias NullableLong; got %v", TypeInt)
	}
	if TypeFloat != NullableDouble {
		t.Errorf("TypeFloat should alias NullableDouble; got %v", TypeFloat)
	}
	if TypeString != NullableString {
		t.Errorf("TypeString should alias NullableString; got %v", TypeString)
	}
	if TypeUnknown != UnknownType {
		t.Errorf("TypeUnknown should alias UnknownType; got %v", TypeUnknown)
	}
}

// --- RecordType tests ---------------------------------------------

// TestRecordType_Shape pins the basic constructor + getters +
// Equals over a representative shape (anonymous record with
// Field{Name="x", Long, 0}, Field{Name="y", String, 1}).
func TestRecordType_Shape(t *testing.T) {
	t.Parallel()
	fields := []Field{
		{Name: "x", FieldType: NotNullLong, Ordinal: 0},
		{Name: "y", FieldType: NullableString, Ordinal: 1},
	}
	r := NewRecordType("MyRec", false, fields)

	if r.Code() != TypeCodeRecord {
		t.Errorf("Code(): got %v, want RECORD", r.Code())
	}
	if r.IsNullable() {
		t.Errorf("IsNullable(): got true, want false")
	}
	if r.RecordName != "MyRec" {
		t.Errorf("Name: got %q", r.RecordName)
	}
	if len(r.Fields) != 2 {
		t.Fatalf("Fields len: got %d", len(r.Fields))
	}

	// Equals: same shape.
	r2 := NewRecordType("MyRec", false, []Field{
		{Name: "x", FieldType: NotNullLong, Ordinal: 0},
		{Name: "y", FieldType: NullableString, Ordinal: 1},
	})
	if !r.Equals(r2) {
		t.Errorf("Equals: same-shape records should be equal")
	}

	// Not equal: different name.
	rDiffName := NewRecordType("OtherRec", false, fields)
	if r.Equals(rDiffName) {
		t.Errorf("Equals: different name should not be equal")
	}
	// Not equal: different nullability.
	rNullable := NewRecordType("MyRec", true, fields)
	if r.Equals(rNullable) {
		t.Errorf("Equals: different nullability should not be equal")
	}
	// Not equal: different field type.
	rDiffFieldType := NewRecordType("MyRec", false, []Field{
		{Name: "x", FieldType: NotNullInt, Ordinal: 0}, // Int instead of Long
		{Name: "y", FieldType: NullableString, Ordinal: 1},
	})
	if r.Equals(rDiffFieldType) {
		t.Errorf("Equals: different field type should not be equal")
	}
	// Not equal: different field count.
	rShorter := NewRecordType("MyRec", false, []Field{fields[0]})
	if r.Equals(rShorter) {
		t.Errorf("Equals: different field count should not be equal")
	}
}

// TestRecordType_LookupField pins the name-based lookup. Anonymous
// fields (Name="") are NOT addressable — must come through GetField.
func TestRecordType_LookupField(t *testing.T) {
	t.Parallel()
	r := NewRecordType("R", false, []Field{
		{Name: "id", FieldType: NotNullLong, Ordinal: 0},
		{Name: "name", FieldType: NullableString, Ordinal: 1},
		{Name: "", FieldType: NotNullBoolean, Ordinal: 2}, // anonymous
	})
	f, ok := r.LookupField("id")
	if !ok || f.Ordinal != 0 || !f.FieldType.Equals(NotNullLong) {
		t.Errorf("LookupField(id): got %v, %v", f, ok)
	}
	if _, ok := r.LookupField("missing"); ok {
		t.Errorf("LookupField(missing): expected not found")
	}
	if _, ok := r.LookupField(""); ok {
		t.Errorf("LookupField(\"\"): anonymous fields should not be name-addressable")
	}
}

// TestRecordType_GetField pins ordinal-based lookup, including
// out-of-range guards.
func TestRecordType_GetField(t *testing.T) {
	t.Parallel()
	r := NewRecordType("R", false, []Field{
		{Name: "a", FieldType: NotNullLong, Ordinal: 0},
		{Name: "b", FieldType: NullableString, Ordinal: 1},
	})
	f, ok := r.GetField(0)
	if !ok || f.Name != "a" {
		t.Errorf("GetField(0): got %v, %v", f, ok)
	}
	f, ok = r.GetField(1)
	if !ok || f.Name != "b" {
		t.Errorf("GetField(1): got %v, %v", f, ok)
	}
	if _, ok := r.GetField(2); ok {
		t.Errorf("GetField(2): expected out-of-range")
	}
	if _, ok := r.GetField(-1); ok {
		t.Errorf("GetField(-1): expected out-of-range")
	}
}

// TestNewRecordType_RejectsDuplicateNamedFields pins the dup-check.
// Anonymous fields with the same Name="" are NOT a duplicate (the
// disambiguation is by Ordinal).
func TestNewRecordType_RejectsDuplicateNamedFields(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate field name")
		}
	}()
	_ = NewRecordType("R", false, []Field{
		{Name: "x", FieldType: NotNullLong, Ordinal: 0},
		{Name: "x", FieldType: NullableString, Ordinal: 1},
	})
}

// TestNewRecordType_AnonymousFieldsAllowed pins that two Name=""
// fields with distinct Ordinals are valid (`RECORD<INT, STRING>`).
func TestNewRecordType_AnonymousFieldsAllowed(t *testing.T) {
	t.Parallel()
	r := NewRecordType("", true, []Field{
		{Name: "", FieldType: NotNullLong, Ordinal: 0},
		{Name: "", FieldType: NotNullString, Ordinal: 1},
	})
	if len(r.Fields) != 2 {
		t.Fatalf("Fields: got %d", len(r.Fields))
	}
}

// TestRecordType_String pins the rendered form. Different shapes
// produce different strings — this is how plan-cache key
// distinguishes records by shape.
func TestRecordType_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want string
	}{
		{
			NewRecordType("Item", false, []Field{
				{Name: "id", FieldType: NotNullLong, Ordinal: 0},
				{Name: "name", FieldType: NullableString, Ordinal: 1},
			}),
			"Item RECORD<id LONG NOT NULL, name STRING NULL> NOT NULL",
		},
		{
			NewRecordType("", true, []Field{
				{Name: "x", FieldType: NotNullInt, Ordinal: 0},
			}),
			"RECORD<x INT NOT NULL> NULL",
		},
		{
			// Empty record (unit type) — legal.
			NewRecordType("Unit", false, nil),
			"Unit RECORD<> NOT NULL",
		},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("String(): got %q, want %q", got, tc.want)
		}
	}
}

// TestRecordType_DefensiveCopy pins that mutating the input slice
// after construction doesn't affect the constructed RecordType.
func TestRecordType_DefensiveCopy(t *testing.T) {
	t.Parallel()
	fields := []Field{{Name: "x", FieldType: NotNullLong, Ordinal: 0}}
	r := NewRecordType("R", false, fields)
	fields[0].Name = "tampered"
	if r.Fields[0].Name != "x" {
		t.Errorf("Fields not defensively copied: got %q", r.Fields[0].Name)
	}
}

// --- ArrayType tests ----------------------------------------------

// TestArrayType_Shape pins the basic constructor + getters + Equals
// over (nullable, elementType) variations.
func TestArrayType_Shape(t *testing.T) {
	t.Parallel()
	a := NewArrayType(false, NotNullLong)
	if a.Code() != TypeCodeArray {
		t.Errorf("Code(): got %v, want ARRAY", a.Code())
	}
	if a.IsNullable() {
		t.Errorf("IsNullable(): got true, want false")
	}
	if !a.ElementType.Equals(NotNullLong) {
		t.Errorf("ElementType: got %v, want LONG NOT NULL", a.ElementType)
	}
	// Equals: same shape.
	if !a.Equals(NewArrayType(false, NotNullLong)) {
		t.Errorf("Equals: identical shape should be equal")
	}
	// Not equal: different nullability.
	if a.Equals(NewArrayType(true, NotNullLong)) {
		t.Errorf("Equals: different nullability should differ")
	}
	// Not equal: different element type.
	if a.Equals(NewArrayType(false, NullableString)) {
		t.Errorf("Equals: different element type should differ")
	}
	// Not equal: different element-type shape.
	if a.Equals(NewArrayType(false, NullableLong)) {
		t.Errorf("Equals: different element nullability should differ")
	}
}

// TestArrayType_NilElementType pins the "type not yet inferred"
// path. nil ElementType is legal; two ArrayTypes both with nil
// ElementType are equal; one nil + one non-nil are not.
func TestArrayType_NilElementType(t *testing.T) {
	t.Parallel()
	a := NewArrayType(true, nil)
	if a.ElementType != nil {
		t.Errorf("ElementType: got %v, want nil", a.ElementType)
	}
	if !a.Equals(NewArrayType(true, nil)) {
		t.Errorf("Equals: two nil-element arrays should be equal")
	}
	if a.Equals(NewArrayType(true, NotNullLong)) {
		t.Errorf("Equals: nil-element vs typed-element should not be equal")
	}
	if NewArrayType(true, NotNullLong).Equals(a) {
		t.Errorf("Equals: typed-element vs nil-element should not be equal (symmetric)")
	}
}

// TestArrayType_String pins the rendered form.
func TestArrayType_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want string
	}{
		{NewArrayType(false, NotNullLong), "ARRAY<LONG NOT NULL> NOT NULL"},
		{NewArrayType(true, NullableString), "ARRAY<STRING NULL> NULL"},
		{NewArrayType(true, nil), "ARRAY<?> NULL"},
		// Nested record-of-array.
		{
			NewArrayType(false, NewRecordType("Item", false, []Field{
				{Name: "id", FieldType: NotNullLong, Ordinal: 0},
			})),
			"ARRAY<Item RECORD<id LONG NOT NULL> NOT NULL> NOT NULL",
		},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("String(): got %q, want %q", got, tc.want)
		}
	}
}

// TestArrayType_Nested pins composition: ARRAY<ARRAY<INT>> equals
// itself; ARRAY<ARRAY<INT>> ≠ ARRAY<INT>.
func TestArrayType_Nested(t *testing.T) {
	t.Parallel()
	innerA := NewArrayType(false, NotNullInt)
	innerB := NewArrayType(false, NotNullInt)
	outerA := NewArrayType(false, innerA)
	outerB := NewArrayType(false, innerB)
	if !outerA.Equals(outerB) {
		t.Errorf("ARRAY<ARRAY<INT>>: shape-equal nested arrays should be equal")
	}
	if outerA.Equals(NewArrayType(false, NotNullInt)) {
		t.Errorf("ARRAY<ARRAY<INT>> ≠ ARRAY<INT>")
	}
}

// --- EnumType tests -----------------------------------------------

// TestEnumType_Shape pins constructor + getters + Equals.
func TestEnumType_Shape(t *testing.T) {
	t.Parallel()
	e := NewEnumType("Suit", false, []EnumValue{
		{Name: "SPADES", Number: 0},
		{Name: "HEARTS", Number: 1},
		{Name: "DIAMONDS", Number: 2},
		{Name: "CLUBS", Number: 3},
	})
	if e.Code() != TypeCodeEnum {
		t.Errorf("Code(): got %v", e.Code())
	}
	if e.IsNullable() {
		t.Errorf("IsNullable(): got true")
	}
	if e.EnumName != "Suit" {
		t.Errorf("Name: got %q", e.EnumName)
	}
	if len(e.Values) != 4 {
		t.Fatalf("Values len: got %d", len(e.Values))
	}
	// Equals: same shape.
	e2 := NewEnumType("Suit", false, []EnumValue{
		{Name: "SPADES", Number: 0},
		{Name: "HEARTS", Number: 1},
		{Name: "DIAMONDS", Number: 2},
		{Name: "CLUBS", Number: 3},
	})
	if !e.Equals(e2) {
		t.Errorf("Equals: identical enums should be equal")
	}
	// Not equal: different name.
	if e.Equals(NewEnumType("Other", false, e.Values)) {
		t.Errorf("Equals: different name should differ")
	}
	// Not equal: different ordering of values.
	eReorder := NewEnumType("Suit", false, []EnumValue{
		{Name: "HEARTS", Number: 1},
		{Name: "SPADES", Number: 0},
		{Name: "DIAMONDS", Number: 2},
		{Name: "CLUBS", Number: 3},
	})
	if e.Equals(eReorder) {
		t.Errorf("Equals: reordered values should differ (declared order matters)")
	}
}

// TestEnumType_Lookup pins LookupValueByName / LookupValueByNumber.
func TestEnumType_Lookup(t *testing.T) {
	t.Parallel()
	e := NewEnumType("Suit", false, []EnumValue{
		{Name: "SPADES", Number: 0},
		{Name: "HEARTS", Number: 1},
	})
	v, ok := e.LookupValueByName("HEARTS")
	if !ok || v.Number != 1 {
		t.Errorf("LookupValueByName(HEARTS): got %v, %v", v, ok)
	}
	if _, ok := e.LookupValueByName(""); ok {
		t.Errorf("LookupValueByName(\"\") should not match")
	}
	if _, ok := e.LookupValueByName("MISSING"); ok {
		t.Errorf("LookupValueByName(MISSING) should not match")
	}
	v, ok = e.LookupValueByNumber(0)
	if !ok || v.Name != "SPADES" {
		t.Errorf("LookupValueByNumber(0): got %v, %v", v, ok)
	}
	if _, ok := e.LookupValueByNumber(99); ok {
		t.Errorf("LookupValueByNumber(99) should not match")
	}
}

// TestNewEnumType_RejectsDuplicates pins both name + number dup
// detection. Either duplicate panics.
func TestNewEnumType_RejectsDuplicates(t *testing.T) {
	t.Parallel()
	t.Run("duplicate-name", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate name")
			}
		}()
		_ = NewEnumType("Bad", false, []EnumValue{
			{Name: "X", Number: 0},
			{Name: "X", Number: 1},
		})
	})
	t.Run("duplicate-number", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate number")
			}
		}()
		_ = NewEnumType("Bad", false, []EnumValue{
			{Name: "X", Number: 0},
			{Name: "Y", Number: 0},
		})
	})
}

// TestEnumType_String pins the rendered form.
func TestEnumType_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want string
	}{
		{
			NewEnumType("Suit", false, []EnumValue{
				{Name: "S", Number: 0}, {Name: "H", Number: 1},
			}),
			"Suit ENUM<S=0, H=1> NOT NULL",
		},
		{
			NewEnumType("", true, []EnumValue{{Name: "A", Number: 42}}),
			"ENUM<A=42> NULL",
		},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("String(): got %q, want %q", got, tc.want)
		}
	}
}

// --- WithNullability tests ----------------------------------------

// TestWithNullability_Primitive pins that flipping nullability on a
// canonical singleton returns the OTHER canonical singleton (so
// pointer-equality stays useful for fast checks). Re-flipping
// returns the original singleton.
func TestWithNullability_Primitive(t *testing.T) {
	t.Parallel()
	if WithNullability(NotNullInt, true) != NullableInt {
		t.Errorf("NotNullInt → nullable should return NullableInt singleton")
	}
	if WithNullability(NullableInt, false) != NotNullInt {
		t.Errorf("NullableInt → not-nullable should return NotNullInt singleton")
	}
	// Same nullability returns input unchanged (pointer-equal).
	if WithNullability(NotNullInt, false) != NotNullInt {
		t.Errorf("NotNullInt → not-nullable (no-op) should return same instance")
	}
	// All canonical singletons round-trip.
	for _, sing := range []Type{
		NotNullBoolean, NullableBoolean,
		NotNullString, NullableString,
		NotNullLong, NullableLong,
		NotNullDouble, NullableDouble,
		NotNullBytes, NullableBytes,
		NotNullUuid, NullableUuid,
		NotNullVersion, NullableVersion,
	} {
		flipped := WithNullability(sing, !sing.IsNullable())
		if flipped.IsNullable() == sing.IsNullable() {
			t.Errorf("WithNullability didn't flip for %v", sing)
		}
		if !WithNullability(flipped, sing.IsNullable()).Equals(sing) {
			t.Errorf("Round-trip lost shape for %v", sing)
		}
	}
}

// TestUuidVersionSingletons pins that NullableUuid / NotNullUuid /
// NullableVersion / NotNullVersion are real canonical singletons:
// WithNullability returns THE singleton (pointer-equal), not a fresh
// PrimitiveType. Without these arms in the WithNullability switch the
// primitive types fell through to a fresh allocation, breaking
// pointer-equality fast paths in callers.
func TestUuidVersionSingletons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		notNull, nullable Type
	}{
		{"UUID", NotNullUuid, NullableUuid},
		{"VERSION", NotNullVersion, NullableVersion},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if WithNullability(tc.notNull, true) != tc.nullable {
				t.Errorf("%s NOT NULL → nullable did not return canonical singleton", tc.name)
			}
			if WithNullability(tc.nullable, false) != tc.notNull {
				t.Errorf("%s NULLABLE → not-null did not return canonical singleton", tc.name)
			}
			// No-op (same nullability) returns same instance.
			if WithNullability(tc.notNull, false) != tc.notNull {
				t.Errorf("%s NOT NULL → not-null (no-op) returned a different instance", tc.name)
			}
		})
	}
}

// TestWithNullability_Structured pins behavior for RecordType,
// ArrayType, EnumType — flipping returns a NEW instance with the
// same payload but flipped Nullable.
func TestWithNullability_Structured(t *testing.T) {
	t.Parallel()
	r := NewRecordType("R", false, []Field{{Name: "x", FieldType: NotNullLong, Ordinal: 0}})
	rNullable := WithNullability(r, true)
	if !rNullable.IsNullable() {
		t.Errorf("Record: expected Nullable=true after flip")
	}
	rNullableR, ok := rNullable.(*RecordType)
	if !ok {
		t.Fatalf("Record: got %T, want *RecordType", rNullable)
	}
	if rNullableR.RecordName != r.RecordName || len(rNullableR.Fields) != len(r.Fields) {
		t.Errorf("Record: shape changed")
	}

	a := NewArrayType(false, NotNullLong)
	aNullable := WithNullability(a, true)
	if !aNullable.IsNullable() {
		t.Errorf("Array: expected Nullable=true")
	}
	if !aNullable.(*ArrayType).ElementType.Equals(NotNullLong) {
		t.Errorf("Array: ElementType changed")
	}

	e := NewEnumType("E", false, []EnumValue{{Name: "X", Number: 0}})
	eNullable := WithNullability(e, true)
	if !eNullable.IsNullable() {
		t.Errorf("Enum: expected Nullable=true")
	}
	if eNullable.(*EnumType).EnumName != "E" {
		t.Errorf("Enum: name changed")
	}
}

// TestShapePredicates pins the IsNull / IsNone / IsAny / IsUnresolved
// / IsArray / IsRecord / IsEnum / IsUuid / IsRelation free functions.
// All safely return false for nil. Each matches the corresponding
// TypeCode shape.
func TestShapePredicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		t       Type
		isNull  bool
		isNone  bool
		isAny   bool
		isUnres bool
		isArr   bool
		isRec   bool
		isEnum  bool
		isUuid  bool
		isRel   bool
	}{
		{name: "nil", t: nil, isUnres: true},
		{name: "INT", t: NotNullInt},
		{name: "NULL", t: NullType, isNull: true, isUnres: true},
		{name: "NONE", t: NoneType, isNone: true, isUnres: true},
		{name: "ANY", t: AnyType, isAny: true, isUnres: true},
		{name: "UNKNOWN", t: UnknownType, isUnres: true},
		{name: "ARRAY<INT>", t: NewArrayType(false, NotNullInt), isArr: true},
		{name: "RECORD", t: &RecordType{}, isRec: true},
		{name: "ENUM", t: NewEnumType("E", false, []EnumValue{{Name: "A", Number: 0}}), isEnum: true},
		{name: "UUID", t: NotNullUuid, isUuid: true},
		{name: "RELATION", t: NewRelationType(NotNullLong), isRel: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if IsNull(tc.t) != tc.isNull {
				t.Errorf("IsNull: got %v, want %v", IsNull(tc.t), tc.isNull)
			}
			if IsNone(tc.t) != tc.isNone {
				t.Errorf("IsNone: got %v, want %v", IsNone(tc.t), tc.isNone)
			}
			if IsAny(tc.t) != tc.isAny {
				t.Errorf("IsAny: got %v, want %v", IsAny(tc.t), tc.isAny)
			}
			if IsUnresolved(tc.t) != tc.isUnres {
				t.Errorf("IsUnresolved: got %v, want %v", IsUnresolved(tc.t), tc.isUnres)
			}
			if IsArray(tc.t) != tc.isArr {
				t.Errorf("IsArray: got %v, want %v", IsArray(tc.t), tc.isArr)
			}
			if IsRecord(tc.t) != tc.isRec {
				t.Errorf("IsRecord: got %v, want %v", IsRecord(tc.t), tc.isRec)
			}
			if IsEnum(tc.t) != tc.isEnum {
				t.Errorf("IsEnum: got %v, want %v", IsEnum(tc.t), tc.isEnum)
			}
			if IsUuid(tc.t) != tc.isUuid {
				t.Errorf("IsUuid: got %v, want %v", IsUuid(tc.t), tc.isUuid)
			}
			if IsRelation(tc.t) != tc.isRel {
				t.Errorf("IsRelation: got %v, want %v", IsRelation(tc.t), tc.isRel)
			}
		})
	}
}

// TestIsPromotable pins the type-promotion lattice — Java's
// PromoteValue.PROMOTION_MAP shape ported to Go. Each row is a
// (from-code, to-code, expected) triple. Identity is always
// promotable; the rest must match Java's hardcoded set exactly.
func TestIsPromotable(t *testing.T) {
	t.Parallel()

	primAt := func(c TypeCode) Type { return &PrimitiveType{TypeCode: c, Nullable: false} }

	cases := []struct {
		from TypeCode
		to   TypeCode
		want bool
	}{
		// Identity — every code can hold its own values.
		{TypeCodeInt, TypeCodeInt, true},
		{TypeCodeLong, TypeCodeLong, true},
		{TypeCodeString, TypeCodeString, true},

		// Numeric widening.
		{TypeCodeInt, TypeCodeLong, true},
		{TypeCodeInt, TypeCodeFloat, true},
		{TypeCodeInt, TypeCodeDouble, true},
		{TypeCodeLong, TypeCodeFloat, true},
		{TypeCodeLong, TypeCodeDouble, true},
		{TypeCodeFloat, TypeCodeDouble, true},

		// Numeric narrowing — NOT promotable (would lose precision).
		{TypeCodeLong, TypeCodeInt, false},
		{TypeCodeFloat, TypeCodeInt, false},
		{TypeCodeFloat, TypeCodeLong, false},
		{TypeCodeDouble, TypeCodeFloat, false},

		// NULL → any.
		{TypeCodeNull, TypeCodeInt, true},
		{TypeCodeNull, TypeCodeString, true},
		{TypeCodeNull, TypeCodeBoolean, true},
		{TypeCodeNull, TypeCodeArray, true},

		// STRING → ENUM / UUID (lookup by name / parse).
		{TypeCodeString, TypeCodeEnum, true},
		{TypeCodeString, TypeCodeUuid, true},
		// Reverse direction NOT allowed — explicit CAST required.
		{TypeCodeEnum, TypeCodeString, false},
		{TypeCodeUuid, TypeCodeString, false},

		// Cross-category not allowed.
		{TypeCodeBoolean, TypeCodeInt, false},
		{TypeCodeString, TypeCodeInt, false},
		{TypeCodeInt, TypeCodeBoolean, false},
		{TypeCodeBytes, TypeCodeString, false},
	}
	for _, tc := range cases {
		got := IsPromotable(primAt(tc.from), primAt(tc.to))
		if got != tc.want {
			t.Errorf("IsPromotable(%v, %v): got %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestIsPromotable_NilHandling pins the contract: nil from/to
// returns false (not panic). Promotion never makes sense across a
// nil type.
func TestIsPromotable_NilHandling(t *testing.T) {
	t.Parallel()
	if IsPromotable(nil, NotNullInt) {
		t.Error("IsPromotable(nil, ...) should be false")
	}
	if IsPromotable(NotNullInt, nil) {
		t.Error("IsPromotable(..., nil) should be false")
	}
	if IsPromotable(nil, nil) {
		t.Error("IsPromotable(nil, nil) should be false")
	}
}

// TestMaximumType pins the binary "common supertype" function.
// Mirrors Java's Type.maximumType for primitives.
func TestMaximumType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		t1   Type
		t2   Type
		want Type
	}{
		// Identity — same code, same nullability.
		{"INT NOT NULL × INT NOT NULL", NotNullInt, NotNullInt, NotNullInt},
		{"INT NULL × INT NULL", NullableInt, NullableInt, NullableInt},
		// Identity — same code, mixed nullability → nullable wins.
		{"INT NOT NULL × INT NULL", NotNullInt, NullableInt, NullableInt},
		{"LONG NOT NULL × LONG NULL", NotNullLong, NullableLong, NullableLong},

		// Numeric widening.
		{"INT × LONG → LONG", NotNullInt, NotNullLong, NotNullLong},
		{"INT × FLOAT → FLOAT", NotNullInt, NotNullFloat, NotNullFloat},
		{"INT × DOUBLE → DOUBLE", NotNullInt, NotNullDouble, NotNullDouble},
		{"LONG × FLOAT → FLOAT", NotNullLong, NotNullFloat, NotNullFloat},
		{"LONG × DOUBLE → DOUBLE", NotNullLong, NotNullDouble, NotNullDouble},
		{"FLOAT × DOUBLE → DOUBLE", NotNullFloat, NotNullDouble, NotNullDouble},
		// Nullability propagates through promotion.
		{"INT NULL × LONG NOT NULL → LONG NULL", NullableInt, NotNullLong, NullableLong},
		{"INT NOT NULL × LONG NULL → LONG NULL", NotNullInt, NullableLong, NullableLong},

		// Symmetric.
		{"LONG × INT → LONG", NotNullLong, NotNullInt, NotNullLong},
		{"DOUBLE × FLOAT → DOUBLE", NotNullDouble, NotNullFloat, NotNullDouble},

		// NULL × T.
		{"NULL × INT → INT NULL", NullType, NotNullInt, NullableInt},
		{"INT × NULL → INT NULL", NotNullInt, NullType, NullableInt},
		{"NULL × NULL → NULL", NullType, NullType, NullType},

		// Cross-category — no common supertype.
		{"INT × STRING → nil", NotNullInt, NotNullString, nil},
		{"BOOLEAN × INT → nil", NotNullBoolean, NotNullInt, nil},
		{"STRING × INT → nil", NotNullString, NotNullInt, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MaximumType(tc.t1, tc.t2)
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Errorf("got nil, want %v", tc.want)
				return
			}
			if !got.Equals(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMaximumTypeOfMany pins variadic fold semantics: identity for
// length 1, transitive promotion across many, nil on empty input
// or any incompatible pair.
func TestMaximumTypeOfMany(t *testing.T) {
	t.Parallel()
	// Empty.
	if MaximumTypeOfMany() != nil {
		t.Error("empty input should return nil")
	}
	// Length 1 — identity.
	if MaximumTypeOfMany(NotNullInt) != NotNullInt {
		t.Error("length-1 should return the input")
	}
	// All same.
	got := MaximumTypeOfMany(NotNullInt, NotNullInt, NotNullInt)
	if !got.Equals(NotNullInt) {
		t.Errorf("all INT → INT, got %v", got)
	}
	// Transitive widening: INT, LONG, DOUBLE → DOUBLE.
	got = MaximumTypeOfMany(NotNullInt, NotNullLong, NotNullDouble)
	if !got.Equals(NotNullDouble) {
		t.Errorf("INT, LONG, DOUBLE → DOUBLE, got %v", got)
	}
	// Order-independent: DOUBLE first should still settle to DOUBLE.
	got = MaximumTypeOfMany(NotNullDouble, NotNullInt, NotNullLong)
	if !got.Equals(NotNullDouble) {
		t.Errorf("DOUBLE, INT, LONG → DOUBLE, got %v", got)
	}
	// Nullability propagates through any nullable input.
	got = MaximumTypeOfMany(NotNullInt, NullableLong)
	if !got.Equals(NullableLong) {
		t.Errorf("nullability should propagate, got %v", got)
	}
	// Incompatible pair anywhere → nil.
	if MaximumTypeOfMany(NotNullInt, NotNullString) != nil {
		t.Error("incompatible pair should return nil")
	}
	if MaximumTypeOfMany(NotNullInt, NotNullLong, NotNullString) != nil {
		t.Error("incompatible pair late in fold should return nil")
	}
}

// TestMaximumType_ArrayRecursion pins ARRAY × ARRAY → ARRAY where
// the element type is the recursive max. Mirrors Java's array case.
func TestMaximumType_ArrayRecursion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		t1   Type
		t2   Type
		want Type
	}{
		{
			"ARRAY<INT> × ARRAY<INT> → ARRAY<INT>",
			NewArrayType(false, NotNullInt),
			NewArrayType(false, NotNullInt),
			NewArrayType(false, NotNullInt),
		},
		{
			"ARRAY<INT> × ARRAY<LONG> → ARRAY<LONG>",
			NewArrayType(false, NotNullInt),
			NewArrayType(false, NotNullLong),
			NewArrayType(false, NotNullLong),
		},
		{
			"ARRAY<INT> NULL × ARRAY<LONG> NOT NULL → ARRAY<LONG> NULL",
			NewArrayType(true, NotNullInt),
			NewArrayType(false, NotNullLong),
			NewArrayType(true, NotNullLong),
		},
		{
			"ARRAY<INT> × ARRAY<STRING> → nil (incompatible elements)",
			NewArrayType(false, NotNullInt),
			NewArrayType(false, NotNullString),
			nil,
		},
		{
			"ARRAY<?> × ARRAY<INT> → nil (erased blocks)",
			NewArrayType(false, nil),
			NewArrayType(false, NotNullInt),
			nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MaximumType(tc.t1, tc.t2)
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Errorf("got nil, want %v", tc.want)
				return
			}
			if !got.Equals(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMaximumType_RecordRecursion pins RECORD × RECORD → RECORD
// where each field's type is the recursive max. Mirrors Java's
// record case in Type.maximumType.
func TestMaximumType_RecordRecursion(t *testing.T) {
	t.Parallel()
	mk := func(nullable bool, fields ...Field) *RecordType {
		return &RecordType{Nullable: nullable, Fields: fields}
	}
	f := func(name string, ft Type, ord int) Field {
		return Field{Name: name, FieldType: ft, Ordinal: ord}
	}

	t.Run("identical fields", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0), f("y", NotNullString, 1))
		r2 := mk(false, f("x", NotNullInt, 0), f("y", NotNullString, 1))
		got := MaximumType(r1, r2).(*RecordType)
		if !got.Equals(r1) {
			t.Errorf("got %v, want %v", got, r1)
		}
	})
	t.Run("widening fields", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0))
		r2 := mk(false, f("x", NotNullLong, 0))
		got := MaximumType(r1, r2).(*RecordType)
		if !got.Fields[0].FieldType.Equals(NotNullLong) {
			t.Errorf("field type: got %v, want NotNullLong", got.Fields[0].FieldType)
		}
	})
	t.Run("nullability propagation", func(t *testing.T) {
		t.Parallel()
		r1 := mk(true, f("x", NotNullInt, 0))
		r2 := mk(false, f("x", NotNullInt, 0))
		got := MaximumType(r1, r2).(*RecordType)
		if !got.Nullable {
			t.Error("nullability should propagate")
		}
	})
	t.Run("field count mismatch", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0))
		r2 := mk(false, f("x", NotNullInt, 0), f("y", NotNullInt, 1))
		if MaximumType(r1, r2) != nil {
			t.Error("field count mismatch should return nil")
		}
	})
	t.Run("incompatible field types", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0))
		r2 := mk(false, f("x", NotNullString, 0))
		if MaximumType(r1, r2) != nil {
			t.Error("incompatible field type should return nil")
		}
	})
	t.Run("name resolution: t1 anonymous → use t2", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("", NotNullInt, 0))
		r2 := mk(false, f("x", NotNullInt, 0))
		got := MaximumType(r1, r2).(*RecordType)
		if got.Fields[0].Name != "x" {
			t.Errorf("field name: got %q, want %q", got.Fields[0].Name, "x")
		}
	})
	t.Run("name resolution: t2 anonymous → use t1", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0))
		r2 := mk(false, f("", NotNullInt, 0))
		got := MaximumType(r1, r2).(*RecordType)
		if got.Fields[0].Name != "x" {
			t.Errorf("field name: got %q, want %q", got.Fields[0].Name, "x")
		}
	})
	t.Run("name resolution: different names → anonymise", func(t *testing.T) {
		t.Parallel()
		r1 := mk(false, f("x", NotNullInt, 0))
		r2 := mk(false, f("y", NotNullInt, 0))
		got := MaximumType(r1, r2).(*RecordType)
		if got.Fields[0].Name != "" {
			t.Errorf("field name: got %q, want empty (anonymised)", got.Fields[0].Name)
		}
	})

	t.Run("record name: both agree → keep", func(t *testing.T) {
		t.Parallel()
		r1 := &RecordType{RecordName: "User", Fields: []Field{f("x", NotNullInt, 0)}}
		r2 := &RecordType{RecordName: "User", Fields: []Field{f("x", NotNullInt, 0)}}
		got := MaximumType(r1, r2).(*RecordType)
		if got.RecordName != "User" {
			t.Errorf("record name: got %q, want User", got.RecordName)
		}
	})
	t.Run("record name: r1 anonymous → use r2", func(t *testing.T) {
		t.Parallel()
		r1 := &RecordType{RecordName: "", Fields: []Field{f("x", NotNullInt, 0)}}
		r2 := &RecordType{RecordName: "User", Fields: []Field{f("x", NotNullInt, 0)}}
		got := MaximumType(r1, r2).(*RecordType)
		if got.RecordName != "User" {
			t.Errorf("record name: got %q, want User", got.RecordName)
		}
	})
	t.Run("record name: different names → anonymise", func(t *testing.T) {
		t.Parallel()
		r1 := &RecordType{RecordName: "User", Fields: []Field{f("x", NotNullInt, 0)}}
		r2 := &RecordType{RecordName: "Order", Fields: []Field{f("x", NotNullInt, 0)}}
		got := MaximumType(r1, r2).(*RecordType)
		if got.RecordName != "" {
			t.Errorf("record name: got %q, want empty", got.RecordName)
		}
	})
}

// TestMaximumType_EnumRecursion pins ENUM × ENUM:
// - same value list → single ENUM with adjusted nullability.
// - different value list → nil.
// Mirrors Java's enum case in Type.maximumType.
func TestMaximumType_EnumRecursion(t *testing.T) {
	t.Parallel()
	colours := []EnumValue{{Name: "RED", Number: 0}, {Name: "GREEN", Number: 1}}
	moods := []EnumValue{{Name: "HAPPY", Number: 0}, {Name: "SAD", Number: 1}}

	t.Run("identical → equal", func(t *testing.T) {
		t.Parallel()
		e1 := NewEnumType("Color", false, colours)
		e2 := NewEnumType("Color", false, colours)
		got := MaximumType(e1, e2).(*EnumType)
		if !got.Equals(e1) {
			t.Errorf("got %v, want %v", got, e1)
		}
	})
	t.Run("nullability propagation", func(t *testing.T) {
		t.Parallel()
		e1 := NewEnumType("Color", true, colours)
		e2 := NewEnumType("Color", false, colours)
		got := MaximumType(e1, e2).(*EnumType)
		if !got.Nullable {
			t.Error("nullability should propagate")
		}
	})
	t.Run("different values → nil", func(t *testing.T) {
		t.Parallel()
		e1 := NewEnumType("X", false, colours)
		e2 := NewEnumType("X", false, moods)
		if MaximumType(e1, e2) != nil {
			t.Error("different values should return nil")
		}
	})
	t.Run("different value count → nil", func(t *testing.T) {
		t.Parallel()
		e1 := NewEnumType("X", false, colours)
		e2 := NewEnumType("X", false, []EnumValue{{Name: "RED", Number: 0}})
		if MaximumType(e1, e2) != nil {
			t.Error("different value count should return nil")
		}
	})
}

// TestMaximumType_RelationRecursion pins RELATION × RELATION:
// recurse on inner row type, erased on either side blocks.
// (Java's maximumType doesn't have a RELATION branch, but our
// port adds it for completeness — useful when comparing two
// table-valued expressions' types.)
func TestMaximumType_RelationRecursion(t *testing.T) {
	t.Parallel()
	t.Run("identical inner → equal", func(t *testing.T) {
		t.Parallel()
		r1 := NewRelationType(NotNullLong)
		r2 := NewRelationType(NotNullLong)
		got := MaximumType(r1, r2).(*RelationType)
		if !got.Equals(r1) {
			t.Errorf("got %v, want %v", got, r1)
		}
	})
	t.Run("widening inner", func(t *testing.T) {
		t.Parallel()
		r1 := NewRelationType(NotNullInt)
		r2 := NewRelationType(NotNullLong)
		got := MaximumType(r1, r2).(*RelationType)
		if !got.InnerType.Equals(NotNullLong) {
			t.Errorf("inner type: got %v, want NotNullLong", got.InnerType)
		}
	})
	t.Run("incompatible inner → nil", func(t *testing.T) {
		t.Parallel()
		if MaximumType(NewRelationType(NotNullInt), NewRelationType(NotNullString)) != nil {
			t.Error("incompatible inner should return nil")
		}
	})
	t.Run("erased blocks", func(t *testing.T) {
		t.Parallel()
		if MaximumType(NewRelationType(nil), NewRelationType(NotNullLong)) != nil {
			t.Error("erased should return nil")
		}
	})
}

// TestMaximumType_NilHandling pins that nil inputs return nil
// (defensive — never panic on a missing operand).
func TestMaximumType_NilHandling(t *testing.T) {
	t.Parallel()
	if MaximumType(nil, NotNullInt) != nil {
		t.Error("nil left → nil")
	}
	if MaximumType(NotNullInt, nil) != nil {
		t.Error("nil right → nil")
	}
	if MaximumType(nil, nil) != nil {
		t.Error("both nil → nil")
	}
}

// FuzzMaximumType_Properties pins three invariants the lattice
// must hold for primitive Type pairs:
//
//  1. Symmetry: MaximumType(a, b).Equals(MaximumType(b, a)).
//  2. Idempotence: MaximumType(a, a) preserves a's Code (and is
//     never nil — at minimum identity always succeeds).
//  3. Closure: when MaximumType returns non-nil, both inputs must
//     be IsPromotable to the result.
//
// Fuzz inputs are byte pairs interpreted as (low-4-bit code-index,
// high-bit nullable) and mapped to canonical primitive singletons.
// Structured types are out of scope — recursion's invariants don't
// fit a simple property predicate.
func FuzzMaximumType_Properties(f *testing.F) {
	// Seed corpus picks pairs that exercise interesting lattice
	// transitions: identity, every adjacent widening edge, NULL × T,
	// and cross-category rejection.
	f.Add(byte(0), byte(0))    // INT × INT (identity)
	f.Add(byte(0), byte(1))    // INT × LONG (widen)
	f.Add(byte(0), byte(2))    // INT × FLOAT (widen)
	f.Add(byte(0), byte(3))    // INT × DOUBLE (widen)
	f.Add(byte(1), byte(2))    // LONG × FLOAT (widen)
	f.Add(byte(1), byte(3))    // LONG × DOUBLE (widen)
	f.Add(byte(2), byte(3))    // FLOAT × DOUBLE (widen)
	f.Add(byte(7), byte(0))    // NULL × INT (NULL → T-nullable)
	f.Add(byte(7), byte(7))    // NULL × NULL (NULL identity)
	f.Add(byte(0), byte(4))    // INT × STRING (incompatible)
	f.Add(byte(0x80), byte(0)) // INT NULL × INT NOT NULL (nullability fold)
	f.Add(byte(255), byte(255))

	pickType := func(b byte) Type {
		nullable := b&0x80 != 0
		switch b & 0x0f {
		case 0:
			if nullable {
				return NullableInt
			}
			return NotNullInt
		case 1:
			if nullable {
				return NullableLong
			}
			return NotNullLong
		case 2:
			if nullable {
				return NullableFloat
			}
			return NotNullFloat
		case 3:
			if nullable {
				return NullableDouble
			}
			return NotNullDouble
		case 4:
			if nullable {
				return NullableString
			}
			return NotNullString
		case 5:
			if nullable {
				return NullableBoolean
			}
			return NotNullBoolean
		case 6:
			if nullable {
				return NullableBytes
			}
			return NotNullBytes
		case 7:
			return NullType
		}
		return UnknownType
	}

	f.Fuzz(func(t *testing.T, b1, b2 byte) {
		t1 := pickType(b1)
		t2 := pickType(b2)

		// 1. Symmetry.
		ab := MaximumType(t1, t2)
		ba := MaximumType(t2, t1)
		if (ab == nil) != (ba == nil) {
			t.Fatalf("symmetry: %v × %v = %v but reverse = %v", t1, t2, ab, ba)
		}
		if ab != nil && !ab.Equals(ba) {
			t.Fatalf("symmetry: %v vs %v", ab, ba)
		}

		// 2. Idempotence — a × a never nil, preserves Code.
		aa := MaximumType(t1, t1)
		if aa == nil {
			t.Fatalf("idempotence: %v × %v should not be nil", t1, t1)
		}
		if aa.Code() != t1.Code() {
			t.Fatalf("idempotence Code: %v vs %v", aa.Code(), t1.Code())
		}

		// 3. Closure: result is promotable from both inputs.
		if ab != nil {
			if !IsPromotable(t1, ab) {
				t.Fatalf("closure: %v not promotable to %v (max of %v × %v)", t1, ab, t1, t2)
			}
			if !IsPromotable(t2, ab) {
				t.Fatalf("closure: %v not promotable to %v (max of %v × %v)", t2, ab, t1, t2)
			}
		}
	})
}

// TestAnyType_Singleton pins ANY: the universal supertype.
// Always nullable; WithNullability(AnyType, false) panics. Mirrors
// Java's Type.ANY contract.
func TestAnyType_Singleton(t *testing.T) {
	t.Parallel()
	if AnyType.Code() != TypeCodeAny {
		t.Errorf("AnyType.Code(): got %v, want ANY", AnyType.Code())
	}
	if !AnyType.IsNullable() {
		t.Error("AnyType is always nullable")
	}
	if WithNullability(AnyType, true) != AnyType {
		t.Error("WithNullability(AnyType, true) should return the same singleton")
	}
	defer func() {
		if recover() == nil {
			t.Error("expected panic from WithNullability(AnyType, false)")
		}
	}()
	_ = WithNullability(AnyType, false)
}

// TestNoneType_Singleton pins NONE: the type of the untyped empty
// array `[]`. Always non-nullable; WithNullability(NoneType, true)
// panics. Mirrors Java's Type.NONE contract.
func TestNoneType_Singleton(t *testing.T) {
	t.Parallel()
	if NoneType.Code() != TypeCodeNone {
		t.Errorf("NoneType.Code(): got %v, want NONE", NoneType.Code())
	}
	if NoneType.IsNullable() {
		t.Error("NoneType is always non-nullable")
	}
	// WithNullability(NoneType, false) is a no-op.
	if WithNullability(NoneType, false) != NoneType {
		t.Error("WithNullability(NoneType, false) should return the same singleton")
	}
	// WithNullability(NoneType, true) panics.
	defer func() {
		if recover() == nil {
			t.Error("expected panic from WithNullability(NoneType, true)")
		}
	}()
	_ = WithNullability(NoneType, true)
}

// TestArrayType_IsErased pins the typed/erased distinction for
// ArrayType. Mirrors Java's Type.Array.isErased().
func TestArrayType_IsErased(t *testing.T) {
	t.Parallel()
	if !NewArrayType(false, nil).IsErased() {
		t.Error("nil ElementType → IsErased() = true")
	}
	if NewArrayType(false, NotNullLong).IsErased() {
		t.Error("typed ElementType → IsErased() = false")
	}
	if !NewArrayType(true, nil).IsErased() {
		t.Error("nullable + nil element → IsErased() = true")
	}
}

// TestRelationType_Shape pins the basic getters + Equals + String.
// Mirrors Java's Type.Relation contract — always non-nullable,
// inner-type-driven equality, erased-relation handling.
func TestRelationType_Shape(t *testing.T) {
	t.Parallel()

	// Concrete inner type.
	r1 := NewRelationType(NotNullLong)
	if r1.Code() != TypeCodeRelation {
		t.Errorf("Code(): got %v, want RELATION", r1.Code())
	}
	if r1.IsNullable() {
		t.Errorf("RelationType is always non-nullable")
	}
	if r1.IsErased() {
		t.Errorf("RelationType with InnerType is not erased")
	}
	if r1.String() != "RELATION<LONG NOT NULL>" {
		t.Errorf("String(): got %q", r1.String())
	}

	// Erased.
	r2 := NewRelationType(nil)
	if !r2.IsErased() {
		t.Errorf("RelationType with nil InnerType IS erased")
	}
	if r2.String() != "RELATION<?>" {
		t.Errorf("Erased String(): got %q", r2.String())
	}
}

// TestRelationType_Equals pins structural equality semantics:
// same inner type → equal; different inner → unequal; both erased
// → equal.
func TestRelationType_Equals(t *testing.T) {
	t.Parallel()
	a := NewRelationType(NotNullLong)
	b := NewRelationType(NotNullLong)
	c := NewRelationType(NotNullString)
	d := NewRelationType(nil)
	e := NewRelationType(nil)

	if !a.Equals(b) {
		t.Errorf("same inner type should be equal")
	}
	if a.Equals(c) {
		t.Errorf("different inner type should not be equal")
	}
	if !d.Equals(e) {
		t.Errorf("two erased relations should be equal")
	}
	if a.Equals(d) {
		t.Errorf("typed and erased relations should not be equal")
	}
	if a.Equals(nil) {
		t.Errorf("Equals(nil) should be false")
	}
	// Mixing types — RelationType is never equal to a non-Relation.
	if a.Equals(NotNullLong) {
		t.Errorf("RelationType should not equal a non-Relation Type")
	}
}

// TestRelationType_WithNullabilityPanics pins the contract: asking
// for a nullable relation is a programming error and panics.
func TestRelationType_WithNullabilityPanics(t *testing.T) {
	t.Parallel()
	r := NewRelationType(NotNullLong)
	defer func() {
		if recover() == nil {
			t.Fatal("expected WithNullability(RelationType, true) to panic")
		}
	}()
	_ = WithNullability(r, true)
}

// TestRelationType_WithNullabilityNoOp pins that WithNullability(r, false)
// is a no-op (RelationType is already non-nullable).
func TestRelationType_WithNullabilityNoOp(t *testing.T) {
	t.Parallel()
	r := NewRelationType(NotNullLong)
	out := WithNullability(r, false)
	if out != Type(r) {
		t.Errorf("WithNullability(r, false) should return the same instance")
	}
}

// TestWithNullability_Nil pins the nil-input safety contract.
func TestWithNullability_Nil(t *testing.T) {
	t.Parallel()
	if WithNullability(nil, true) != nil {
		t.Error("WithNullability(nil) should return nil")
	}
	if WithNullability(nil, false) != nil {
		t.Error("WithNullability(nil) should return nil")
	}
}

// --- TypeRepository tests -----------------------------------------

// TestTypeRepository_RegisterLookup pins the basic register + lookup
// round-trip plus the founds/not-found split.
func TestTypeRepository_RegisterLookup(t *testing.T) {
	t.Parallel()
	repo := NewTypeRepository()
	if repo.Size() != 0 {
		t.Errorf("Size: got %d, want 0", repo.Size())
	}
	if err := repo.Register("Suit", NewEnumType("Suit", false, []EnumValue{
		{Name: "S", Number: 0}, {Name: "H", Number: 1},
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if repo.Size() != 1 {
		t.Errorf("Size: got %d, want 1", repo.Size())
	}
	got, ok := repo.Lookup("Suit")
	if !ok {
		t.Fatal("Lookup(Suit): not found")
	}
	if got.Code() != TypeCodeEnum {
		t.Errorf("Lookup(Suit).Code(): got %v", got.Code())
	}
	if _, ok := repo.Lookup("Missing"); ok {
		t.Errorf("Lookup(Missing): expected not found")
	}
}

// TestTypeRepository_RejectsInvalid pins the validation arms:
// empty name, nil type, duplicate name all return TypeRegistrationError.
func TestTypeRepository_RejectsInvalid(t *testing.T) {
	t.Parallel()
	repo := NewTypeRepository()
	t.Run("empty-name", func(t *testing.T) {
		t.Parallel()
		err := NewTypeRepository().Register("", NotNullInt)
		if err == nil {
			t.Fatal("expected error on empty name")
		}
		var tre *TypeRegistrationError
		if !errorsAs(err, &tre) {
			t.Errorf("expected TypeRegistrationError, got %T: %v", err, err)
		}
	})
	t.Run("nil-type", func(t *testing.T) {
		t.Parallel()
		err := NewTypeRepository().Register("X", nil)
		if err == nil {
			t.Fatal("expected error on nil type")
		}
		var tre *TypeRegistrationError
		if !errorsAs(err, &tre) || tre.Name != "X" {
			t.Errorf("expected TypeRegistrationError for X, got %v", err)
		}
	})
	t.Run("duplicate-name", func(t *testing.T) {
		t.Parallel()
		if err := repo.Register("DupTest", NotNullInt); err != nil {
			t.Fatalf("first Register: %v", err)
		}
		err := repo.Register("DupTest", NotNullString)
		if err == nil {
			t.Fatal("expected error on duplicate")
		}
		var tre *TypeRegistrationError
		if !errorsAs(err, &tre) || tre.Name != "DupTest" {
			t.Errorf("expected TypeRegistrationError for DupTest, got %v", err)
		}
	})
}

// TestTypeRepository_Names pins that Names() returns every
// registered name (order-undefined, so the test sorts before
// comparing).
func TestTypeRepository_Names(t *testing.T) {
	t.Parallel()
	repo := NewTypeRepository()
	for _, n := range []string{"A", "B", "C"} {
		if err := repo.Register(n, NotNullInt); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	names := repo.Names()
	if len(names) != 3 {
		t.Fatalf("Names len: got %d, want 3", len(names))
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	for _, want := range []string{"A", "B", "C"} {
		if !seen[want] {
			t.Errorf("Names missing %q: %v", want, names)
		}
	}
}

// errorsAs is a thin wrapper around errors.As to keep the call sites
// terse — Go's stdlib API requires importing "errors", which the
// rest of this file doesn't need.
func errorsAs(err error, target any) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if t, ok := target.(**TypeRegistrationError); ok {
			if tre, ok := err.(*TypeRegistrationError); ok {
				*t = tre
				return true
			}
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- Value.Type() tests -------------------------------------------
//
// Post-Track-G1 every Value impl's Type() returns
// the rich Type directly. The tests below pin the per-impl
// type/nullability outputs against the legacy (now-retired)
// `ValueRichType` semantics where each impl forced nullable on
// outputs that could be NULL (CAST, PARAMETER, NULL literal,
// scalar fn over NULL inputs, aggregate operands).

// TestValue_Type_Leaves pins the leaf-Value Type() outputs:
// literals, columns, parameters, NULL.
func TestValue_Type_Leaves(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       Value
		wantStr string
	}{
		{"BooleanValue(true)", NewBooleanValue(true), "BOOLEAN NOT NULL"},
		{"BooleanValue(false)", NewBooleanValue(false), "BOOLEAN NOT NULL"},
		{"BooleanValue(nil)", &BooleanValue{Value: nil}, "BOOLEAN NULL"},
		// ConstantValue: nullability is derived from Value (non-nil
		// → NOT NULL, nil → NULL). The Typ field's own nullability
		// is overridden — callers don't need to pre-pick the right
		// NotNull/Nullable singleton.
		{"ConstantValue(int64=5)", &ConstantValue{Value: int64(5), Typ: TypeInt}, "LONG NOT NULL"},
		{"ConstantValue(string=hello)", &ConstantValue{Value: "hello", Typ: TypeString}, "STRING NOT NULL"},
		{"ConstantValue(nil)", &ConstantValue{Value: nil, Typ: TypeInt}, "LONG NULL"},
		{"NullValue(typed-INT)", &NullValue{Typ: TypeInt}, "LONG NULL"},
		{"NullValue(unknown)", &NullValue{Typ: TypeUnknown}, "UNKNOWN NULL"},
		{"FieldValue(int)", &FieldValue{Field: "x", Typ: TypeInt}, "LONG NULL"},
		{"FieldValue(bool)", &FieldValue{Field: "active", Typ: TypeBool}, "BOOLEAN NULL"},
		{"ParameterValue(int)", &ParameterValue{Ordinal: 1, Typ: TypeInt}, "LONG NULL"},
	}
	for _, tc := range cases {
		got := tc.v.Type()
		if got.String() != tc.wantStr {
			t.Errorf("%s: got %q, want %q", tc.name, got.String(), tc.wantStr)
		}
	}
}

// TestValue_Type_Composites pins the composite-Value Type() outputs:
// ArithmeticValue, CastValue, PromoteValue, NotValue, ScalarFunctionValue.
func TestValue_Type_Composites(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       Value
		wantStr string
	}{
		{
			"ArithmeticValue(c+c)",
			&ArithmeticValue{
				Op:    OpAdd,
				Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
				Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
			},
			"LONG NULL",
		},
		{
			"CastValue(int → STRING)",
			NewCastValue(&ConstantValue{Value: int64(42), Typ: TypeInt}, TypeString),
			"STRING NULL",
		},
		{
			"PromoteValue(NOT NULL bool → FLOAT)",
			// BooleanValue(true).Type() == NotNullBoolean → promote
			// inherits NOT NULL → DOUBLE NOT NULL.
			NewPromoteValue(NewBooleanValue(true), TypeFloat),
			"DOUBLE NOT NULL",
		},
		{
			"PromoteValue(NULL field → FLOAT)",
			NewPromoteValue(&FieldValue{Field: "x", Typ: TypeFloat}, TypeFloat),
			"DOUBLE NULL",
		},
		{
			"NotValue(BooleanValue)",
			NewNotValue(NewBooleanValue(true)),
			"BOOLEAN NOT NULL",
		},
		{
			"ScalarFunctionValue(UPPER)",
			NewScalarFunctionValue("UPPER", TypeString,
				&ConstantValue{Value: "x", Typ: TypeString}),
			"STRING NULL",
		},
	}
	for _, tc := range cases {
		got := tc.v.Type()
		if got.String() != tc.wantStr {
			t.Errorf("%s: got %q, want %q", tc.name, got.String(), tc.wantStr)
		}
	}
}

// TestValue_Type_Aggregate pins the aggregate-specific rules:
// COUNT / COUNT(*) → NOT NULL long; SUM / MIN / MAX / AVG with an
// operand → operand-type but nullable (returns NULL on empty
// groups).
func TestValue_Type_Aggregate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       Value
		wantStr string
	}{
		{"COUNT(*)", NewAggregateValue(AggCountStar, nil), "LONG NOT NULL"},
		{
			"COUNT(col)",
			NewAggregateValue(AggCount, &FieldValue{Field: "x", Typ: TypeInt}),
			"LONG NOT NULL",
		},
		{
			"SUM(col)",
			NewAggregateValue(AggSum, &FieldValue{Field: "x", Typ: TypeInt}),
			"LONG NULL",
		},
		{
			"MIN(string)",
			NewAggregateValue(AggMin, &FieldValue{Field: "name", Typ: TypeString}),
			"STRING NULL",
		},
	}
	for _, tc := range cases {
		got := tc.v.Type()
		if got.String() != tc.wantStr {
			t.Errorf("%s: got %q, want %q", tc.name, got.String(), tc.wantStr)
		}
	}
}

// TestValue_Type_RecordConstructor pins the synthesised RecordType
// path: the constructor produces an anonymous nullable RecordType
// whose Fields carry per-field types derived from each child Value.
func TestValue_Type_RecordConstructor(t *testing.T) {
	t.Parallel()
	rcv := NewRecordConstructorValue(
		RecordConstructorField{Name: "id", Value: NewBooleanValue(true)},
		RecordConstructorField{Name: "v", Value: &ConstantValue{Value: int64(42), Typ: TypeInt}},
	)
	got := rcv.Type()
	rt, ok := got.(*RecordType)
	if !ok {
		t.Fatalf("expected RecordType, got %T", got)
	}
	if rt.RecordName != "" {
		t.Errorf("RecordName: got %q, want \"\"", rt.RecordName)
	}
	if !rt.Nullable {
		t.Errorf("Nullable: got false, want true (synthesised record always nullable)")
	}
	if len(rt.Fields) != 2 {
		t.Fatalf("Fields len: got %d, want 2", len(rt.Fields))
	}
	// First field: BooleanValue(true).Type() == NotNullBoolean.
	if rt.Fields[0].Name != "id" || !rt.Fields[0].FieldType.Equals(NotNullBoolean) {
		t.Errorf("Fields[0]: got %v", rt.Fields[0])
	}
	// Second field: ConstantValue(int64=5, Typ=TypeInt).Type() ==
	// NotNullLong (non-nil Value → NOT NULL per ConstantValue.Type()).
	if rt.Fields[1].Name != "v" || !rt.Fields[1].FieldType.Equals(NotNullLong) {
		t.Errorf("Fields[1]: got %v", rt.Fields[1])
	}
}
