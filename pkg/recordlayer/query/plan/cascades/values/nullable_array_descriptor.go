package values

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Descriptor-level helpers for the NullableArrayWrapper shape — the Go
// counterpart of Java's NullableArrayTypeUtils/NullableArrayUtils structural
// checks. A NULLABLE array column is stored as an OPTIONAL message field
// whose message has exactly one repeated field named "values"
// (`message W { repeated E values = 1; }`); [] and NULL stay distinct
// (wrapper present with empty list vs wrapper absent). Every consumer that
// asks "is this column an array?" of a raw descriptor must see THROUGH the
// wrapper — the wrapper's name is meaningless (Java generates a random one
// per serialization), only the shape identifies it.

// WrappedArrayValuesFieldName is the wrapper's single repeated field name
// (Java: NullableArrayUtils.REPEATED_FIELD_NAME).
const WrappedArrayValuesFieldName = "values"

// IsWrappedArrayDescriptor reports whether md is the NullableArrayWrapper
// shape: exactly one field, named "values", repeated. Mirrors Java's
// NullableArrayUtils.isWrappedArrayDescriptor.
func IsWrappedArrayDescriptor(md protoreflect.MessageDescriptor) bool {
	if md == nil || md.Fields().Len() != 1 {
		return false
	}
	f := md.Fields().Get(0)
	return string(f.Name()) == WrappedArrayValuesFieldName && f.Cardinality() == protoreflect.Repeated
}

// EffectiveListField resolves a column field to its effective REPEATED field:
//   - a flat repeated field returns itself (wrapped=false);
//   - a singular message field whose message is the wrapper returns the
//     inner `values` field (wrapped=true);
//   - anything else returns (nil, false, false).
func EffectiveListField(fd protoreflect.FieldDescriptor) (list protoreflect.FieldDescriptor, wrapped, ok bool) {
	if fd == nil {
		return nil, false, false
	}
	if fd.IsList() {
		return fd, false, true
	}
	if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() && IsWrappedArrayDescriptor(fd.Message()) {
		return fd.Message().Fields().Get(0), true, true
	}
	return nil, false, false
}

// NewWrappedArrayMessage builds an empty wrapper message instance for the
// given wrapper field (a field whose message is the wrapper shape). The
// caller appends elements to the returned list and stores the message value
// on the field.
func NewWrappedArrayMessage(fd protoreflect.FieldDescriptor) (msg *dynamicpb.Message, values protoreflect.List) {
	m := dynamicpb.NewMessage(fd.Message())
	valuesFD := fd.Message().Fields().Get(0)
	return m, m.Mutable(valuesFD).List()
}
