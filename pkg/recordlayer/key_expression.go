package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FieldKeyExpression extracts a single field value from a record for use as a key component
type FieldKeyExpression struct {
	fieldName string
}

// Field creates a key expression that extracts a single field
func Field(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name}
}

// Evaluate extracts the field value from the message
func (f *FieldKeyExpression) Evaluate(msg proto.Message) ([]interface{}, error) {
	// Get the message reflection
	m := msg.ProtoReflect()
	
	// Find the field descriptor
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", f.fieldName)
	}

	// Get the field value
	value := m.Get(fd)

	// Convert to interface{} based on field type
	var result interface{}
	switch fd.Kind() {
	case protoreflect.StringKind:
		result = value.String()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		result = int32(value.Int())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		result = value.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		result = uint32(value.Uint())
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		result = value.Uint()
	case protoreflect.FloatKind:
		result = float32(value.Float())
	case protoreflect.DoubleKind:
		result = value.Float()
	case protoreflect.BoolKind:
		result = value.Bool()
	case protoreflect.BytesKind:
		result = value.Bytes()
	default:
		return nil, fmt.Errorf("unsupported field type %s for key expression", fd.Kind())
	}

	return []interface{}{result}, nil
}

// FieldNames returns the field name accessed by this expression
func (f *FieldKeyExpression) FieldNames() []string {
	return []string{f.fieldName}
}

// TODO: Add support for:
// - Composite keys (multiple fields)
// - Nested field access
// - Key expressions on repeated fields
// These can be added later as needed