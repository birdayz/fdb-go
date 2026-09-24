package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// validateKeyExpression validates a key expression against a protobuf message descriptor.
// Checks that referenced fields exist, have correct types, and fan types match repeatedness.
// Matches Java's KeyExpression.validate(Descriptor).
func validateKeyExpression(expr KeyExpression, desc protoreflect.MessageDescriptor) error {
	_, err := validateKeyExpressionFields(expr, desc)
	return err
}

// validateKeyExpressionFields returns the validated leaf descriptors in expression
// order, matching Java's KeyExpression.validate. Constants contribute no fields;
// nesting returns child fields, and composite/list expressions concatenate them.
func validateKeyExpressionFields(expr KeyExpression, desc protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	if expr == nil {
		return nil, nil
	}
	var children []KeyExpression
	switch e := expr.(type) {
	case *FieldKeyExpression:
		if err := validateFieldKeyExpression(e, desc, false); err != nil {
			return nil, err
		}
		return []protoreflect.FieldDescriptor{desc.Fields().ByName(protoreflect.Name(e.fieldName))}, nil
	case *CompositeKeyExpression:
		children = e.expressions
	case *NestingKeyExpression:
		return validateNestingKeyExpression(e, desc)
	case *GroupingKeyExpression:
		return validateKeyExpressionFields(e.wholeKey, desc)
	case *EmptyKeyExpression, *RecordTypeKeyExpression, *LiteralKeyExpression, *VersionKeyExpression:
		return nil, nil
	case *FunctionKeyExpression:
		return validateKeyExpressionFields(e.arguments, desc)
	case *DimensionsKeyExpression:
		return validateKeyExpressionFields(e.WholeKey, desc)
	case *KeyWithValueExpression:
		return validateKeyWithValueExpression(e, desc)
	case *SplitKeyExpression:
		return validateSplitKeyExpression(e, desc)
	case *ListKeyExpression:
		children = e.children
	default:
		// Unknown expression type — skip validation (forward-compatible).
		return nil, nil
	}
	var fields []protoreflect.FieldDescriptor
	for _, child := range children {
		childFields, err := validateKeyExpressionFields(child, desc)
		if err != nil {
			return nil, err
		}
		fields = append(fields, childFields...)
	}
	return fields, nil
}

// validateFieldKeyExpression validates a field exists in the descriptor and
// checks FanType consistency with field repeatedness.
// Matches Java's FieldKeyExpression.validate(Descriptor, FieldDescriptor,
// boolean) (FieldKeyExpression.java:145-172), its texts and its classes: the
// KeyExpression.InvalidExpressionException of the first three checks is
// KeyExpressionError, and the scalar check's Query.InvalidExpressionException
// is QueryInvalidExpressionError. A map field is repeated, as protobuf-java's
// isRepeated() says.
func validateFieldKeyExpression(f *FieldKeyExpression, desc protoreflect.MessageDescriptor, allowMessageType bool) error {
	fd := desc.Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return &KeyExpressionError{Message: fmt.Sprintf(
			"Descriptor %s does not have field: %s", desc.Name(), f.fieldName)}
	}

	// protobuf-java's isRepeated(): true for a map field too, whose entries are
	// a repeated message on the wire (FieldKeyExpression.java:152, :158).
	isRepeated := fd.Cardinality() == protoreflect.Repeated
	switch f.fanType {
	case FanTypeFanOut, FanTypeConcatenate:
		if !isRepeated {
			return &KeyExpressionError{Message: fmt.Sprintf(
				"%s is not repeated with FanType.%s", f.fieldName, javaFanTypeName(f.fanType))}
		}
	case FanTypeNone:
		if isRepeated {
			return &KeyExpressionError{Message: fmt.Sprintf(
				"%s is repeated with FanType.None", f.fieldName)}
		}
	}

	// Message fields are only allowed where the caller admits them (a
	// nesting's parent).
	if !allowMessageType && fd.Kind() == protoreflect.MessageKind && !isTupleField(fd) {
		return &QueryInvalidExpressionError{Message: fmt.Sprintf(
			"%s is a nested message, but accessed as a scalar", f.fieldName)}
	}

	return nil
}

// javaFanTypeName is the name of Java's KeyExpression.FanType constant.
func javaFanTypeName(t FanType) string {
	switch t {
	case FanTypeFanOut:
		return "FanOut"
	case FanTypeConcatenate:
		return "Concatenate"
	default:
		return "None"
	}
}

// uuidProtoFullName is the fully-qualified name of TupleFieldsProto.UUID — the
// proto message fdb-relational uses to store SQL UUID column values (it has no
// native proto primitive). Comparing by full name avoids a recordlayer→gen
// import and works across descriptor instances.
const uuidProtoFullName = "com.apple.foundationdb.record.UUID"

// isTupleField reports whether a message-typed field is one of the special
// "tuple field" messages Java's TupleFieldsHelper.isTupleField treats as a
// SCALAR tuple element rather than a nested message (so it's a valid leaf in a
// key/index expression without Nest()). Java's set is UUID + the Nullable*
// wrappers; Go's DDL only emits the UUID wrapper (native proto primitives cover
// the Nullable* cases — FLOAT/INT/STRING/… index directly), so UUID is the only
// one we need to recognize. The runtime extraction lives in scalarToInterface.
func isTupleField(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() == uuidProtoFullName
}

// validateNestingKeyExpression validates the parent field is a message type
// and recursively validates the child expression against the nested descriptor.
// Matches Java's NestingKeyExpression.validate().
func validateNestingKeyExpression(n *NestingKeyExpression, desc protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	// Validate parent field with allowMessageType=true.
	parentFKE := &FieldKeyExpression{fieldName: n.parentField, fanType: n.fanType}
	if err := validateFieldKeyExpression(parentFKE, desc, true); err != nil {
		return nil, err
	}

	// Get the nested message descriptor.
	fd := desc.Fields().ByName(protoreflect.Name(n.parentField))
	if fd == nil {
		// Already checked above, but be safe.
		return nil, &KeyExpressionError{Message: fmt.Sprintf(
			"Descriptor %s does not have field: %s", desc.Name(), n.parentField)}
	}
	if fd.Kind() != protoreflect.MessageKind {
		// Java's parent.getDescriptor calls protobuf-java's getMessageType, which
		// throws UnsupportedOperationException; the text is protobuf-java's,
		// measured on the conformance JVM ("Key validation at build, as Java
		// builds").
		return nil, &UnsupportedOperationError{Message: notMessageTypeText(fd)}
	}

	// Recursively validate child against the nested descriptor.
	fields, err := validateKeyExpressionFields(n.child, fd.Message())
	if err != nil {
		return nil, err
	}
	if fd.IsMap() {
		// Go-only, after every check Java makes, so a key Java refuses is
		// refused with Java's fault. Java fans out a map's entries as the
		// repeated messages they are; Go's evaluator reads a field as repeated
		// only when it is a list, so a map would be read as one message.
		// DIVERGENCES.md, "A proto map field is not fanned out in a key
		// expression".
		return nil, &UnsupportedOperationError{Message: fmt.Sprintf(
			"%s is a map field; Go does not fan out a map in a key expression", n.parentField)}
	}
	return fields, nil
}

// validateKeyWithValueExpression validates column size and inner key.
// Matches Java's KeyWithValueExpression.validate().
func validateKeyWithValueExpression(k *KeyWithValueExpression, desc protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	if k.innerKey.ColumnSize() < k.splitPoint {
		// Java's getMessage; its split_point and child_columns are log info,
		// not part of the text (LoggableException does not render them).
		return nil, &KeyExpressionError{Message: "Child expression of covering expression returns too few columns"}
	}
	return validateKeyExpressionFields(k.innerKey, desc)
}

// validateSplitKeyExpression validates that the joined expression produces exactly 1 column
// and creates duplicates. Matches Java's SplitKeyExpression.validate().
func validateSplitKeyExpression(s *SplitKeyExpression, desc protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	if s.joined.ColumnSize() != 1 {
		return nil, &KeyExpressionError{Message: "Must have a single key before splitting"}
	}
	if !createsDuplicates(s.joined) {
		return nil, &KeyExpressionError{Message: "Must produce multiple values for splitting"}
	}
	return validateKeyExpressionFields(s.joined, desc)
}

// notMessageTypeText is protobuf-java's UnsupportedOperationException text for
// getMessageType on a field that is not a message: the field's full name
// follows the sentence (protobuf-java 4.29.3, measured).
func notMessageTypeText(fd protoreflect.FieldDescriptor) string {
	return fmt.Sprintf("This field is not of message type. (%s)", fd.FullName())
}
