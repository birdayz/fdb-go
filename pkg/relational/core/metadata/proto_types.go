package metadata

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// protoFieldToDataType translates a protobuf field descriptor into the
// corresponding api.DataType. Repeated fields are wrapped in an
// ArrayType whose element carries the scalar / message / enum type.
//
// Matches Java's RecordLayerSchemaTemplate field-type resolution. The
// Java side treats every proto field as nullable (so do we) because
// proto2 explicit-presence is the dominant case in FDB Record Layer
// schemas and proto3 fields are tracked via optional modifiers too.
func protoFieldToDataType(fd protoreflect.FieldDescriptor) (api.DataType, error) {
	// Maps are represented as repeated synthesised message types in
	// protoreflect; flag them explicitly since the SQL layer has no
	// native map support yet (Java mirrors this as UnresolvedType).
	if fd.IsMap() {
		return api.NewUnresolvedType("map", true), nil
	}
	inner, err := protoScalarToDataType(fd)
	if err != nil {
		return nil, err
	}
	if fd.Cardinality() == protoreflect.Repeated {
		return api.NewArrayType(inner, true), nil
	}
	return inner, nil
}

// protoScalarToDataType handles the element type only — cardinality is
// applied by the caller (so repeated-of-message etc. works uniformly).
func protoScalarToDataType(fd protoreflect.FieldDescriptor) (api.DataType, error) {
	const nullable = true
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
		return messageTypeFromDescriptor(md, nullable)
	}
	return nil, fmt.Errorf("unsupported proto field kind %v for field %s", fd.Kind(), fd.FullName())
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
			return nil, fmt.Errorf("%s.%s: %w", md.FullName(), fd.Name(), err)
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
