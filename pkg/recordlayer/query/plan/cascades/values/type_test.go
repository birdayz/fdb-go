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

// TestFromValueType pins the legacy-to-new bridge. Each ValueType
// maps to a concrete Type, and nullability comes from the caller
// (since ValueType doesn't track it).
func TestFromValueType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		vt       ValueType
		nullable bool
		wantCode TypeCode
		wantNull bool
	}{
		{TypeBool, true, TypeCodeBoolean, true},
		{TypeBool, false, TypeCodeBoolean, false},
		{TypeInt, true, TypeCodeLong, true},
		{TypeInt, false, TypeCodeLong, false},
		{TypeFloat, true, TypeCodeDouble, true},
		{TypeFloat, false, TypeCodeDouble, false},
		{TypeString, true, TypeCodeString, true},
		{TypeString, false, TypeCodeString, false},
		// TypeUnknown maps to UnknownType regardless of the nullable
		// flag — the bridge can't synthesise a code that isn't there.
		{TypeUnknown, true, TypeCodeUnknown, true},
		{TypeUnknown, false, TypeCodeUnknown, true}, // UnknownType is always nullable
	}
	for _, tc := range cases {
		got := FromValueType(tc.vt, tc.nullable)
		if got.Code() != tc.wantCode {
			t.Errorf("FromValueType(%v, %v).Code(): got %v, want %v", tc.vt, tc.nullable, got.Code(), tc.wantCode)
		}
		if got.IsNullable() != tc.wantNull {
			t.Errorf("FromValueType(%v, %v).IsNullable(): got %v, want %v", tc.vt, tc.nullable, got.IsNullable(), tc.wantNull)
		}
	}
}

// TestToValueType pins the reverse bridge. LONG / DOUBLE collapse
// into the seed's TypeInt / TypeFloat (the legacy enum has no
// width distinction). Special / structured codes degrade to
// TypeUnknown.
func TestToValueType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    Type
		want ValueType
	}{
		{NotNullInt, TypeInt},
		{NullableInt, TypeInt},
		{NotNullLong, TypeInt}, // collapse
		{NotNullFloat, TypeFloat},
		{NotNullDouble, TypeFloat}, // collapse
		{NotNullString, TypeString},
		// BYTES has no legacy ValueType — degrades to TypeUnknown.
		{NotNullBytes, TypeUnknown},
		{NotNullBoolean, TypeBool},
		{NullType, TypeUnknown},
		{UnknownType, TypeUnknown},
		{nil, TypeUnknown},
	}
	for _, tc := range cases {
		if got := ToValueType(tc.t); got != tc.want {
			t.Errorf("ToValueType(%v): got %v, want %v", tc.t, got, tc.want)
		}
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

// TestType_RoundTrip pins the (FromValueType ∘ ToValueType) ≈ id
// invariant for the value types the legacy enum actually
// represents. NotNull bit is inferable from the round-trip target
// — we always feed nullable=false in this direction.
func TestType_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, vt := range []ValueType{TypeBool, TypeInt, TypeFloat, TypeString} {
		t1 := FromValueType(vt, false)
		got := ToValueType(t1)
		if got != vt {
			t.Errorf("ValueType(%v) -> Type(%v) -> ValueType(%v): not a round-trip", vt, t1, got)
		}
	}
}
