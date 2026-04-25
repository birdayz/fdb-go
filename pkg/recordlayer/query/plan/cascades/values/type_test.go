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
