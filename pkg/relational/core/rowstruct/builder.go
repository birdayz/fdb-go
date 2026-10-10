// Portions derived from FoundationDB Record Layer (
// EmbeddedRelationalStruct.java, EmbeddedRelationalArray.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rowstruct

import (
	"strings"

	"github.com/google/uuid"

	"fdb.dev/pkg/relational/api"
)

// StructBuilder builds an api.Struct attribute by attribute, the value a
// direct-access insert takes: Java's EmbeddedRelationalStruct.newBuilder()
// (EmbeddedRelationalStruct.java:42-121). Attribute names are kept as given; a
// direct-access insert matches them against the table's column names.
type StructBuilder struct {
	fields []api.StructField
	values []any
}

// NewStructBuilder returns an empty builder.
func NewStructBuilder() *StructBuilder { return &StructBuilder{} }

func (b *StructBuilder) add(name string, typ api.DataType, value any) *StructBuilder {
	b.fields = append(b.fields, api.NewStructField(name, typ, len(b.fields)+1))
	b.values = append(b.values, value)
	return b
}

// AddBoolean adds a BOOLEAN attribute.
func (b *StructBuilder) AddBoolean(name string, v bool) *StructBuilder {
	return b.add(name, api.NewBooleanType(false), v)
}

// AddInt adds an INTEGER attribute.
func (b *StructBuilder) AddInt(name string, v int32) *StructBuilder {
	return b.add(name, api.NewIntegerType(false), v)
}

// AddLong adds a BIGINT attribute.
func (b *StructBuilder) AddLong(name string, v int64) *StructBuilder {
	return b.add(name, api.NewLongType(false), v)
}

// AddFloat adds a FLOAT attribute.
func (b *StructBuilder) AddFloat(name string, v float32) *StructBuilder {
	return b.add(name, api.NewFloatType(false), v)
}

// AddDouble adds a DOUBLE attribute.
func (b *StructBuilder) AddDouble(name string, v float64) *StructBuilder {
	return b.add(name, api.NewDoubleType(false), v)
}

// AddString adds a STRING attribute.
func (b *StructBuilder) AddString(name, v string) *StructBuilder {
	return b.add(name, api.NewStringType(false), v)
}

// AddBytes adds a BYTES attribute.
func (b *StructBuilder) AddBytes(name string, v []byte) *StructBuilder {
	return b.add(name, api.NewBytesType(false), v)
}

// AddUUID adds a UUID attribute.
func (b *StructBuilder) AddUUID(name string, v uuid.UUID) *StructBuilder {
	return b.add(name, api.NewUUIDType(false), v)
}

// AddStruct adds a STRUCT attribute.
func (b *StructBuilder) AddStruct(name string, v api.Struct) *StructBuilder {
	return b.add(name, structTypeOf(v), v)
}

// AddArray adds an ARRAY attribute.
func (b *StructBuilder) AddArray(name string, v api.Array) *StructBuilder {
	return b.add(name, api.NewArrayType(v.MetaData().ElementDataType(), false), v)
}

// Build returns the struct. Its type is anonymous, as Java's is
// (ANONYMOUS_STRUCT_<uuid>).
func (b *StructBuilder) Build() api.Struct {
	name := "ANONYMOUS_STRUCT_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	return &builtStruct{
		md:     NewStructMetaData(api.NewStructType(name, b.fields, false)),
		values: append([]any(nil), b.values...),
	}
}

// structTypeOf is a struct value's type, read from its metadata.
func structTypeOf(s api.Struct) api.DataType {
	md := s.MetaData()
	if m, ok := md.(*StructMetaData); ok {
		return m.typ
	}
	fields := make([]api.StructField, 0, md.AttributeCount())
	for i := 1; i <= md.AttributeCount(); i++ {
		name, _ := md.AttributeName(i)
		typ, _ := md.AttributeDataType(i)
		fields = append(fields, api.NewStructField(name, typ, i))
	}
	return api.NewStructType(md.TypeName(), fields, false)
}

// builtStruct is the struct a StructBuilder builds.
type builtStruct struct {
	md     *StructMetaData
	values []any
}

func (s *builtStruct) MetaData() api.StructMetaData { return s.md }

func (s *builtStruct) AttributeCount() int { return len(s.values) }

func (s *builtStruct) Attribute(oneBasedIndex int) (any, error) {
	if oneBasedIndex < 1 || oneBasedIndex > len(s.values) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"attribute index %d out of range for struct %q with %d attributes",
			oneBasedIndex, s.md.TypeName(), len(s.values))
	}
	return s.values[oneBasedIndex-1], nil
}

func (s *builtStruct) AttributeByName(name string) (any, error) {
	for i := 1; i <= len(s.values); i++ {
		if n, _ := s.md.AttributeName(i); equalFoldASCII(n, name) {
			return s.values[i-1], nil
		}
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
		"struct %q has no attribute %q", s.md.TypeName(), name)
}

func (s *builtStruct) Attributes() []any { return append([]any(nil), s.values...) }

// ArrayBuilder builds an api.Array element by element: Java's
// EmbeddedRelationalArray.newBuilder() (EmbeddedRelationalArray.java:50-140).
// Every element has the type of the first.
type ArrayBuilder struct {
	elementType api.DataType
	elements    []any
}

// NewArrayBuilder returns an empty builder.
func NewArrayBuilder() *ArrayBuilder { return &ArrayBuilder{} }

func (b *ArrayBuilder) add(typ api.DataType, value any) *ArrayBuilder {
	if b.elementType == nil {
		b.elementType = typ
	}
	b.elements = append(b.elements, value)
	return b
}

// AddLong adds a BIGINT element.
func (b *ArrayBuilder) AddLong(v int64) *ArrayBuilder { return b.add(api.NewLongType(false), v) }

// AddString adds a STRING element.
func (b *ArrayBuilder) AddString(v string) *ArrayBuilder { return b.add(api.NewStringType(false), v) }

// AddBytes adds a BYTES element.
func (b *ArrayBuilder) AddBytes(v []byte) *ArrayBuilder { return b.add(api.NewBytesType(false), v) }

// AddUUID adds a UUID element.
func (b *ArrayBuilder) AddUUID(v uuid.UUID) *ArrayBuilder { return b.add(api.NewUUIDType(false), v) }

// AddStruct adds a STRUCT element.
func (b *ArrayBuilder) AddStruct(v api.Struct) *ArrayBuilder { return b.add(structTypeOf(v), v) }

// Build returns the array. An empty array's elements are NULL-typed, as
// Java's are.
func (b *ArrayBuilder) Build() api.Array {
	typ := b.elementType
	if typ == nil {
		typ = api.NewNullType()
	}
	return &builtArray{elementType: typ, elements: append([]any(nil), b.elements...)}
}

// builtArray is the array an ArrayBuilder builds.
type builtArray struct {
	elementType api.DataType
	elements    []any
}

func (a *builtArray) MetaData() api.ArrayMetaData { return builtArrayMetaData{a.elementType} }

func (a *builtArray) BaseType() int { return api.JDBCType(a.elementType.Code()) }

func (a *builtArray) BaseTypeName() string { return a.elementType.Code().String() }

func (a *builtArray) Length() int { return len(a.elements) }

func (a *builtArray) Element(oneBasedIndex int) (any, error) {
	if oneBasedIndex < 1 || oneBasedIndex > len(a.elements) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
			"array index %d out of range for an array of %d elements", oneBasedIndex, len(a.elements))
	}
	return a.elements[oneBasedIndex-1], nil
}

func (a *builtArray) Elements() []any { return append([]any(nil), a.elements...) }

type builtArrayMetaData struct{ elementType api.DataType }

func (m builtArrayMetaData) ElementType() int { return api.JDBCType(m.elementType.Code()) }

func (m builtArrayMetaData) ElementTypeName() string { return m.elementType.Code().String() }

func (m builtArrayMetaData) ElementDataType() api.DataType { return m.elementType }

func (m builtArrayMetaData) Nullable() int { return api.ColumnNoNulls }

var (
	_ api.Struct = (*builtStruct)(nil)
	_ api.Array  = (*builtArray)(nil)
)
