package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FanType controls how repeated (list) proto fields are handled in key expressions.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyExpression.FanType.
type FanType int

const (
	// FanTypeNone means the field must not be repeated. This is the default.
	FanTypeNone FanType = iota
	// FanTypeFanOut produces one tuple per repeated value — used for multi-entry indexes.
	FanTypeFanOut
	// FanTypeConcatenate puts all repeated values into a single tuple element.
	FanTypeConcatenate
)

// FieldKeyExpression extracts a single field value from a record for use as a key component.
// For repeated fields, use FanOut() or Concatenate() to control multi-value handling.
type FieldKeyExpression struct {
	fieldName string
	fanType   FanType
}

// Field creates a key expression that extracts a single (non-repeated) field.
func Field(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeNone}
}

// FanOut creates a key expression for a repeated field that produces one index entry
// per repeated value. Matches Java's Key.Expressions.field("name", FanType.FanOut).
func FanOut(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeFanOut}
}

// Evaluate extracts the field value(s) from the message.
// For non-repeated fields, returns one tuple with the single value.
// For repeated fields with FanOut, returns one tuple per value.
// For repeated fields with Concatenate, returns one tuple containing a nested tuple of all values.
func (f *FieldKeyExpression) Evaluate(msg proto.Message) ([][]interface{}, error) {
	m := msg.ProtoReflect()

	fd := m.Descriptor().Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", f.fieldName)
	}

	if fd.IsList() {
		return f.evaluateRepeated(m, fd)
	}

	// Scalar field — FanType is ignored (matching Java behavior).
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	return [][]interface{}{{result}}, nil
}

// evaluateRepeated handles repeated proto fields according to FanType.
func (f *FieldKeyExpression) evaluateRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]interface{}, error) {
	list := m.Get(fd).List()
	count := list.Len()

	switch f.fanType {
	case FanTypeFanOut:
		if count == 0 {
			return nil, nil // Empty list → no index entries (matches Java)
		}
		result := make([][]interface{}, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			result[i] = []interface{}{val}
		}
		return result, nil

	case FanTypeConcatenate:
		values := make([]interface{}, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			values[i] = val
		}
		// All values packed into a single tuple element (as a nested tuple)
		return [][]interface{}{{values}}, nil

	default: // FanTypeNone
		return nil, fmt.Errorf("field %s is repeated with FanType.None", f.fieldName)
	}
}

// scalarToInterface converts a protoreflect.Value to a Go interface{} suitable for FDB tuples.
// All integer types → int64, all floats → float64 (FDB tuple layer constraint).
func scalarToInterface(fd protoreflect.FieldDescriptor, value protoreflect.Value) (interface{}, error) {
	kind := fd.Kind()
	// For list elements, get the element kind from the field descriptor
	if fd.IsList() {
		kind = fd.Kind()
	}
	switch kind {
	case protoreflect.StringKind:
		return value.String(), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return value.Int(), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(value.Uint()), nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return value.Float(), nil
	case protoreflect.BoolKind:
		return value.Bool(), nil
	case protoreflect.BytesKind:
		return value.Bytes(), nil
	case protoreflect.EnumKind:
		return int64(value.Enum()), nil
	default:
		return nil, fmt.Errorf("unsupported field type %s for key expression", kind)
	}
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

// NestingKeyExpression navigates into a nested protobuf message and evaluates
// a child expression against it. Matches Java's NestingKeyExpression.
//
// Example: Nest("flower", Field("type")) indexes the "type" field inside
// a nested "flower" message.
//
// For repeated message fields, use NestFanOut to fan out over each sub-message.
type NestingKeyExpression struct {
	parentField string
	fanType     FanType
	child       KeyExpression
}

// Nest creates a nesting expression: navigate into a message field and evaluate
// the child expression on the sub-message. Matches Java's field("parent").nest("child").
func Nest(parentField string, child KeyExpression) KeyExpression {
	return &NestingKeyExpression{parentField: parentField, fanType: FanTypeNone, child: child}
}

// NestFanOut creates a nesting expression for a repeated message field.
// Each element of the repeated field is evaluated with the child expression,
// producing fan-out. Matches Java's field("parent", FanType.FanOut).nest(child).
func NestFanOut(parentField string, child KeyExpression) KeyExpression {
	return &NestingKeyExpression{parentField: parentField, fanType: FanTypeFanOut, child: child}
}

// Evaluate navigates into the parent message field and evaluates the child
// expression on the sub-message(s).
func (n *NestingKeyExpression) Evaluate(msg proto.Message) ([][]interface{}, error) {
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(n.parentField))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", n.parentField)
	}
	if fd.Kind() != protoreflect.MessageKind {
		return nil, fmt.Errorf("field %s is not a message type, cannot nest", n.parentField)
	}

	if fd.IsList() {
		return n.evaluateRepeated(m, fd)
	}

	// Scalar message field — get the sub-message and evaluate child on it.
	if !m.Has(fd) {
		// Unset message field → evaluate child on nil (returns null-like results).
		// Match Java: evaluates child on null message → returns null key components.
		return n.child.Evaluate(nil)
	}
	subMsg := m.Get(fd).Message().Interface()
	return n.child.Evaluate(subMsg)
}

// evaluateRepeated handles repeated message fields.
func (n *NestingKeyExpression) evaluateRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]interface{}, error) {
	if n.fanType != FanTypeFanOut {
		return nil, fmt.Errorf("field %s is repeated, must use NestFanOut", n.parentField)
	}

	list := m.Get(fd).List()
	count := list.Len()
	if count == 0 {
		return nil, nil // Empty repeated → no results
	}

	var result [][]interface{}
	for i := 0; i < count; i++ {
		subMsg := list.Get(i).Message().Interface()
		childTuples, err := n.child.Evaluate(subMsg)
		if err != nil {
			return nil, err
		}
		result = append(result, childTuples...)
	}
	return result, nil
}

// FieldNames returns the parent field name plus child field names.
func (n *NestingKeyExpression) FieldNames() []string {
	childNames := n.child.FieldNames()
	result := make([]string, 0, 1+len(childNames))
	result = append(result, n.parentField)
	result = append(result, childNames...)
	return result
}