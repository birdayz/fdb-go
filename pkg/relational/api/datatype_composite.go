package api

import (
	"strings"
)

// Composite DataType implementations — nested / parameterized types.

// ---- VectorType ----
//
// VectorType carries a bit-precision (16, 32, 64) and a dimension
// count. Mirrors Java's VectorType with precision + dimensions fields.

type VectorType struct {
	typeBase
	precision  int
	dimensions int
}

// NewVectorType returns a VectorType with the given precision (bits per
// element) and dimension count. Precision and dimensions must be > 0;
// callers are responsible for validation — matches Java (no checks).
func NewVectorType(precision, dimensions int, isNullable bool) *VectorType {
	return &VectorType{
		typeBase:   typeBase{code: CodeVector, isPrimitive: true, isNullable: isNullable},
		precision:  precision,
		dimensions: dimensions,
	}
}

func (t *VectorType) Precision() int  { return t.precision }
func (t *VectorType) Dimensions() int { return t.dimensions }

func (t *VectorType) IsResolved() bool { return true }

func (t *VectorType) WithNullable(isNullable bool) DataType {
	if isNullable == t.isNullable {
		return t
	}
	return NewVectorType(t.precision, t.dimensions, isNullable)
}

func (t *VectorType) Resolve(_ map[string]Named) DataType { return t }

func (t *VectorType) Equal(other DataType) bool {
	o, ok := other.(*VectorType)
	return ok && o.precision == t.precision && o.dimensions == t.dimensions && o.isNullable == t.isNullable
}

func (t *VectorType) String() string {
	var b strings.Builder
	b.WriteString("vector(p=")
	writeInt(&b, t.precision)
	b.WriteString(", d=")
	writeInt(&b, t.dimensions)
	b.WriteByte(')')
	b.WriteString(nullableSuffix(t.isNullable))
	return b.String()
}

// ---- ArrayType ----

type ArrayType struct {
	typeBase
	elementType DataType
}

// NewArrayType returns an ArrayType whose elements have the given type.
// Java's ArrayType.from(DataType) defaults isNullable=false.
func NewArrayType(elementType DataType, isNullable bool) *ArrayType {
	return &ArrayType{
		typeBase:    typeBase{code: CodeArray, isNullable: isNullable},
		elementType: elementType,
	}
}

func (t *ArrayType) ElementType() DataType { return t.elementType }

func (t *ArrayType) IsResolved() bool { return t.elementType.IsResolved() }

func (t *ArrayType) WithNullable(isNullable bool) DataType {
	if isNullable == t.isNullable {
		return t
	}
	return NewArrayType(t.elementType, isNullable)
}

func (t *ArrayType) Resolve(resolution map[string]Named) DataType {
	if t.IsResolved() {
		return t
	}
	return NewArrayType(t.elementType.Resolve(resolution), t.isNullable)
}

func (t *ArrayType) Equal(other DataType) bool {
	o, ok := other.(*ArrayType)
	return ok && o.isNullable == t.isNullable && o.elementType.Equal(t.elementType)
}

// HasIdenticalStructure treats ArrayTypes with structurally-identical
// element types as identical, matching Java's CompositeType contract.
func (t *ArrayType) HasIdenticalStructure(other any) bool {
	o, ok := other.(*ArrayType)
	if !ok {
		return false
	}
	if t.isNullable != o.isNullable {
		return false
	}
	if comp, ok := t.elementType.(CompositeType); ok {
		return comp.HasIdenticalStructure(o.elementType)
	}
	return t.elementType.Equal(o.elementType)
}

func (t *ArrayType) String() string {
	return "[" + t.elementType.String() + "]" + nullableSuffix(t.isNullable)
}

// ---- EnumValue ----

// EnumValue is a single entry in an EnumType. Name and number are both
// identity-bearing — equality requires both to match.
type EnumValue struct {
	name   string
	number int
}

// NewEnumValue constructs an EnumValue.
func NewEnumValue(name string, number int) EnumValue {
	return EnumValue{name: name, number: number}
}

func (v EnumValue) Name() string { return v.name }
func (v EnumValue) Number() int  { return v.number }
func (v EnumValue) String() string {
	return v.name
}

// ---- EnumType ----

type EnumType struct {
	typeBase
	name   string
	values []EnumValue
}

// NewEnumType constructs a named enum type. Panics with
// ErrCodeInternalError if name is empty or values is empty — matches
// Java's Assert.thatUnchecked in EnumType.from.
func NewEnumType(name string, values []EnumValue, isNullable bool) *EnumType {
	if name == "" {
		panic(NewError(ErrCodeInternalError, "enum type name must not be empty"))
	}
	if len(values) == 0 {
		panic(NewError(ErrCodeInternalError, "enum type must have at least one value"))
	}
	// Defensive copy — callers must not mutate after construction.
	cp := make([]EnumValue, len(values))
	copy(cp, values)
	return &EnumType{
		typeBase: typeBase{code: CodeEnum, isPrimitive: true, isNullable: isNullable},
		name:     name,
		values:   cp,
	}
}

func (t *EnumType) Name() string { return t.name }

// Values returns a copy of the enum values. The internal slice is not
// exposed to preserve immutability.
func (t *EnumType) Values() []EnumValue {
	out := make([]EnumValue, len(t.values))
	copy(out, t.values)
	return out
}

func (t *EnumType) IsResolved() bool { return true }

func (t *EnumType) WithNullable(isNullable bool) DataType {
	if isNullable == t.isNullable {
		return t
	}
	return NewEnumType(t.name, t.values, isNullable)
}

func (t *EnumType) Resolve(_ map[string]Named) DataType { return t }

func (t *EnumType) Equal(other DataType) bool {
	o, ok := other.(*EnumType)
	if !ok {
		return false
	}
	if o.isNullable != t.isNullable || o.name != t.name || len(o.values) != len(t.values) {
		return false
	}
	for i, v := range t.values {
		if v != o.values[i] {
			return false
		}
	}
	return true
}

// String mirrors Java's EnumType.toString(): "enum(Name){V1,V2}".
// Notably Java does NOT append the "∪ ∅" nullability suffix on
// EnumType (unlike primitives), so neither do we — wire-format
// compatibility takes precedence over surface regularity.
func (t *EnumType) String() string {
	var b strings.Builder
	b.WriteString("enum(")
	b.WriteString(t.name)
	b.WriteString("){")
	for i, v := range t.values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(v.name)
	}
	b.WriteByte('}')
	return b.String()
}

// ---- StructField ----

// StructField is one field of a StructType. index is the declared
// position (>= 0); Java's StructType.Field stores it for reporting.
type StructField struct {
	name  string
	typ   DataType
	index int
}

// NewStructField constructs a StructField. Panics with
// ErrCodeInternalError if index < 0 — matches Java's Assert.thatUnchecked.
func NewStructField(name string, typ DataType, index int) StructField {
	if index < 0 {
		panic(NewErrorf(ErrCodeInternalError, "struct field index must be >= 0, got %d", index))
	}
	return StructField{name: name, typ: typ, index: index}
}

func (f StructField) Name() string   { return f.name }
func (f StructField) Type() DataType { return f.typ }
func (f StructField) Index() int     { return f.index }
func (f StructField) Equal(o StructField) bool {
	return f.name == o.name && f.index == o.index && f.typ.Equal(o.typ)
}
func (f StructField) String() string { return f.name }

// ---- StructType ----

type StructType struct {
	typeBase
	name   string
	fields []StructField
}

// NewStructType constructs a struct type. The fields slice is copied
// defensively.
func NewStructType(name string, fields []StructField, isNullable bool) *StructType {
	cp := make([]StructField, len(fields))
	copy(cp, fields)
	return &StructType{
		typeBase: typeBase{code: CodeStruct, isNullable: isNullable},
		name:     name,
		fields:   cp,
	}
}

func (t *StructType) Name() string { return t.name }

// Fields returns a copy of the fields. The internal slice is not
// exposed to preserve immutability.
func (t *StructType) Fields() []StructField {
	out := make([]StructField, len(t.fields))
	copy(out, t.fields)
	return out
}

// NumFields returns the number of fields without allocating.
func (t *StructType) NumFields() int { return len(t.fields) }

// Field returns the i-th field. Panics if i is out of range.
func (t *StructType) Field(i int) StructField { return t.fields[i] }

func (t *StructType) IsResolved() bool {
	for _, f := range t.fields {
		if !f.typ.IsResolved() {
			return false
		}
	}
	return true
}

func (t *StructType) WithNullable(isNullable bool) DataType {
	if isNullable == t.isNullable {
		return t
	}
	return NewStructType(t.name, t.fields, isNullable)
}

func (t *StructType) Resolve(resolution map[string]Named) DataType {
	if t.IsResolved() {
		return t
	}
	resolved := make([]StructField, len(t.fields))
	for i, f := range t.fields {
		resolved[i] = NewStructField(f.name, f.typ.Resolve(resolution), f.index)
	}
	return NewStructType(t.name, resolved, t.isNullable)
}

func (t *StructType) Equal(other DataType) bool {
	o, ok := other.(*StructType)
	if !ok {
		return false
	}
	if o.isNullable != t.isNullable || o.name != t.name || len(o.fields) != len(t.fields) {
		return false
	}
	for i, f := range t.fields {
		if !f.Equal(o.fields[i]) {
			return false
		}
	}
	return true
}

// HasIdenticalStructure compares struct types ignoring name — same
// field list (names + indexes + structural types) implies identical.
func (t *StructType) HasIdenticalStructure(other any) bool {
	o, ok := other.(*StructType)
	if !ok {
		return false
	}
	if t.isNullable != o.isNullable || len(t.fields) != len(o.fields) {
		return false
	}
	for i, f := range t.fields {
		of := o.fields[i]
		if f.name != of.name || f.index != of.index {
			return false
		}
		if comp, ok := f.typ.(CompositeType); ok {
			if !comp.HasIdenticalStructure(of.typ) {
				return false
			}
		} else if !f.typ.Equal(of.typ) {
			return false
		}
	}
	return true
}

// String mirrors Java's StructType.toString():
// `{name[:5]} { field1:type1,field2:type2 } `. Java does NOT append
// the nullability suffix, so neither do we.
//
// The name is truncated to the first 5 *runes* — not bytes, to avoid
// slicing a multibyte character in half. Java's `String.substring(0, 5)`
// operates on UTF-16 code units; for any name that fits in the BMP the
// result is byte-identical.
//
// NB: Java's 5-rune truncation can produce false collisions in plan
// cache keys when two struct types share a 5-rune prefix. This matches
// Java deliberately — changing it would break wire compatibility.
func (t *StructType) String() string {
	var b strings.Builder
	b.WriteString(truncateRunes(t.name, 5))
	b.WriteString(" { ")
	for i, f := range t.fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f.name)
		b.WriteByte(':')
		b.WriteString(f.typ.String())
	}
	b.WriteString(" } ")
	return b.String()
}

// truncateRunes returns the first n runes of s. Safe for any UTF-8
// input (no mid-rune slicing). If s has fewer than n runes, returns
// s unchanged.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// ---- UnresolvedType ----
//
// UnresolvedType is a forward reference used during parse to name a
// type that has not been declared yet. Resolve() replaces it with the
// looked-up Named type, preserving this instance's nullability.

type UnresolvedType struct {
	typeBase
	name string
}

// NewUnresolvedType constructs an unresolved placeholder for the given name.
func NewUnresolvedType(name string, isNullable bool) *UnresolvedType {
	return &UnresolvedType{
		typeBase: typeBase{code: CodeUnknown, isNullable: isNullable},
		name:     name,
	}
}

func (t *UnresolvedType) Name() string { return t.name }

func (t *UnresolvedType) IsResolved() bool { return false }

func (t *UnresolvedType) WithNullable(isNullable bool) DataType {
	if isNullable == t.isNullable {
		return t
	}
	return NewUnresolvedType(t.name, isNullable)
}

// Resolve looks up the referenced name in the resolution map and
// returns it with this instance's nullability applied. Panics with
// ErrCodeInternalError if the name is not present (matches Java's
// Assert.thatUnchecked in UnresolvedType.resolve).
func (t *UnresolvedType) Resolve(resolution map[string]Named) DataType {
	n, ok := resolution[t.name]
	if !ok {
		panic(NewErrorf(ErrCodeInternalError, "could not find type %s", t.name))
	}
	dt, ok := n.(DataType)
	if !ok {
		panic(NewErrorf(ErrCodeInternalError, "resolved entry for %s is not a DataType", t.name))
	}
	return dt.WithNullable(t.isNullable)
}

func (t *UnresolvedType) Equal(other DataType) bool {
	o, ok := other.(*UnresolvedType)
	return ok && o.isNullable == t.isNullable && o.name == t.name
}

func (t *UnresolvedType) String() string {
	return "unresolved(" + t.name + ")" + nullableSuffix(t.isNullable)
}

// writeInt appends a decimal representation of n to b. Avoids
// fmt.Sprintf in the String() hot path of VectorType.
func writeInt(b *strings.Builder, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	b.Write(buf[i:])
}
