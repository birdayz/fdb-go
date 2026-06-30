package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// validateKeyExpression validates a key expression against a protobuf message descriptor.
// Checks that referenced fields exist, have correct types, and fan types match repeatedness.
// Matches Java's KeyExpression.validate(Descriptor).
func validateKeyExpression(expr KeyExpression, desc protoreflect.MessageDescriptor) error {
	return validateKeyExpressionImpl(expr, desc, false)
}

// validateKeyExpressionImpl is the internal recursive implementation.
// allowMessageType is true when called from NestingKeyExpression for the parent field.
func validateKeyExpressionImpl(expr KeyExpression, desc protoreflect.MessageDescriptor, allowMessageType bool) error {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *FieldKeyExpression:
		return validateFieldKeyExpression(e, desc, allowMessageType)
	case *CompositeKeyExpression:
		for _, child := range e.expressions {
			if err := validateKeyExpression(child, desc); err != nil {
				return err
			}
		}
		return nil
	case *NestingKeyExpression:
		return validateNestingKeyExpression(e, desc)
	case *GroupingKeyExpression:
		return validateKeyExpression(e.wholeKey, desc)
	case *EmptyKeyExpression:
		return nil
	case *RecordTypeKeyExpression:
		return nil
	case *LiteralKeyExpression:
		return nil
	case *VersionKeyExpression:
		return nil
	case *FunctionKeyExpression:
		return validateKeyExpression(e.arguments, desc)
	case *DimensionsKeyExpression:
		return validateKeyExpression(e.WholeKey, desc)
	case *KeyWithValueExpression:
		return validateKeyWithValueExpression(e, desc)
	case *SplitKeyExpression:
		return validateSplitKeyExpression(e, desc)
	case *ListKeyExpression:
		for _, child := range e.children {
			if err := validateKeyExpression(child, desc); err != nil {
				return err
			}
		}
		return nil
	default:
		// Unknown expression type — skip validation (forward-compatible).
		return nil
	}
}

// validateFieldKeyExpression validates a field exists in the descriptor and
// checks FanType consistency with field repeatedness.
// Matches Java's FieldKeyExpression.validate(Descriptor, boolean).
func validateFieldKeyExpression(f *FieldKeyExpression, desc protoreflect.MessageDescriptor, allowMessageType bool) error {
	fd := desc.Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return &KeyExpressionError{Message: fmt.Sprintf(
			"field %q not found in message %q", f.fieldName, desc.FullName())}
	}

	// Check FanType vs repeatedness.
	// Matches Java's FieldKeyExpression.validate() which checks:
	//   FanOut/Concatenate → field must be repeated
	//   None → field must NOT be repeated
	isRepeated := fd.IsList()
	switch f.fanType {
	case FanTypeFanOut, FanTypeConcatenate:
		if !isRepeated {
			return &KeyExpressionError{Message: fmt.Sprintf(
				"field %q in %q is not repeated, but fan type requires a repeated field",
				f.fieldName, desc.FullName())}
		}
	case FanTypeNone:
		if isRepeated {
			return &KeyExpressionError{Message: fmt.Sprintf(
				"field %q in %q is repeated; use FanOut() or Concatenate() for repeated fields",
				f.fieldName, desc.FullName())}
		}
	}

	// Check field type — message fields are only allowed via NestingKeyExpression.
	// Matches Java's FieldKeyExpression.validate() which checks !allowMessageType → must be scalar.
	if !allowMessageType && fd.Kind() == protoreflect.MessageKind && !isTupleField(fd) {
		return &KeyExpressionError{Message: fmt.Sprintf(
			"field %q in %q is a message type; use Nest() to navigate into nested messages",
			f.fieldName, desc.FullName())}
	}

	return nil
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
func validateNestingKeyExpression(n *NestingKeyExpression, desc protoreflect.MessageDescriptor) error {
	// Validate parent field with allowMessageType=true.
	parentFKE := &FieldKeyExpression{fieldName: n.parentField, fanType: n.fanType}
	if err := validateFieldKeyExpression(parentFKE, desc, true); err != nil {
		return err
	}

	// Get the nested message descriptor.
	fd := desc.Fields().ByName(protoreflect.Name(n.parentField))
	if fd == nil {
		// Already checked above, but be safe.
		return &KeyExpressionError{Message: fmt.Sprintf(
			"field %q not found in message %q", n.parentField, desc.FullName())}
	}
	if fd.Kind() != protoreflect.MessageKind {
		return &KeyExpressionError{Message: fmt.Sprintf(
			"field %q in %q is not a message type; cannot nest into non-message fields",
			n.parentField, desc.FullName())}
	}

	// Recursively validate child against the nested descriptor.
	return validateKeyExpression(n.child, fd.Message())
}

// validateKeyWithValueExpression validates column size and inner key.
// Matches Java's KeyWithValueExpression.validate().
func validateKeyWithValueExpression(k *KeyWithValueExpression, desc protoreflect.MessageDescriptor) error {
	if k.innerKey.ColumnSize() < k.splitPoint {
		return &KeyExpressionError{Message: fmt.Sprintf(
			"child expression of covering expression returns too few columns: split_point=%d, child_columns=%d",
			k.splitPoint, k.innerKey.ColumnSize())}
	}
	return validateKeyExpression(k.innerKey, desc)
}

// validateSplitKeyExpression validates that the joined expression produces exactly 1 column
// and creates duplicates. Matches Java's SplitKeyExpression.validate().
func validateSplitKeyExpression(s *SplitKeyExpression, desc protoreflect.MessageDescriptor) error {
	if s.joined.ColumnSize() != 1 {
		return &KeyExpressionError{Message: "must have a single key before splitting"}
	}
	if !createsDuplicates(s.joined) {
		return &KeyExpressionError{Message: "must produce multiple values for splitting"}
	}
	return validateKeyExpression(s.joined, desc)
}
