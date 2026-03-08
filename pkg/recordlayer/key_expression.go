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

// Evaluate extracts the field value from the message.
// Returns one tuple containing the single field value.
func (f *FieldKeyExpression) Evaluate(msg proto.Message) ([][]interface{}, error) {
	// Get the message reflection
	m := msg.ProtoReflect()

	// Find the field descriptor
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", f.fieldName)
	}

	// Get the field value
	value := m.Get(fd)

	// Convert to interface{} based on field type.
	// All integer types are normalized to int64 because FDB's tuple layer
	// only supports int64 (matching Java's long). Float32 → float64 for the same reason.
	var result interface{}
	switch fd.Kind() {
	case protoreflect.StringKind:
		result = value.String()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		result = value.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		result = int64(value.Uint())
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		result = value.Float()
	case protoreflect.BoolKind:
		result = value.Bool()
	case protoreflect.BytesKind:
		result = value.Bytes()
	default:
		return nil, fmt.Errorf("unsupported field type %s for key expression", fd.Kind())
	}

	return [][]interface{}{{result}}, nil
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

// Evaluate returns the record type name, optionally followed by nested values.
// In Java, RecordTypeKeyExpression.evaluate() returns the record type name as a string.
// We derive it from the proto message's descriptor name.
// When nested is present, computes cross-product: each nested tuple is prefixed with the type name.
func (r *RecordTypeKeyExpression) Evaluate(msg proto.Message) ([][]interface{}, error) {
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	if r.nested == nil {
		return [][]interface{}{{typeName}}, nil
	}

	nestedTuples, err := r.nested.Evaluate(msg)
	if err != nil {
		return nil, err
	}

	result := make([][]interface{}, len(nestedTuples))
	for i, nt := range nestedTuples {
		combined := make([]interface{}, 0, 1+len(nt))
		combined = append(combined, typeName)
		combined = append(combined, nt...)
		result[i] = combined
	}
	return result, nil
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

// EmptyKeyExpression produces an empty tuple — used for ungrouped record counting.
// When used as a recordCountKey, a single total count is maintained.
type EmptyKeyExpression struct{}

// EmptyKey creates a key expression that produces an empty tuple.
func EmptyKey() KeyExpression {
	return &EmptyKeyExpression{}
}

// Evaluate returns one empty tuple (no key components).
func (e *EmptyKeyExpression) Evaluate(_ proto.Message) ([][]interface{}, error) {
	return [][]interface{}{{}}, nil
}

// FieldNames returns no field names.
func (e *EmptyKeyExpression) FieldNames() []string {
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

// Evaluate computes the Cartesian product of all child expression results.
// Matches Java's ThenKeyExpression which crosses children:
//
//	child0 returns [[1], [2]]
//	child1 returns [[a], [b]]
//	result = [[1,a], [1,b], [2,a], [2,b]]
//
// For the common case where each child returns exactly one tuple, the result
// is a single tuple that is the concatenation of all child tuples — identical
// to the old flat-append behavior.
func (c *CompositeKeyExpression) Evaluate(msg proto.Message) ([][]interface{}, error) {
	// Start with a single empty tuple
	result := [][]interface{}{{}}

	for _, expr := range c.expressions {
		childTuples, err := expr.Evaluate(msg)
		if err != nil {
			return nil, err
		}

		// Cross-product: for each existing tuple, combine with each child tuple
		var crossed [][]interface{}
		for _, existing := range result {
			for _, child := range childTuples {
				combined := make([]interface{}, 0, len(existing)+len(child))
				combined = append(combined, existing...)
				combined = append(combined, child...)
				crossed = append(crossed, combined)
			}
		}
		result = crossed
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