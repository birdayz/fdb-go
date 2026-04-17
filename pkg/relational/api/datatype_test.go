package api

import (
	"strings"
	"testing"
)

// ---- primitive singleton + nullability ----

func TestPrimitiveSingletons(t *testing.T) {
	t.Parallel()
	// NewXxxType returns the same pointer for repeated calls with the
	// same nullability. Acts as a low-cost equality shortcut.
	pairs := []struct {
		name string
		a, b DataType
	}{
		{"boolean", NewBooleanType(false), NewBooleanType(false)},
		{"boolean.null", NewBooleanType(true), NewBooleanType(true)},
		{"int", NewIntegerType(false), NewIntegerType(false)},
		{"long", NewLongType(false), NewLongType(false)},
		{"float", NewFloatType(false), NewFloatType(false)},
		{"double", NewDoubleType(false), NewDoubleType(false)},
		{"string", NewStringType(false), NewStringType(false)},
		{"bytes", NewBytesType(false), NewBytesType(false)},
		{"version", NewVersionType(false), NewVersionType(false)},
		{"uuid", NewUUIDType(false), NewUUIDType(false)},
		{"null", NewNullType(), NewNullType()},
		{"unknown", NewUnknownType(), NewUnknownType()},
	}
	for _, p := range pairs {
		if p.a != p.b {
			t.Errorf("%s: expected singleton, got distinct pointers", p.name)
		}
	}
}

func TestPrimitiveFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		t             DataType
		wantNullable  bool
		wantPrimitive bool
		wantCode      Code
	}{
		{"bool not-null", NewBooleanType(false), false, true, CodeBoolean},
		{"bool null", NewBooleanType(true), true, true, CodeBoolean},
		{"int", NewIntegerType(false), false, true, CodeInteger},
		{"long", NewLongType(true), true, true, CodeLong},
		{"string", NewStringType(false), false, true, CodeString},
		{"bytes", NewBytesType(true), true, true, CodeBytes},
		{"version", NewVersionType(false), false, true, CodeVersion},
		{"uuid", NewUUIDType(true), true, true, CodeUUID},
		{"null", NewNullType(), true, true, CodeNull},
		{"unknown", NewUnknownType(), false, false, CodeUnknown},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.t.IsNullable() != tc.wantNullable {
				t.Errorf("IsNullable = %v, want %v", tc.t.IsNullable(), tc.wantNullable)
			}
			if tc.t.IsPrimitive() != tc.wantPrimitive {
				t.Errorf("IsPrimitive = %v, want %v", tc.t.IsPrimitive(), tc.wantPrimitive)
			}
			if tc.t.Code() != tc.wantCode {
				t.Errorf("Code = %v, want %v", tc.t.Code(), tc.wantCode)
			}
		})
	}
}

func TestPrimitiveWithNullableToggles(t *testing.T) {
	t.Parallel()
	a := NewIntegerType(false)
	b := a.WithNullable(true)
	c := b.WithNullable(false)
	if b.IsNullable() != true {
		t.Error("WithNullable(true) did not flip flag")
	}
	if c != a {
		t.Error("round-trip should return original singleton")
	}
}

func TestPrimitiveResolvedSelf(t *testing.T) {
	t.Parallel()
	types := []DataType{
		NewBooleanType(false), NewIntegerType(false), NewLongType(false),
		NewFloatType(false), NewDoubleType(false), NewStringType(false),
		NewBytesType(false), NewVersionType(false), NewUUIDType(false),
		NewNullType(),
	}
	for _, ty := range types {
		if !ty.IsResolved() {
			t.Errorf("%s: primitive should be resolved", ty.Code())
		}
		if got := ty.Resolve(nil); got != ty {
			t.Errorf("%s: Resolve(nil) did not return self", ty.Code())
		}
	}
}

func TestPrimitiveStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    DataType
		want string
	}{
		{NewBooleanType(false), "boolean"},
		{NewBooleanType(true), "boolean ∪ ∅"},
		{NewIntegerType(false), "int"},
		{NewIntegerType(true), "int ∪ ∅"},
		{NewLongType(false), "long"},
		{NewFloatType(false), "float"},
		{NewDoubleType(false), "double"},
		{NewStringType(false), "string"},
		{NewBytesType(false), "bytes"},
		{NewVersionType(false), "version"},
		{NewUUIDType(false), "uuid"},
		{NewNullType(), "null"},
		{NewUnknownType(), "???"},
	}
	for _, c := range cases {
		if got := c.t.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

func TestPrimitiveEqual(t *testing.T) {
	t.Parallel()
	if !NewIntegerType(false).Equal(NewIntegerType(false)) {
		t.Fatal("same type should be equal")
	}
	if NewIntegerType(false).Equal(NewIntegerType(true)) {
		t.Fatal("different nullability should not be equal")
	}
	if NewIntegerType(false).Equal(NewLongType(false)) {
		t.Fatal("int != long")
	}
}

// ---- NULL type special cases ----

func TestNullTypeNonNullablePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		e, ok := r.(*Error)
		if !ok || e.Code != ErrCodeInternalError {
			t.Fatalf("expected InternalError, got %v", r)
		}
	}()
	NewNullType().WithNullable(false)
}

// ---- UnknownType special cases ----

func TestUnknownTypePanics(t *testing.T) {
	t.Parallel()
	u := NewUnknownType()
	func() {
		defer func() { _ = recover() }()
		u.WithNullable(true)
		t.Error("WithNullable should have panicked")
	}()
	func() {
		defer func() { _ = recover() }()
		u.Resolve(nil)
		t.Error("Resolve should have panicked")
	}()
}

// ---- VectorType ----

func TestVectorType(t *testing.T) {
	t.Parallel()
	v := NewVectorType(32, 128, false)
	if v.Precision() != 32 || v.Dimensions() != 128 {
		t.Errorf("precision/dimensions: %+v", v)
	}
	if v.IsNullable() {
		t.Error("should not be nullable")
	}
	if !v.IsResolved() {
		t.Error("vector should be resolved")
	}
	if got := v.String(); got != "vector(p=32, d=128)" {
		t.Errorf("String() = %q", got)
	}

	// Toggle nullability.
	v2 := v.WithNullable(true)
	if !v2.IsNullable() {
		t.Error("WithNullable(true) did not flip")
	}
	if !strings.HasSuffix(v2.String(), "∪ ∅") {
		t.Errorf("nullable suffix missing: %q", v2.String())
	}
	// Same-nullability returns self.
	if v.WithNullable(false) != v {
		t.Error("idempotent WithNullable should return same pointer")
	}

	// Equality.
	if !v.Equal(NewVectorType(32, 128, false)) {
		t.Error("equal vectors not equal")
	}
	if v.Equal(NewVectorType(64, 128, false)) {
		t.Error("different precision considered equal")
	}
	if v.Equal(NewVectorType(32, 64, false)) {
		t.Error("different dimensions considered equal")
	}
}

// ---- ArrayType ----

func TestArrayType(t *testing.T) {
	t.Parallel()
	a := NewArrayType(NewIntegerType(false), false)
	if a.ElementType().Code() != CodeInteger {
		t.Errorf("element type: %v", a.ElementType())
	}
	if a.IsPrimitive() {
		t.Error("array is not primitive")
	}
	if !a.IsResolved() {
		t.Error("array of int should be resolved")
	}
	if got := a.String(); got != "[int]" {
		t.Errorf("String() = %q", got)
	}

	// Nested (array of array).
	nested := NewArrayType(a, true)
	if !nested.IsResolved() {
		t.Error("array of array of int should be resolved")
	}
	if got := nested.String(); got != "[[int]] ∪ ∅" {
		t.Errorf("String() = %q", got)
	}
}

func TestArrayTypeResolvePropagation(t *testing.T) {
	t.Parallel()
	// An array of UnresolvedType is not resolved; Resolve() must
	// rebuild with the inner type resolved.
	u := NewUnresolvedType("MyType", false)
	a := NewArrayType(u, false)
	if a.IsResolved() {
		t.Fatal("array over unresolved should not be resolved")
	}
	// Resolution map values must be Named (matches Java's
	// Map<String, Named>). Use an EnumType as the target.
	target := NewEnumType("MyType", []EnumValue{NewEnumValue("A", 0)}, false)
	resMap := map[string]Named{"MyType": target}
	resolved := a.Resolve(resMap).(*ArrayType)
	if !resolved.IsResolved() {
		t.Fatal("resolved array should be resolved")
	}
	if !resolved.ElementType().Equal(target) {
		t.Errorf("element not resolved: %v", resolved.ElementType())
	}
}

func TestArrayTypeEqualAndStructure(t *testing.T) {
	t.Parallel()
	a := NewArrayType(NewIntegerType(false), false)
	b := NewArrayType(NewIntegerType(false), false)
	if !a.Equal(b) {
		t.Error("same arrays should be equal")
	}
	if !a.HasIdenticalStructure(b) {
		t.Error("same arrays should have identical structure")
	}
	c := NewArrayType(NewLongType(false), false)
	if a.Equal(c) {
		t.Error("arrays with different element types should differ")
	}
	if a.HasIdenticalStructure(c) {
		t.Error("arrays with different element types should have different structure")
	}
}

// ---- EnumType ----

func TestEnumType(t *testing.T) {
	t.Parallel()
	vals := []EnumValue{NewEnumValue("RED", 0), NewEnumValue("GREEN", 1)}
	e := NewEnumType("Color", vals, false)
	if e.Name() != "Color" {
		t.Errorf("Name = %q", e.Name())
	}
	got := e.Values()
	if len(got) != 2 || got[0].Name() != "RED" || got[1].Number() != 1 {
		t.Errorf("values: %+v", got)
	}
	// Returned slice is a copy — mutating it must not affect the type.
	got[0] = NewEnumValue("MUTATED", 99)
	if e.Values()[0].Name() == "MUTATED" {
		t.Error("Values() returned internal slice, not a copy")
	}
	if s := e.String(); !strings.Contains(s, "Color") || !strings.Contains(s, "RED") {
		t.Errorf("String() = %q", s)
	}
}

func TestEnumTypeConstructionPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values []EnumValue
		tname  string
	}{
		{"empty name", []EnumValue{NewEnumValue("x", 0)}, ""},
		{"empty values", nil, "Color"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			NewEnumType(tc.tname, tc.values, false)
		})
	}
}

// ---- StructType ----

func TestStructType(t *testing.T) {
	t.Parallel()
	fields := []StructField{
		NewStructField("id", NewLongType(false), 0),
		NewStructField("name", NewStringType(false), 1),
	}
	s := NewStructType("User", fields, false)
	if s.Name() != "User" || s.NumFields() != 2 {
		t.Errorf("unexpected struct: %+v", s)
	}
	if s.Field(0).Name() != "id" || s.Field(1).Type().Code() != CodeString {
		t.Error("field accessor broken")
	}
	if !s.IsResolved() {
		t.Error("struct of resolved fields should be resolved")
	}
}

func TestStructTypeUnresolved(t *testing.T) {
	t.Parallel()
	u := NewUnresolvedType("Addr", false)
	fields := []StructField{
		NewStructField("id", NewLongType(false), 0),
		NewStructField("addr", u, 1),
	}
	s := NewStructType("User", fields, false)
	if s.IsResolved() {
		t.Fatal("struct with unresolved field should not be resolved")
	}
	// Resolution map values must be Named — use a nested StructType.
	addrStruct := NewStructType("Addr", []StructField{
		NewStructField("street", NewStringType(false), 0),
	}, false)
	resolved := s.Resolve(map[string]Named{
		"Addr": addrStruct,
	}).(*StructType)
	if !resolved.IsResolved() {
		t.Fatal("resolved struct should be resolved")
	}
	if resolved.Field(1).Type().Code() != CodeStruct {
		t.Errorf("field not resolved: %+v", resolved.Field(1))
	}
}

func TestStructFieldIndexNegativePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		e, ok := r.(*Error)
		if !ok || e.Code != ErrCodeInternalError {
			t.Fatalf("unexpected: %v", r)
		}
	}()
	NewStructField("bad", NewLongType(false), -1)
}

func TestStructHasIdenticalStructure(t *testing.T) {
	t.Parallel()
	a := NewStructType("A", []StructField{
		NewStructField("x", NewLongType(false), 0),
	}, false)
	b := NewStructType("B", []StructField{
		NewStructField("x", NewLongType(false), 0),
	}, false)
	// Different name — Equal is false, HasIdenticalStructure is true.
	if a.Equal(b) {
		t.Error("structs with different names should not Equal")
	}
	if !a.HasIdenticalStructure(b) {
		t.Error("structs with identical shape should be structurally identical")
	}

	c := NewStructType("B", []StructField{
		NewStructField("x", NewStringType(false), 0),
	}, false)
	if a.HasIdenticalStructure(c) {
		t.Error("different field types → not structurally identical")
	}
}

// ---- UnresolvedType ----

func TestUnresolvedType(t *testing.T) {
	t.Parallel()
	u := NewUnresolvedType("X", true)
	if u.Name() != "X" || !u.IsNullable() || u.IsResolved() {
		t.Errorf("unexpected: %+v", u)
	}
	// Missing resolution map entry panics.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		e, ok := r.(*Error)
		if !ok || e.Code != ErrCodeInternalError {
			t.Fatalf("unexpected: %v", r)
		}
	}()
	u.Resolve(map[string]Named{})
}

// ---- JDBC mapping ----

func TestJDBCType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		c    Code
		want int
	}{
		{CodeBoolean, JDBCBoolean},
		{CodeLong, JDBCBigInt},
		{CodeInteger, JDBCInteger},
		{CodeFloat, JDBCFloat},
		{CodeDouble, JDBCDouble},
		{CodeString, JDBCVarchar},
		{CodeBytes, JDBCBinary},
		{CodeVersion, JDBCBinary},
		{CodeEnum, JDBCOther},
		{CodeUUID, JDBCOther},
		{CodeVector, JDBCOther},
		{CodeUnknown, JDBCOther},
		{CodeStruct, JDBCStruct},
		{CodeArray, JDBCArray},
		{CodeNull, JDBCNull},
	}
	for _, c := range cases {
		if got := JDBCType(c.c); got != c.want {
			t.Errorf("JDBCType(%s) = %d, want %d", c.c, got, c.want)
		}
	}
}

// ---- Interface assertions (compile-time safety net) ----

var (
	_ DataType      = (*BooleanType)(nil)
	_ DataType      = (*IntegerType)(nil)
	_ DataType      = (*LongType)(nil)
	_ DataType      = (*FloatType)(nil)
	_ DataType      = (*DoubleType)(nil)
	_ DataType      = (*StringType)(nil)
	_ DataType      = (*BytesType)(nil)
	_ DataType      = (*VersionType)(nil)
	_ DataType      = (*UUIDType)(nil)
	_ DataType      = (*NullType)(nil)
	_ DataType      = (*UnknownType)(nil)
	_ DataType      = (*VectorType)(nil)
	_ DataType      = (*ArrayType)(nil)
	_ DataType      = (*EnumType)(nil)
	_ DataType      = (*StructType)(nil)
	_ DataType      = (*UnresolvedType)(nil)
	_ Named         = (*EnumType)(nil)
	_ Named         = (*StructType)(nil)
	_ Named         = (*UnresolvedType)(nil)
	_ CompositeType = (*ArrayType)(nil)
	_ CompositeType = (*StructType)(nil)
)
