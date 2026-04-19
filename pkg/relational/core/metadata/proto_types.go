package metadata

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// protoFieldToDataType translates a protobuf field descriptor into the
// corresponding api.DataType. Repeated fields are wrapped in an
// ArrayType whose element carries the scalar / message / enum type.
//
// Mirrors Java's Type.fromProtoType nullability inference:
//
//   - LABEL_REQUIRED (proto2 only) → nullable=false
//   - LABEL_OPTIONAL / LABEL_REPEATED → nullable=true
//
// For repeated fields the nullability applies to the ArrayType wrapper
// (Java: arrays are always nullable); the inner element type is
// derived non-repeated.
func protoFieldToDataType(fd protoreflect.FieldDescriptor) (api.DataType, error) {
	// Maps are represented as repeated synthesised message types in
	// protoreflect; flag them explicitly since the SQL layer has no
	// native map support yet (Java mirrors this as UnresolvedType).
	if fd.IsMap() {
		return api.NewUnresolvedType("map", true), nil
	}
	inner, err := protoScalarToDataTypeWithNullability(fd, isFieldNullable(fd))
	if err != nil {
		return nil, err
	}
	if fd.Cardinality() == protoreflect.Repeated {
		return api.NewArrayType(inner, true), nil
	}
	return inner, nil
}

// isFieldNullable reports whether a proto field's label / cardinality
// says its value is nullable. Only proto2 REQUIRED makes it false;
// OPTIONAL and REPEATED (treated as "nullable element") are true.
// Matches Java's isProtoFieldNullable behaviour.
func isFieldNullable(fd protoreflect.FieldDescriptor) bool {
	return fd.Cardinality() != protoreflect.Required
}

// protoScalarToDataType defaults to nullable=true for callers that
// don't pass an explicit label; used by unwrapWrappedArray which
// extracts an element type from a repeated field (elements are
// conceptually optional).
func protoScalarToDataType(fd protoreflect.FieldDescriptor) (api.DataType, error) {
	return protoScalarToDataTypeWithNullability(fd, true)
}

// protoScalarToDataTypeWithNullability handles the element type only;
// cardinality (repeated → array) is applied by the caller.
func protoScalarToDataTypeWithNullability(fd protoreflect.FieldDescriptor, nullable bool) (api.DataType, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return api.NewBooleanType(nullable), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return api.NewIntegerType(nullable), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return api.NewLongType(nullable), nil
	case protoreflect.FloatKind:
		return api.NewFloatType(nullable), nil
	case protoreflect.DoubleKind:
		return api.NewDoubleType(nullable), nil
	case protoreflect.StringKind:
		return api.NewStringType(nullable), nil
	case protoreflect.BytesKind:
		return api.NewBytesType(nullable), nil
	case protoreflect.EnumKind:
		return enumTypeFromDescriptor(fd.Enum(), nullable), nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		md := fd.Message()
		// Matches Java's fromProtoType() UUID short-circuit: a
		// com.apple.foundationdb.record.UUID message is surfaced as a
		// dedicated UUIDType, not a two-field struct. Without this the
		// SQL layer would see {mostSignificantBits, leastSignificantBits}
		// instead of a single UUID column.
		if isUUIDDescriptor(md) {
			return api.NewUUIDType(nullable), nil
		}
		// Matches Java's NullableArrayTypeUtils.describesWrappedArray:
		// the record-layer serializer encodes a nullable array as a
		// single-field wrapper message. Unwrap it here so the SQL
		// layer sees an ArrayType, not a struct holding an array.
		// The wrapper itself is nullable=true (it's a message), the
		// inner element type is derived from the repeated field.
		if inner, ok := unwrapWrappedArray(md); ok {
			return inner, nil
		}
		return messageTypeFromDescriptor(md, nullable)
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
		"unsupported proto field kind %v for field %s", fd.Kind(), fd.FullName())
}

// messageTypeFromDescriptor turns a proto message descriptor into an
// api.StructType with one StructField per proto field.
func messageTypeFromDescriptor(md protoreflect.MessageDescriptor, nullable bool) (*api.StructType, error) {
	fields := md.Fields()
	structFields := make([]api.StructField, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		dt, err := protoFieldToDataType(fd)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeUnsupportedOperation,
				"%s.%s", md.FullName(), fd.Name())
		}
		structFields = append(structFields, api.NewStructField(string(fd.Name()), dt, i))
	}
	return api.NewStructType(string(md.Name()), structFields, nullable), nil
}

// uuidFullName is the fully-qualified proto message name for the record
// layer's UUID type. Comparing by full name is safe because the Java
// side hard-codes the descriptor equality check too; the name is part
// of the Java-compat wire contract.
const uuidFullName = "com.apple.foundationdb.record.UUID"

// isUUIDDescriptor reports whether md is the record-layer UUID message.
func isUUIDDescriptor(md protoreflect.MessageDescriptor) bool {
	return string(md.FullName()) == uuidFullName
}

// wrappedArrayFieldName is the special field name the record-layer
// serializer uses inside the nullable-array wrapper message. Must match
// Java's NullableArrayTypeUtils.REPEATED_FIELD_NAME byte-for-byte.
const wrappedArrayFieldName = "values"

// unwrapWrappedArray detects the
// "message M { repeated R values = 1; }"
// pattern that the record-layer serializer emits for nullable arrays.
// When it matches, returns an ArrayType whose element is the inner
// field's type (the whole thing nullable=true because the wrapper
// message itself is optional); otherwise (nil, false).
//
// Mirrors Java's NullableArrayTypeUtils.describesWrappedArray plus the
// element-type extraction done inline by fromProtoType.
func unwrapWrappedArray(md protoreflect.MessageDescriptor) (api.DataType, bool) {
	fields := md.Fields()
	if fields.Len() != 1 {
		return nil, false
	}
	fd := fields.Get(0)
	// IsMap guard: protobuf maps are lowered to a synthetic single
	// repeated message field with the map's declared name, so a
	// `map<string,int32> values = 1;` field would otherwise pass all
	// the shape checks. The serializer never wraps map fields this
	// way, but defend anyway — a caller-supplied descriptor could.
	if fd.Cardinality() != protoreflect.Repeated || fd.IsMap() || string(fd.Name()) != wrappedArrayFieldName {
		return nil, false
	}
	// The repeated field drives the element type. Build the scalar
	// type only (not the array), then wrap it in ArrayType so we
	// don't accidentally double-wrap a Repeated → ArrayType.
	elem, err := protoScalarToDataType(fd)
	if err != nil {
		return nil, false
	}
	return api.NewArrayType(elem, true), true
}

// enumTypeFromDescriptor mirrors messageTypeFromDescriptor for enums.
// The resulting api.EnumType carries the declared values so SQL-level
// enum comparisons can be type-checked.
func enumTypeFromDescriptor(ed protoreflect.EnumDescriptor, nullable bool) *api.EnumType {
	values := ed.Values()
	enumValues := make([]api.EnumValue, 0, values.Len())
	for i := 0; i < values.Len(); i++ {
		v := values.Get(i)
		enumValues = append(enumValues, api.NewEnumValue(string(v.Name()), int(v.Number())))
	}
	return api.NewEnumType(string(ed.Name()), enumValues, nullable)
}
