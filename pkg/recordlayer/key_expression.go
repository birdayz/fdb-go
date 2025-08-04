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

// RecordTypeKeyExpression represents the special record type key prefix
type RecordTypeKeyExpression struct {
	// nested is the optional nested key expression
	nested KeyExpression
}

// RecordTypeKey creates a key expression that prefixes with the record type
func RecordTypeKey() *RecordTypeKeyExpression {
	return &RecordTypeKeyExpression{}
}

// Nest adds a nested key expression after the record type prefix
func (r *RecordTypeKeyExpression) Nest(expr KeyExpression) KeyExpression {
	r.nested = expr
	return r
}

// Evaluate returns the record type index, optionally followed by nested values
func (r *RecordTypeKeyExpression) Evaluate(msg proto.Message) ([]interface{}, error) {
	// For RecordTypeKeyExpression, we can't evaluate without knowing the record type
	// This will be handled specially in SaveRecord/LoadRecord
	return nil, fmt.Errorf("RecordTypeKeyExpression requires record type context")
}

// FieldNames returns the field names accessed by nested expression
func (r *RecordTypeKeyExpression) FieldNames() []string {
	if r.nested != nil {
		return r.nested.FieldNames()
	}
	return []string{}
}

// IsRecordTypeExpression checks if a key expression starts with record type
func IsRecordTypeExpression(expr KeyExpression) bool {
	_, ok := expr.(*RecordTypeKeyExpression)
	return ok
}

// GetNestedExpression returns the nested expression of a RecordTypeKeyExpression
func GetNestedExpression(expr KeyExpression) KeyExpression {
	if rt, ok := expr.(*RecordTypeKeyExpression); ok {
		return rt.nested
	}
	return nil
}

// CompositeKeyExpression combines multiple key expressions
type CompositeKeyExpression struct {
	expressions []KeyExpression
}

// Concat creates a composite key from multiple expressions
func Concat(exprs ...KeyExpression) KeyExpression {
	return &CompositeKeyExpression{expressions: exprs}
}

// Evaluate returns values from all component expressions
func (c *CompositeKeyExpression) Evaluate(msg proto.Message) ([]interface{}, error) {
	var result []interface{}
	for _, expr := range c.expressions {
		values, err := expr.Evaluate(msg)
		if err != nil {
			return nil, err
		}
		result = append(result, values...)
	}
	return result, nil
}

// FieldNames returns all field names from component expressions
func (c *CompositeKeyExpression) FieldNames() []string {
	var names []string
	for _, expr := range c.expressions {
		names = append(names, expr.FieldNames()...)
	}
	return names
}